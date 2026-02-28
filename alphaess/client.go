// Package alphaess provides a Go client for the AlphaESS Open API.
package alphaess

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	baseURL = "https://openapi.alphaess.com/api"
)

// Client is the AlphaESS API client.
type Client struct {
	httpClient *http.Client
	appID      string
	appSecret  string
	sn         string // System serial number (optional, can be auto-discovered)
	debug      bool
}

// APIResponse is the common response wrapper.
type APIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// ESSSystem represents an ESS system.
type ESSSystem struct {
	SN string `json:"sysSn"`
}

// ESSListResponse is the response from getEssList.
type ESSListResponse struct {
	APIResponse
	Data []ESSSystem `json:"data"`
}

// PowerData contains real-time power information.
type PowerData struct {
	SOC          float64 `json:"soc"`          // State of charge (%)
	BatteryPower float64 `json:"pbat"`         // Battery power (W), +charging / -discharging
	GridPower    float64 `json:"pgrid"`        // Grid power (W)
	PVPower      float64 `json:"ppv"`          // PV/Solar power (W)
	LoadPower    float64 `json:"pload"`        // Load/consumption power (W)
}

// PowerResponse is the response from getLastPowerData.
type PowerResponse struct {
	APIResponse
	Data PowerData `json:"data"`
}

// ChargeConfigData contains charging configuration settings.
type ChargeConfigData struct {
	GridCharge int     `json:"gridCharge"` // 1 = enabled, 0 = disabled
	TimeChaf1  string  `json:"timeChaf1"`  // Charge start time 1 (HH:MM)
	TimeChae1  string  `json:"timeChae1"`  // Charge end time 1 (HH:MM)
	TimeChaf2  string  `json:"timeChaf2"`  // Charge start time 2 (HH:MM)
	TimeChae2  string  `json:"timeChae2"`  // Charge end time 2 (HH:MM)
	BatHighCap float64 `json:"batHighCap"` // Battery high capacity threshold (%)
}

// ChargeConfigResponse is the response from getChargeConfigInfo.
type ChargeConfigResponse struct {
	APIResponse
	Data ChargeConfigData `json:"data"`
}

// DischargeConfigData contains discharge configuration settings.
type DischargeConfigData struct {
	CtrDis    int     `json:"ctrDis"`    // Discharge control: 1 = enabled, 0 = disabled
	TimeDisf1 string  `json:"timeDisf1"` // Discharge start time 1 (HH:MM)
	TimeDise1 string  `json:"timeDise1"` // Discharge end time 1 (HH:MM)
	TimeDisf2 string  `json:"timeDisf2"` // Discharge start time 2 (HH:MM)
	TimeDise2 string  `json:"timeDise2"` // Discharge end time 2 (HH:MM)
	BatUseCap float64 `json:"batUseCap"` // Battery use capacity / min SOC (%)
}

// DischargeConfigResponse is the response from getDisChargeConfigInfo.
type DischargeConfigResponse struct {
	APIResponse
	Data DischargeConfigData `json:"data"`
}

// NewClient creates a new AlphaESS client.
func NewClient(appID, appSecret string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		appID:      appID,
		appSecret:  appSecret,
	}
}

// SetSN sets the system serial number to use. If not set, the first system will be used.
func (c *Client) SetSN(sn string) {
	c.sn = sn
}

// SetDebug enables or disables debug output.
func (c *Client) SetDebug(debug bool) {
	c.debug = debug
}

// sign generates the API signature: SHA512(appID + appSecret + timestamp)
func (c *Client) sign(timestamp string) string {
	data := c.appID + c.appSecret + timestamp
	hash := sha512.Sum512([]byte(data))
	return hex.EncodeToString(hash[:])
}

// doRequest performs an authenticated API request with signing headers.
func (c *Client) doRequest(method, path string) ([]byte, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature := c.sign(timestamp)

	req, err := http.NewRequest(method, baseURL+path, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("appId", c.appID)
	req.Header.Set("timestamp", timestamp)
	req.Header.Set("timeStamp", timestamp)
	req.Header.Set("sign", signature)

	if c.debug {
		fmt.Printf("[DEBUG] Request: %s %s\n", method, baseURL+path)
		fmt.Printf("[DEBUG] Headers: appId=%s, timestamp=%s, sign=%s...\n", c.appID, timestamp, signature[:16])
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Response: %s\n", string(body))
	}

	return body, nil
}

// GetESSList returns all ESS systems associated with the account.
func (c *Client) GetESSList() ([]ESSSystem, error) {
	body, err := c.doRequest("GET", "/getEssList")
	if err != nil {
		return nil, fmt.Errorf("fetching ESS list: %w", err)
	}

	var essResp ESSListResponse
	if err := json.Unmarshal(body, &essResp); err != nil {
		return nil, fmt.Errorf("decoding ESS list: %w", err)
	}

	if essResp.Code != 200 {
		return nil, fmt.Errorf("get ESS list failed: %s (code %d)", essResp.Msg, essResp.Code)
	}

	return essResp.Data, nil
}

// GetSN returns the system SN to use (configured or first available).
func (c *Client) GetSN() (string, error) {
	if c.sn != "" {
		return c.sn, nil
	}

	systems, err := c.GetESSList()
	if err != nil {
		return "", err
	}

	if len(systems) == 0 {
		return "", fmt.Errorf("no ESS systems found")
	}

	c.sn = systems[0].SN
	return c.sn, nil
}

// GetLastPowerData returns the latest power data for the system.
func (c *Client) GetLastPowerData() (*PowerData, error) {
	sn, err := c.GetSN()
	if err != nil {
		return nil, err
	}

	body, err := c.doRequest("GET", "/getLastPowerData?sysSn="+sn)
	if err != nil {
		return nil, fmt.Errorf("fetching power data: %w", err)
	}

	var powerResp PowerResponse
	if err := json.Unmarshal(body, &powerResp); err != nil {
		return nil, fmt.Errorf("decoding power data: %w", err)
	}

	if powerResp.Code != 200 {
		return nil, fmt.Errorf("get power data failed: %s (code %d)", powerResp.Msg, powerResp.Code)
	}

	return &powerResp.Data, nil
}

// GetChargeConfig returns the charging configuration for the system.
func (c *Client) GetChargeConfig() (*ChargeConfigData, error) {
	sn, err := c.GetSN()
	if err != nil {
		return nil, err
	}

	body, err := c.doRequest("GET", "/getChargeConfigInfo?sysSn="+sn)
	if err != nil {
		return nil, fmt.Errorf("fetching charge config: %w", err)
	}

	var configResp ChargeConfigResponse
	if err := json.Unmarshal(body, &configResp); err != nil {
		return nil, fmt.Errorf("decoding charge config: %w", err)
	}

	if configResp.Code != 200 {
		return nil, fmt.Errorf("get charge config failed: %s (code %d)", configResp.Msg, configResp.Code)
	}

	return &configResp.Data, nil
}

// GetDischargeConfig returns the discharge configuration for the system.
func (c *Client) GetDischargeConfig() (*DischargeConfigData, error) {
	sn, err := c.GetSN()
	if err != nil {
		return nil, err
	}

	body, err := c.doRequest("GET", "/getDisChargeConfigInfo?sysSn="+sn)
	if err != nil {
		return nil, fmt.Errorf("fetching discharge config: %w", err)
	}

	var configResp DischargeConfigResponse
	if err := json.Unmarshal(body, &configResp); err != nil {
		return nil, fmt.Errorf("decoding discharge config: %w", err)
	}

	if configResp.Code != 200 {
		return nil, fmt.Errorf("get discharge config failed: %s (code %d)", configResp.Msg, configResp.Code)
	}

	return &configResp.Data, nil
}
