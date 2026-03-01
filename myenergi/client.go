// Package myenergi provides a Go client for the myenergi API (Zappi, Eddi, etc).
package myenergi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/icholy/digest"
)

// Client is the myenergi API client.
type Client struct {
	httpClient *http.Client
	hubSerial  string // Hub serial number (username)
	password   string
	serverURL  string // Determined from director or last digit of serial
	debug      bool
}

// ZappiMode represents the charging mode.
type ZappiMode int

const (
	ZappiModeFast    ZappiMode = 1
	ZappiModeEco     ZappiMode = 2
	ZappiModeEcoPlus ZappiMode = 3
	ZappiModeStopped ZappiMode = 4
)

func (m ZappiMode) String() string {
	switch m {
	case ZappiModeFast:
		return "Fast"
	case ZappiModeEco:
		return "Eco"
	case ZappiModeEcoPlus:
		return "Eco+"
	case ZappiModeStopped:
		return "Stopped"
	default:
		return fmt.Sprintf("Unknown(%d)", m)
	}
}

// ZappiStatus represents the charging status.
type ZappiStatus int

const (
	ZappiStatusPaused       ZappiStatus = 1
	ZappiStatusCharging     ZappiStatus = 3
	ZappiStatusFastCharging ZappiStatus = 4
	ZappiStatusComplete     ZappiStatus = 5
)

func (s ZappiStatus) String() string {
	switch s {
	case ZappiStatusPaused:
		return "Paused"
	case ZappiStatusCharging:
		return "Charging"
	case ZappiStatusFastCharging:
		return "Fast Charging"
	case ZappiStatusComplete:
		return "Complete"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// ZappiPlugStatus represents the plug connection status.
type ZappiPlugStatus string

func (p ZappiPlugStatus) String() string {
	switch p {
	case "A":
		return "Disconnected"
	case "B1":
		return "Connected"
	case "B2":
		return "Waiting for EV"
	case "C1":
		return "EV Ready"
	case "C2":
		return "Charging"
	case "F":
		return "Fault"
	default:
		return string(p)
	}
}

// Zappi represents a Zappi EV charger device.
type Zappi struct {
	Serial         int64           `json:"sno"`    // Serial number
	Mode           ZappiMode       `json:"zmo"`    // Charging mode
	Status         ZappiStatus     `json:"sta"`    // Charging status
	PlugStatus     ZappiPlugStatus `json:"pst"`    // Plug status
	ChargeAdded    float64         `json:"che"`    // Charge added this session (kWh)
	Diverted       float64         `json:"div"`    // Current diversion (W)
	GridPower      float64         `json:"grd"`    // Grid power (W), positive=importing, negative=exporting
	GeneratedPower float64         `json:"gen"`    // Generated/solar power (W)

	// CT clamp readings (W) - what each measures depends on configuration
	CT1Power       float64         `json:"ectp1"`  // CT1 power (often internal/charger)
	CT2Power       float64         `json:"ectp2"`  // CT2 power
	CT3Power       float64         `json:"ectp3"`  // CT3 power
	CT4Power       float64         `json:"ectp4"`  // CT4 power
	CT5Power       float64         `json:"ectp5"`  // CT5 power
	CT6Power       float64         `json:"ectp6"`  // CT6 power

	// CT clamp types - describes what each CT measures
	CT1Type        string          `json:"ectt1"`  // CT1 type (e.g., "Internal Load", "Grid", "Generation")
	CT2Type        string          `json:"ectt2"`  // CT2 type
	CT3Type        string          `json:"ectt3"`  // CT3 type
	CT4Type        string          `json:"ectt4"`  // CT4 type
	CT5Type        string          `json:"ectt5"`  // CT5 type
	CT6Type        string          `json:"ectt6"`  // CT6 type

	MinGreenLevel  int             `json:"mgl"`    // Minimum green level (%)
	Voltage        float64         `json:"vol"`    // Voltage (V/10)
	Frequency      float64         `json:"frq"`    // Frequency (Hz/100)
	BoostKWh       float64         `json:"sbk"`    // Smart boost kWh requested
	SmartBoostHour int             `json:"sbh"`    // Smart boost hour
	SmartBoostMin  int             `json:"sbm"`    // Smart boost minute
	LockStatus     int             `json:"lck"`    // Lock status
	FirmwareVer    string          `json:"fwv"`    // Firmware version

	// Phase data for 3-phase installations
	Phase1Power    float64         `json:"ectp1p1"` // Phase 1 power
	Phase2Power    float64         `json:"ectp1p2"` // Phase 2 power
	Phase3Power    float64         `json:"ectp1p3"` // Phase 3 power
}

// ChargerPower returns the power being used by the charger (W).
// This is typically CT1 which measures internal load.
func (z *Zappi) ChargerPower() float64 {
	return z.CT1Power
}

// SolarPower returns the solar/generation power (W).
// Uses the 'gen' field which aggregates all generation CTs.
func (z *Zappi) SolarPower() float64 {
	return z.GeneratedPower
}

// HouseConsumption calculates house power consumption (W).
// Formula: Grid import + Solar generation - EV charging - Export
// Positive grid = importing, negative = exporting
func (z *Zappi) HouseConsumption() float64 {
	// House load = What's coming in (grid + solar) minus what's going to the car
	// GridPower: positive = importing from grid, negative = exporting to grid
	// GeneratedPower: solar production (always positive)
	// CT1Power (charger): power going to the EV
	return z.GridPower + z.GeneratedPower - z.CT1Power
}

// VoltageV returns the voltage in Volts (API returns V/10).
func (z *Zappi) VoltageV() float64 {
	return z.Voltage / 10
}

// FrequencyHz returns the frequency in Hz (API returns Hz/100).
func (z *Zappi) FrequencyHz() float64 {
	return z.Frequency / 100
}

// IsExporting returns true if exporting power to the grid.
func (z *Zappi) IsExporting() bool {
	return z.GridPower < 0
}

// IsImporting returns true if importing power from the grid.
func (z *Zappi) IsImporting() bool {
	return z.GridPower > 0
}

// NewClient creates a new myenergi client.
func NewClient(hubSerial, password string) *Client {
	// Create transport with digest auth
	transport := &digest.Transport{
		Username: hubSerial,
		Password: password,
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		hubSerial: hubSerial,
		password:  password,
	}
}

// SetDebug enables or disables debug output.
func (c *Client) SetDebug(debug bool) {
	c.debug = debug
}

// discoverServer finds the correct server URL using the director.
func (c *Client) discoverServer() error {
	if c.serverURL != "" {
		return nil
	}

	if len(c.hubSerial) == 0 {
		return fmt.Errorf("hub serial is empty")
	}

	// Query director to get the correct server
	directorURL := "https://director.myenergi.net"

	if c.debug {
		fmt.Printf("[DEBUG] Querying director: %s\n", directorURL)
	}

	req, err := http.NewRequest("GET", directorURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Wget/1.14 (linux-gnu)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("director request failed: %w", err)
	}
	defer resp.Body.Close()

	// Get server from response header (check various capitalizations)
	asn := resp.Header.Get("X_MYENERGI-asn")
	if asn == "" {
		asn = resp.Header.Get("x_myenergi-asn")
	}
	if asn == "" {
		asn = resp.Header.Get("X-Myenergi-Asn")
	}

	if c.debug {
		fmt.Printf("[DEBUG] Director response status: %d\n", resp.StatusCode)
		fmt.Printf("[DEBUG] Director ASN header: %s\n", asn)
	}

	if asn != "" {
		c.serverURL = "https://" + asn
	} else {
		// Fallback: try s18 which is commonly used for newer installations
		c.serverURL = "https://s18.myenergi.net"
	}

	if c.debug {
		fmt.Printf("[DEBUG] Using server URL: %s\n", c.serverURL)
	}

	return nil
}

// doRequest performs an authenticated request.
func (c *Client) doRequest(method, path string) ([]byte, error) {
	if err := c.discoverServer(); err != nil {
		return nil, fmt.Errorf("discovering server: %w", err)
	}

	url := c.serverURL + path

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Wget/1.14 (linux-gnu)")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if c.debug {
		fmt.Printf("[DEBUG] Request: %s %s\n", method, url)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Response: %s\n", string(body))
	}

	return body, nil
}

// ZappiResponse is the response from the Zappi status endpoint.
type ZappiResponse struct {
	Zappi []Zappi `json:"zappi"`
}

// GetZappiStatus returns the status of all Zappi devices.
func (c *Client) GetZappiStatus() ([]Zappi, error) {
	body, err := c.doRequest("GET", "/cgi-jstatus-Z")
	if err != nil {
		return nil, fmt.Errorf("fetching Zappi status: %w", err)
	}

	var resp ZappiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return resp.Zappi, nil
}

// SetZappiMode sets the charging mode for a Zappi.
func (c *Client) SetZappiMode(serial string, mode ZappiMode) error {
	path := fmt.Sprintf("/cgi-zappi-mode-Z%s-%d-0-0-0000", serial, mode)
	_, err := c.doRequest("GET", path)
	if err != nil {
		return fmt.Errorf("setting Zappi mode: %w", err)
	}
	return nil
}

// BoostZappi starts a boost charge for the specified kWh.
func (c *Client) BoostZappi(serial string, kwh int) error {
	path := fmt.Sprintf("/cgi-zappi-mode-Z%s-0-10-%d-0000", serial, kwh)
	_, err := c.doRequest("GET", path)
	if err != nil {
		return fmt.Errorf("boosting Zappi: %w", err)
	}
	return nil
}

// SmartBoostZappi starts a smart boost to complete by the specified time.
func (c *Client) SmartBoostZappi(serial string, kwh int, hour, minute int) error {
	timeStr := fmt.Sprintf("%02d%02d", hour, minute)
	path := fmt.Sprintf("/cgi-zappi-mode-Z%s-0-11-%d-%s", serial, kwh, timeStr)
	_, err := c.doRequest("GET", path)
	if err != nil {
		return fmt.Errorf("smart boosting Zappi: %w", err)
	}
	return nil
}

// StopBoostZappi stops any active boost.
func (c *Client) StopBoostZappi(serial string) error {
	path := fmt.Sprintf("/cgi-zappi-mode-Z%s-0-2-0-0000", serial)
	_, err := c.doRequest("GET", path)
	if err != nil {
		return fmt.Errorf("stopping Zappi boost: %w", err)
	}
	return nil
}

// SetMinGreenLevel sets the minimum green level percentage.
func (c *Client) SetMinGreenLevel(serial string, percent int) error {
	path := fmt.Sprintf("/cgi-set-min-green-Z%s-%d", serial, percent)
	_, err := c.doRequest("GET", path)
	if err != nil {
		return fmt.Errorf("setting min green level: %w", err)
	}
	return nil
}
