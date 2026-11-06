// Package alphaess reads live data from an AlphaESS system via the AlphaESS
// Open API (https://open.alphaess.com).
package alphaess

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mpdroog/homecontrol/internal/httpx"
)

const (
	baseURL = "https://openapi.alphaess.com/api"
	timeout = 30 * time.Second
	codeOK  = 200
)

// ErrAPI is returned when the API answers with a non-200 "code" field.
var ErrAPI = errors.New("alphaess api error")

// Power is a live power snapshot. Positive BatteryW = discharging.
type Power struct {
	SOC      float64 `json:"soc"`
	BatteryW float64 `json:"pbat"`
	GridW    float64 `json:"pgrid"`
	LoadW    float64 `json:"pload"`
}

// Client talks to the Open API for one system.
type Client struct {
	http   *http.Client
	appID  string
	secret string
	sn     string
}

// New creates a client for the system with serial number sn.
func New(appID, secret, sn string) *Client {
	return &Client{
		http:   &http.Client{Timeout: timeout}, //nolint:exhaustruct // stdlib defaults are fine
		appID:  appID,
		secret: secret,
		sn:     sn,
	}
}

// Power returns the latest power data of the system.
func (c *Client) Power(ctx context.Context) (Power, error) {
	var out Power

	err := c.get(ctx, "/getLastPowerData?sysSn="+url.QueryEscape(c.sn), &out)
	if err != nil {
		return Power{}, fmt.Errorf("alphaess power: %w", err)
	}

	return out, nil
}

func (c *Client) get(ctx context.Context, path string, data any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha512.Sum512([]byte(c.appID + c.secret + ts))
	req.Header.Set("Appid", c.appID)
	req.Header.Set("Timestamp", ts)
	req.Header.Set("Sign", hex.EncodeToString(sum[:]))

	resp, err := httpx.Do(c.http, req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}

	var envelope struct {
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
		Code int             `json:"code"`
	}

	err = json.Unmarshal(resp.Body, &envelope)
	if err != nil {
		return fmt.Errorf("decoding envelope: %w", err)
	}

	if envelope.Code != codeOK {
		return fmt.Errorf("%w: code %d: %s", ErrAPI, envelope.Code, envelope.Msg)
	}

	err = json.Unmarshal(envelope.Data, data)
	if err != nil {
		return fmt.Errorf("decoding data: %w", err)
	}

	return nil
}
