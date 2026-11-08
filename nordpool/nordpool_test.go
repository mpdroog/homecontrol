package nordpool

import (
	"errors"
	"os"
	"testing"
	"time"
)

// testdata/2026-11-08.json is a real response (NL, 15-minute periods).
func TestParseRealResponse(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/2026-11-08.json")
	if err != nil {
		t.Fatal(err)
	}

	prices, err := parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(prices) != 96 {
		t.Fatalf("got %d prices, want 96 quarter hours", len(prices))
	}

	first := prices[0]
	if want := time.Date(2026, 11, 7, 23, 0, 0, 0, time.UTC); !first.Start.Equal(want) {
		t.Errorf("first start %s, want %s", first.Start, want)
	}

	if got := first.End.Sub(first.Start); got != 15*time.Minute {
		t.Errorf("period %s, want 15m", got)
	}

	if first.EURPerKWh != 0.25192 {
		t.Errorf("first price %v EUR/kWh, want 0.25192 (251.92 EUR/MWh)", first.EURPerKWh)
	}
}

func TestParseMissingArea(t *testing.T) {
	t.Parallel()

	body := []byte(`{"multiAreaEntries":[{"deliveryStart":"2026-11-07T23:00:00Z",` +
		`"deliveryEnd":"2026-11-07T23:15:00Z","entryPerArea":{"BE":100}}]}`)

	_, err := parse(body)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("error %v, want ErrMalformed", err)
	}
}
