package myenergi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// testdata/status.json is a real cgi-jstatus response: car plugged in (B2),
// mode Fast, charge complete, 19.72 kWh added.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := New("hub", "key", "20933805")
	c.server = srv.URL // skip the director

	return c
}

func TestStatusRealResponse(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cgi-jstatus-Z20933805" {
			t.Errorf("path %s", r.URL.Path)
		}

		_, werr := w.Write(body)
		if werr != nil {
			t.Error(werr)
		}
	})

	z, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if !z.Connected() || z.Mode != ModeFast || z.Status != StatusComplete {
		t.Errorf("got plug %q mode %s status %d, want connected, Fast, complete", z.Plug, z.Mode, z.Status)
	}

	if z.ChargeAddedKWh != 19.72 || z.Voltage() != 230.6 {
		t.Errorf("got %v kWh at %v V, want 19.72 kWh at 230.6 V", z.ChargeAddedKWh, z.Voltage())
	}
}

func TestSetModePath(t *testing.T) {
	t.Parallel()

	var path string

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path

		_, err := w.Write([]byte(`{"status":0}`))
		if err != nil {
			t.Error(err)
		}
	})

	err := c.SetMode(context.Background(), ModeStop)
	if err != nil {
		t.Fatalf("set mode: %v", err)
	}

	if path != "/cgi-zappi-mode-Z20933805-4-0-0-0000" {
		t.Errorf("path %s", path)
	}
}
