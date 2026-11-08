// Package myenergi reads and controls a myenergi Zappi via the (unofficial)
// myenergi cloud API.
package myenergi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/icholy/digest"

	"github.com/mpdroog/homecontrol/internal/httpx"
)

const (
	directorURL = "https://director.myenergi.net"
	asnHeader   = "X_myenergi-Asn"
	timeout     = 30 * time.Second
	deciPerUnit = 10 // API reports voltage in 0.1 V
)

// Mode is the Zappi charge mode.
type Mode int

// Zappi charge modes as used by the API.
const (
	ModeFast    Mode = 1
	ModeEco     Mode = 2
	ModeEcoPlus Mode = 3
	ModeStop    Mode = 4
)

func (m Mode) String() string {
	switch m {
	case ModeFast:
		return "Fast"
	case ModeEco:
		return "Eco"
	case ModeEcoPlus:
		return "Eco+"
	case ModeStop:
		return "Stop"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Status is the Zappi charge status ("sta").
type Status int

// Known Zappi status values.
const (
	StatusPaused    Status = 1
	StatusDiverting Status = 3
	StatusBoosting  Status = 4
	StatusComplete  Status = 5
)

// Zappi is the subset of the status fields we use.
type Zappi struct {
	Plug           string  `json:"pst"` // A = no car, B1/B2/C1/C2 = connected, F = fault
	Serial         int64   `json:"sno"`
	Mode           Mode    `json:"zmo"`
	Status         Status  `json:"sta"`
	ChargeAddedKWh float64 `json:"che"` // energy added this session
	ChargePowerW   float64 `json:"div"` // current charge power
	VoltageDeciV   float64 `json:"vol"` // voltage in 0.1 V
	FrequencyHz    float64 `json:"frq"`
}

// Connected reports whether a car is plugged in.
func (z *Zappi) Connected() bool {
	return z.Plug != "" && z.Plug != "A"
}

// Voltage returns the grid voltage in volts.
func (z *Zappi) Voltage() float64 {
	return z.VoltageDeciV / deciPerUnit
}

// Client talks to the myenergi cloud for one Zappi.
type Client struct {
	http   *http.Client
	server string // e.g. https://s18.myenergi.net, discovered via the director
	serial string // Zappi serial
}

// New creates a client; hub is the hub (or vHub) serial, key the API key,
// serial the Zappi serial.
func New(hub, key, serial string) *Client {
	return &Client{
		http: &http.Client{ //nolint:exhaustruct // stdlib defaults for redirects and cookies
			Transport: &digest.Transport{Username: hub, Password: key}, //nolint:exhaustruct // library defaults
			Timeout:   timeout,
		},
		server: "",
		serial: serial,
	}
}

// Status returns the status of the Zappi.
func (c *Client) Status(ctx context.Context) (Zappi, error) {
	resp, err := c.get(ctx, "/cgi-jstatus-Z"+c.serial)
	if err != nil {
		return Zappi{}, fmt.Errorf("zappi status: %w", err)
	}

	var out struct {
		Zappi []Zappi `json:"zappi"`
	}

	err = json.Unmarshal(resp, &out)
	if err != nil {
		return Zappi{}, fmt.Errorf("zappi status: decoding: %w", err)
	}

	if len(out.Zappi) != 1 {
		return Zappi{}, fmt.Errorf("zappi status: %w: got %d devices for serial %s", ErrUnexpected, len(out.Zappi), c.serial)
	}

	return out.Zappi[0], nil
}

// SetMode switches the Zappi to the given charge mode.
func (c *Client) SetMode(ctx context.Context, mode Mode) error {
	_, err := c.get(ctx, fmt.Sprintf("/cgi-zappi-mode-Z%s-%d-0-0-0000", c.serial, mode))
	if err != nil {
		return fmt.Errorf("zappi set mode %s: %w", mode, err)
	}

	return nil
}

// get performs an authenticated GET; the first call goes via the director,
// which tells us (in a header) which server holds this hub.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	base := c.server
	if base == "" {
		base = directorURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := httpx.Do(c.http, req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}

	if asn := resp.Header.Get(asnHeader); asn != "" && asn != "undefined" {
		c.server = "https://" + asn
	}

	return resp.Body, nil
}
