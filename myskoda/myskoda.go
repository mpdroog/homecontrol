// Package myskoda reads the battery level of a Škoda and can ask the car to
// start charging, via the (unofficial) MySkoda app API.
package myskoda

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mpdroog/homecontrol/internal/httpx"
)

const (
	clientID     = "7f045eee-7003-4379-9968-9355ed2adb06@apps_vw-dilab_com"
	redirectURI  = "myskoda://redirect/login/"
	apiBase      = "https://mysmob.api.connect.skoda-auto.cz"
	identityBase = "https://identity.vwgroup.io"
	scopes       = "address badge birthdate cars driversLicense dealers email mileage mbb " +
		"nationalIdentifier openid phone profession profile vin"
	userAgent    = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"
	timeout      = 30 * time.Second
	tokenLife    = 55 * time.Minute
	maxRedirects = 20
	pkceBytes    = 32
	nonceBytes   = 16
)

var (
	// ErrLogin is returned when the login flow does not end with an auth code.
	ErrLogin = errors.New("myskoda login failed")
	// ErrNoCSRF is returned when the login page has no CSRF token.
	ErrNoCSRF = errors.New("csrf token not found")
	// ErrNoStatus is returned when the car reports no charging status.
	ErrNoStatus = errors.New("no charging status")

	reCSRF     = regexp.MustCompile(`csrf_token:\s*['"]([^'"]+)['"]`)
	reCSRFForm = regexp.MustCompile(`name="_csrf"[^>]*value="([^"]+)"`)
	reHMAC     = regexp.MustCompile(`"hmac"\s*:\s*"([^"]+)"`)
	reRelay    = regexp.MustCompile(`"relayState"\s*:\s*"([^"]+)"`)
)

// Client is a logged-in MySkoda session. Safe for concurrent use.
type Client struct {
	expiry  time.Time
	http    *http.Client
	log     *slog.Logger
	user    string
	pass    string
	vin     string
	access  string
	refresh string
	mu      sync.Mutex
}

// New creates a client for the car with the given VIN; it logs in lazily on
// the first request.
func New(user, pass, vin string, log *slog.Logger) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("myskoda: cookie jar: %w", err)
	}

	return &Client{
		http: &http.Client{ //nolint:exhaustruct // stdlib defaults are fine
			Jar:     jar,
			Timeout: timeout,
			// Redirects are followed by hand: the last one goes to myskoda://.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log:     log,
		user:    user,
		pass:    pass,
		vin:     vin,
		access:  "",
		refresh: "",
		expiry:  time.Time{},
		mu:      sync.Mutex{},
	}, nil
}

// BatteryPercent returns the state of charge of the car.
func (c *Client) BatteryPercent(ctx context.Context) (int, error) {
	body, err := c.api(ctx, http.MethodGet, "/api/v1/charging/"+url.PathEscape(c.vin), nil)
	if err != nil {
		return 0, fmt.Errorf("myskoda charging status: %w", err)
	}

	var data chargingResponse

	err = json.Unmarshal(body, &data)
	if err != nil {
		return 0, fmt.Errorf("myskoda charging status: decoding: %w", err)
	}

	if data.Status == nil {
		return 0, fmt.Errorf("myskoda charging status: %w", ErrNoStatus)
	}

	return data.Status.Battery.Percent, nil
}

type chargingResponse struct {
	Status *chargingStatus `json:"status"`
}

type chargingStatus struct {
	Battery batteryStatus `json:"battery"`
}

type batteryStatus struct {
	Percent int `json:"stateOfChargeInPercent"`
}

// StartCharging asks the car to start charging (wakes it if it went to sleep).
func (c *Client) StartCharging(ctx context.Context) error {
	_, err := c.api(ctx, http.MethodPost, "/api/v1/charging/"+url.PathEscape(c.vin)+"/start", nil)
	if err != nil {
		return fmt.Errorf("myskoda start charging: %w", err)
	}

	return nil
}

// api performs an authenticated API call, logging in or refreshing as needed.
// A 401 triggers one fresh login and a retry.
func (c *Client) api(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}

	body, err := c.apiOnce(ctx, method, path, payload)

	var se *httpx.StatusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		c.log.WarnContext(ctx, "myskoda: token rejected, logging in again", "err", err)

		err = c.login(ctx)
		if err != nil {
			return nil, err
		}

		return c.apiOnce(ctx, method, path, payload)
	}

	return body, err
}

func (c *Client) apiOnce(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.access)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpx.Do(c.http, req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}

	return resp.Body, nil
}

// ensureToken makes sure we hold a valid access token. Caller holds c.mu.
func (c *Client) ensureToken(ctx context.Context) error {
	if c.access != "" && time.Now().Before(c.expiry) {
		return nil
	}

	if c.refresh != "" {
		err := c.refreshToken(ctx)
		if err == nil {
			return nil
		}

		c.log.WarnContext(ctx, "myskoda: token refresh failed, logging in again", "err", err)
	}

	return c.login(ctx)
}

type tokens struct {
	Access  string `json:"accessToken"`
	Refresh string `json:"refreshToken"`
}

func (c *Client) refreshToken(ctx context.Context) error {
	payload, err := json.Marshal(map[string]string{"refreshToken": c.refresh})
	if err != nil {
		return fmt.Errorf("refresh: encoding: %w", err)
	}

	return c.tokenCall(ctx, "/api/v1/authentication/refresh-token?tokenType=CONNECT", payload)
}

func (c *Client) tokenCall(ctx context.Context, path string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("token: building request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := httpx.Do(c.http, req)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}

	var tok tokens

	err = json.Unmarshal(resp.Body, &tok)
	if err != nil {
		return fmt.Errorf("token: decoding: %w", err)
	}

	if tok.Access == "" {
		return fmt.Errorf("token: %w: no access token", ErrLogin)
	}

	c.access, c.refresh, c.expiry = tok.Access, tok.Refresh, time.Now().Add(tokenLife)

	return nil
}

// login runs the VW identity OAuth2 PKCE flow. Caller holds c.mu.
func (c *Client) login(ctx context.Context) error {
	verifier, err := randomString(pkceBytes)
	if err != nil {
		return fmt.Errorf("login: pkce: %w", err)
	}

	code, err := c.authorize(ctx, verifier)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	payload, err := json.Marshal(map[string]string{"code": code, "redirectUri": redirectURI, "verifier": verifier})
	if err != nil {
		return fmt.Errorf("login: encoding exchange: %w", err)
	}

	err = c.tokenCall(ctx, "/api/v1/authentication/exchange-authorization-code?tokenType=CONNECT", payload)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	c.log.InfoContext(ctx, "myskoda: logged in")

	return nil
}

// authorize walks the login pages (e-mail, then password) and returns the
// authorization code from the final myskoda:// redirect.
func (c *Client) authorize(ctx context.Context, verifier string) (string, error) {
	nonce, err := randomString(nonceBytes)
	if err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}

	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id":             {clientID},
		"nonce":                 {nonce},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {scopes},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"login"},
	}

	res, err := c.follow(ctx, http.MethodGet, identityBase+"/oidc/v1/authorize?"+query.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("authorize: %w", err)
	}

	form, err := parseForm(res.page, loginForm{csrf: "", hmac: "", relay: ""})
	if err != nil {
		return "", fmt.Errorf("e-mail page: %w", err)
	}

	signin := identityBase + "/signin-service/v1/" + url.PathEscape(clientID) + "/login/"

	res, err = c.follow(ctx, http.MethodPost, signin+"identifier", url.Values{
		"relayState": {form.relay}, "email": {c.user}, "hmac": {form.hmac}, "_csrf": {form.csrf},
	})
	if err != nil {
		return "", fmt.Errorf("submitting e-mail: %w", err)
	}

	form, err = parseForm(res.page, form)
	if err != nil {
		return "", fmt.Errorf("password page: %w", err)
	}

	res, err = c.follow(ctx, http.MethodPost, signin+"authenticate", url.Values{
		"relayState": {form.relay}, "email": {c.user}, "password": {c.pass}, "hmac": {form.hmac}, "_csrf": {form.csrf},
	})
	if err != nil {
		return "", fmt.Errorf("submitting password: %w", err)
	}

	return codeFrom(res.next, res.page)
}

// codeFrom extracts the authorization code from the myskoda:// redirect.
func codeFrom(final string, page []byte) (string, error) {
	if final == "" {
		return "", fmt.Errorf("%w: no redirect to app after password (wrong password? locked account?): %.300s", ErrLogin, page)
	}

	redirect, err := url.Parse(final)
	if err != nil {
		return "", fmt.Errorf("parsing redirect: %w", err)
	}

	code := redirect.Query().Get("code")
	if code == "" {
		return "", fmt.Errorf("%w: no code in redirect", ErrLogin)
	}

	return code, nil
}

// response is the outcome of a request in the login flow: a page body, or
// (for a redirect) the next URL.
type response struct {
	next string // absolute redirect target or myskoda:// app URL; "" for a page
	page []byte
}

// follow sends a request and follows redirects by hand. It stops at the first
// normal page or at the myskoda:// app redirect (returned in next).
func (c *Client) follow(ctx context.Context, method, target string, form url.Values) (response, error) {
	for range maxRedirects {
		res, err := c.hop(ctx, method, target, form)
		if err != nil {
			return response{}, err
		}

		if res.next == "" || strings.HasPrefix(res.next, "myskoda://") {
			return res, nil
		}

		method, target, form = http.MethodGet, res.next, nil
	}

	return response{}, fmt.Errorf("%w: more than %d redirects", ErrLogin, maxRedirects)
}

// hop performs one request without following redirects.
func (c *Client) hop(ctx context.Context, method, target string, form url.Values) (response, error) {
	var body io.Reader = http.NoBody
	if form != nil {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return response{}, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req) //nolint:bodyclose // closed by httpx.ReadAndClose below
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w", method, req.URL.Redacted(), err)
	}

	page, err := httpx.ReadAndClose(resp)
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w", method, req.URL.Redacted(), err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return response{}, &httpx.StatusError{Method: method, URL: req.URL.Redacted(), Code: resp.StatusCode, Body: string(page)}
	}

	next, err := redirectTarget(req, resp)
	if err != nil || next == "" {
		return response{page: page, next: ""}, err
	}

	return response{page: nil, next: next}, nil
}

// redirectTarget returns where a 3xx response points to (absolute URL, or the
// raw myskoda:// app URL), or "" when resp is not a redirect.
func redirectTarget(req *http.Request, resp *http.Response) (string, error) {
	loc := resp.Header.Get("Location")
	if resp.StatusCode < http.StatusMultipleChoices || loc == "" {
		return "", nil
	}

	if strings.HasPrefix(loc, "myskoda://") {
		return loc, nil
	}

	next, err := req.URL.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("parsing redirect %q: %w", loc, err)
	}

	return next.String(), nil
}

type loginForm struct {
	csrf  string
	hmac  string
	relay string
}

// parseForm extracts the hidden login-form values; hmac and relayState fall
// back to the previous page's values when the new page lacks them.
func parseForm(page []byte, prev loginForm) (loginForm, error) {
	out := loginForm{csrf: match(reCSRF, page), hmac: match(reHMAC, page), relay: match(reRelay, page)}
	if out.csrf == "" {
		out.csrf = match(reCSRFForm, page)
	}

	if out.csrf == "" {
		return loginForm{}, ErrNoCSRF
	}

	if out.hmac == "" {
		out.hmac = prev.hmac
	}

	if out.relay == "" {
		out.relay = prev.relay
	}

	return out, nil
}

func match(re *regexp.Regexp, page []byte) string {
	m := re.FindSubmatch(page)
	if m == nil {
		return ""
	}

	return string(m[1])
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)

	_, err := rand.Read(buf)
	if err != nil {
		return "", fmt.Errorf("random: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}
