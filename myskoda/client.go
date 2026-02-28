// Package myskoda provides a Go client for the MySkoda API to read EV battery status.
package myskoda

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	clientID     = "7f045eee-7003-4379-9968-9355ed2adb06@apps_vw-dilab_com"
	redirectURI  = "myskoda://redirect/login/"
	baseURLSkoda = "https://mysmob.api.connect.skoda-auto.cz"
	baseURLIdent = "https://identity.vwgroup.io"
	scopes       = "address badge birthdate cars driversLicense dealers email mileage mbb nationalIdentifier openid phone profession profile vin"
)

// Client is the MySkoda API client.
type Client struct {
	httpClient   *http.Client
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time
	username     string
	password     string
}

// Battery contains the battery status.
type Battery struct {
	StateOfChargePercent int `json:"stateOfChargeInPercent"`
	RemainingRangeMeters int `json:"remainingCruisingRangeInMeters"`
}

// ChargingStatus contains charging information.
type ChargingStatus struct {
	Battery                        Battery `json:"battery"`
	State                          string  `json:"state"`
	ChargePowerKW                  float64 `json:"chargePowerInKw"`
	ChargingRateKmPerHour          float64 `json:"chargingRateInKilometersPerHour"`
	ChargeType                     string  `json:"chargeType"`
	RemainingMinutesToFullyCharged int     `json:"remainingTimeToFullyChargedInMinutes"`
}

// Charging is the response from the charging endpoint.
type Charging struct {
	IsVehicleInSavedLocation bool            `json:"isVehicleInSavedLocation"`
	Status                   *ChargingStatus `json:"status"`
}

// Vehicle represents a vehicle in the garage.
type Vehicle struct {
	VIN          string `json:"vin"`
	Name         string `json:"name"`
	LicensePlate string `json:"licensePlate"`
}

// GarageResponse is the response from the garage endpoint.
type GarageResponse struct {
	Vehicles []Vehicle `json:"vehicles"`
}

// csrfState holds the CSRF tokens extracted from the login page.
type csrfState struct {
	CSRFToken  string
	HMAC       string
	RelayState string
}

// NewClient creates a new MySkoda client.
func NewClient(username, password string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar: %w", err)
	}

	return &Client{
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Stop following redirects when we hit the myskoda:// URL
				if strings.HasPrefix(req.URL.String(), "myskoda://") {
					return http.ErrUseLastResponse
				}
				// Also stop on 3xx to handle manually
				return http.ErrUseLastResponse
			},
		},
		username: username,
		password: password,
	}, nil
}

// generatePKCE generates a PKCE code verifier and challenge.
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)

	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// generateNonce generates a random nonce.
func generateNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// extractCSRFState extracts CSRF tokens from the page.
// Mimics the Python CSRFParser which parses window._IDK JavaScript object
func extractCSRFState(body []byte) (*csrfState, error) {
	state := &csrfState{}
	content := string(body)

	// Extract csrf_token - it appears as: csrf_token: 'value',
	csrfRegex := regexp.MustCompile(`csrf_token:\s*['"]([^'"]+)['"]`)
	if m := csrfRegex.FindStringSubmatch(content); m != nil {
		state.CSRFToken = m[1]
	}

	// Extract hmac from templateModel JSON - appears as: "hmac":"value"
	hmacRegex := regexp.MustCompile(`"hmac"\s*:\s*"([^"]+)"`)
	if m := hmacRegex.FindStringSubmatch(content); m != nil {
		state.HMAC = m[1]
	}

	// Extract relayState from templateModel JSON - appears as: "relayState":"value"
	relayRegex := regexp.MustCompile(`"relayState"\s*:\s*"([^"]+)"`)
	if m := relayRegex.FindStringSubmatch(content); m != nil {
		state.RelayState = m[1]
	}

	// Fallback: Hidden form fields
	if state.CSRFToken == "" {
		formCsrfRegex := regexp.MustCompile(`name="_csrf"[^>]*value="([^"]+)"`)
		if m := formCsrfRegex.FindStringSubmatch(content); m != nil {
			state.CSRFToken = m[1]
		}
	}

	if state.CSRFToken == "" {
		return nil, fmt.Errorf("csrf_token not found in page")
	}

	return state, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) == 0 {
		return "(empty)"
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// createMultipartForm creates a multipart form with the given fields.
func createMultipartForm(fields map[string]string) (body *bytes.Buffer, contentType string, err error) {
	body = &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return nil, "", err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, "", err
	}

	return body, writer.FormDataContentType(), nil
}

// followRedirects follows HTTP redirects until a non-redirect or target URL.
func (c *Client) followRedirects(resp *http.Response) (*http.Response, string, error) {
	for {
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp, "", nil
		}

		loc := resp.Header.Get("Location")
		if loc == "" {
			return resp, "", nil
		}

		// Check if we've reached the final redirect
		if strings.HasPrefix(loc, "myskoda://") {
			return resp, loc, nil
		}

		// Handle relative URLs
		if !strings.HasPrefix(loc, "http") {
			baseURL := fmt.Sprintf("%s://%s", resp.Request.URL.Scheme, resp.Request.URL.Host)
			if strings.HasPrefix(loc, "/") {
				loc = baseURL + loc
			} else {
				loc = baseURL + "/" + loc
			}
		}

		resp.Body.Close()

		var err error
		resp, err = c.httpClient.Get(loc)
		if err != nil {
			return nil, "", fmt.Errorf("following redirect to %s: %w", loc, err)
		}
	}
}

// Login authenticates with the MySkoda API using OAuth2 PKCE flow.
func (c *Client) Login() error {
	return c.LoginWithDebug(false)
}

// LoginWithDebug authenticates with optional debug output.
func (c *Client) LoginWithDebug(debug bool) error {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return fmt.Errorf("generating PKCE: %w", err)
	}

	nonce, err := generateNonce()
	if err != nil {
		return fmt.Errorf("generating nonce: %w", err)
	}

	if debug {
		fmt.Println("[DEBUG] Step 1: Initial OIDC authorize")
	}

	// Step 1: Initial OIDC authorize - this redirects to the login page
	authURL := fmt.Sprintf("%s/oidc/v1/authorize?"+
		"client_id=%s&"+
		"nonce=%s&"+
		"redirect_uri=%s&"+
		"response_type=code&"+
		"scope=%s&"+
		"code_challenge=%s&"+
		"code_challenge_method=S256&"+
		"prompt=login",
		baseURLIdent,
		url.QueryEscape(clientID),
		url.QueryEscape(nonce),
		url.QueryEscape(redirectURI),
		url.QueryEscape(scopes),
		url.QueryEscape(challenge),
	)

	resp, err := c.httpClient.Get(authURL)
	if err != nil {
		return fmt.Errorf("initial authorize: %w", err)
	}

	// Follow redirects to get to the actual login page
	resp, _, err = c.followRedirects(resp)
	if err != nil {
		return fmt.Errorf("following initial redirects: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading authorize response: %w", err)
	}

	// Extract CSRF state from window._IDK
	csrf, err := extractCSRFState(body)
	if err != nil {
		return fmt.Errorf("extracting CSRF state: %w", err)
	}

	if debug {
		fmt.Printf("[DEBUG] CSRF extracted - token: %s, hmac: %s, relay: %s\n",
			truncate(csrf.CSRFToken, 20),
			truncate(csrf.HMAC, 20),
			truncate(csrf.RelayState, 20))
		fmt.Println("[DEBUG] Step 2: Submit email")
	}

	// Step 2: Submit email to the identifier endpoint
	emailURL := fmt.Sprintf("%s/signin-service/v1/%s/login/identifier",
		baseURLIdent, url.PathEscape(clientID))

	formData := url.Values{
		"relayState": {csrf.RelayState},
		"email":      {c.username},
		"hmac":       {csrf.HMAC},
		"_csrf":      {csrf.CSRFToken},
	}

	req, err := http.NewRequest("POST", emailURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("creating email request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("submitting email: %w", err)
	}

	// Follow any redirects
	resp, _, err = c.followRedirects(resp)
	if err != nil {
		return fmt.Errorf("following email redirects: %w", err)
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading email response: %w", err)
	}

	if debug {
		fmt.Printf("[DEBUG] Email response status: %d\n", resp.StatusCode)
		// Check what page type we got (identitykit meta tag)
		metaRegex := regexp.MustCompile(`name="identitykit"\s+content="([^"]+)"`)
		if m := metaRegex.FindSubmatch(body); m != nil {
			fmt.Printf("[DEBUG] Email response page type: %s\n", string(m[1]))
		}
		// Show templateModel to see hmac/relayState
		tmRegex := regexp.MustCompile(`"hmac"\s*:\s*("[^"]*"|null)`)
		if m := tmRegex.FindSubmatch(body); m != nil {
			fmt.Printf("[DEBUG] Email response hmac: %s\n", string(m[1]))
		}
		relayRegex := regexp.MustCompile(`"relayState"\s*:\s*"([^"]*)"`)
		if m := relayRegex.FindSubmatch(body); m != nil {
			fmt.Printf("[DEBUG] Email response relayState: %s...\n", truncate(string(m[1]), 30))
		}
	}

	// Extract new CSRF state for password submission
	// Save old hmac/relayState in case they're not in the new page
	oldHMAC := csrf.HMAC
	oldRelayState := csrf.RelayState

	csrf, err = extractCSRFState(body)
	if err != nil {
		return fmt.Errorf("extracting CSRF state after email: %w", err)
	}

	// Preserve hmac and relayState from step 1 if not found in step 2
	if csrf.HMAC == "" {
		csrf.HMAC = oldHMAC
	}
	if csrf.RelayState == "" {
		csrf.RelayState = oldRelayState
	}

	if debug {
		fmt.Printf("[DEBUG] New CSRF - token: %s, hmac: %s, relay: %s\n",
			truncate(csrf.CSRFToken, 20),
			truncate(csrf.HMAC, 20),
			truncate(csrf.RelayState, 20))
		fmt.Println("[DEBUG] Step 3: Submit password")
	}

	// Step 3: Submit password to the authenticate endpoint
	passwordURL := fmt.Sprintf("%s/signin-service/v1/%s/login/authenticate",
		baseURLIdent, url.PathEscape(clientID))

	formData = url.Values{
		"relayState": {csrf.RelayState},
		"email":      {c.username},
		"password":   {c.password},
		"hmac":       {csrf.HMAC},
		"_csrf":      {csrf.CSRFToken},
	}

	req, err = http.NewRequest("POST", passwordURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return fmt.Errorf("creating password request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("submitting password: %w", err)
	}
	defer resp.Body.Close()

	if debug {
		fmt.Printf("[DEBUG] Password response status: %d\n", resp.StatusCode)
		fmt.Printf("[DEBUG] Password response location: %s\n", resp.Header.Get("Location"))
	}

	// Follow redirects until we get myskoda:// URL
	var finalURL string
	for {
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			break
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}
		if debug {
			fmt.Printf("[DEBUG] Following redirect to: %s\n", truncate(loc, 80))
		}
		if strings.HasPrefix(loc, "myskoda://") {
			finalURL = loc
			break
		}
		resp.Body.Close()
		resp, err = c.httpClient.Get(loc)
		if err != nil {
			return fmt.Errorf("following redirect: %w", err)
		}
	}
	defer resp.Body.Close()
	if err != nil {
		return fmt.Errorf("following password redirects: %w", err)
	}

	if finalURL == "" {
		// Read response body for debugging
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		// Check for specific error patterns
		if strings.Contains(bodyStr, "invalid password") || strings.Contains(bodyStr, "incorrect password") ||
			strings.Contains(bodyStr, "wrong password") || strings.Contains(bodyStr, "invalidCredentials") {
			return fmt.Errorf("authentication failed - invalid password")
		}
		if strings.Contains(bodyStr, "account locked") || strings.Contains(bodyStr, "too many attempts") {
			return fmt.Errorf("authentication failed - account may be locked")
		}

		return fmt.Errorf("did not receive redirect to myskoda://, status: %d\nFull response:\n%s", resp.StatusCode, bodyStr)
	}

	// Parse the auth code from the redirect URL
	u, err := url.Parse(finalURL)
	if err != nil {
		return fmt.Errorf("parsing redirect URL: %w", err)
	}
	authCode := u.Query().Get("code")
	if authCode == "" {
		return fmt.Errorf("authorization code not found in redirect URL")
	}

	// Step 4: Exchange authorization code for tokens
	tokenURL := fmt.Sprintf("%s/api/v1/authentication/exchange-authorization-code?tokenType=CONNECT", baseURLSkoda)
	tokenReq := map[string]string{
		"code":         authCode,
		"redirectUri":  redirectURI,
		"verifier": verifier,
	}
	tokenBody, _ := json.Marshal(tokenReq)

	req, err = http.NewRequest("POST", tokenURL, bytes.NewReader(tokenBody))
	if err != nil {
		return fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("exchanging code for tokens: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		IDToken      string `json:"idToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decoding token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("no access token in response")
	}

	c.accessToken = tokenResp.AccessToken
	c.refreshToken = tokenResp.RefreshToken
	c.tokenExpiry = time.Now().Add(55 * time.Minute)

	return nil
}

// RefreshAccessToken refreshes the access token using the refresh token.
func (c *Client) RefreshAccessToken() error {
	tokenURL := fmt.Sprintf("%s/api/v1/authentication/refresh-token?tokenType=CONNECT", baseURLSkoda)
	tokenReq := map[string]string{
		"refreshToken": c.refreshToken,
	}
	tokenBody, _ := json.Marshal(tokenReq)

	req, err := http.NewRequest("POST", tokenURL, bytes.NewReader(tokenBody))
	if err != nil {
		return fmt.Errorf("creating refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refreshing token: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		IDToken      string `json:"idToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decoding refresh response: %w", err)
	}

	c.accessToken = tokenResp.AccessToken
	c.refreshToken = tokenResp.RefreshToken
	c.tokenExpiry = time.Now().Add(55 * time.Minute)

	return nil
}

// ensureValidToken checks if the token is still valid and refreshes if needed.
func (c *Client) ensureValidToken() error {
	if time.Now().After(c.tokenExpiry) {
		return c.RefreshAccessToken()
	}
	return nil
}

// doRequest performs an authenticated API request.
func (c *Client) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	if err := c.ensureValidToken(); err != nil {
		return nil, err
	}

	reqURL := baseURLSkoda + path
	req, err := http.NewRequest(method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	return c.httpClient.Do(req)
}

// GetVehicles returns all vehicles in the user's garage.
func (c *Client) GetVehicles() ([]Vehicle, error) {
	resp, err := c.doRequest("GET", "/api/v2/garage", nil)
	if err != nil {
		return nil, fmt.Errorf("fetching vehicles: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var garage GarageResponse
	if err := json.NewDecoder(resp.Body).Decode(&garage); err != nil {
		return nil, fmt.Errorf("decoding garage response: %w", err)
	}

	return garage.Vehicles, nil
}

// GetCharging returns the charging/battery status for a vehicle.
func (c *Client) GetCharging(vin string) (*Charging, error) {
	resp, err := c.doRequest("GET", "/api/v1/charging/"+vin, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching charging status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var charging Charging
	if err := json.NewDecoder(resp.Body).Decode(&charging); err != nil {
		return nil, fmt.Errorf("decoding charging response: %w", err)
	}

	return &charging, nil
}
