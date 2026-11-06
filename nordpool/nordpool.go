// Package nordpool fetches Dutch day-ahead electricity prices from the
// public Nord Pool data portal.
package nordpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/mpdroog/homecontrol/internal/httpx"
)

const (
	endpoint = "https://dataportal-api.nordpoolgroup.com/api/DayAheadPrices"
	area     = "NL"
	timeout  = 30 * time.Second
	mwhToKWh = 1000
)

// ErrNotPublished means the prices for that day are not available (yet).
// Day-ahead prices for tomorrow are published around 13:00 CET.
var ErrNotPublished = errors.New("prices not published")

// ErrMalformed means the response did not have the expected shape.
var ErrMalformed = errors.New("malformed response")

// Price is the day-ahead price for one delivery period.
type Price struct {
	Start     time.Time
	End       time.Time
	EURPerKWh float64
}

// Client fetches prices.
type Client struct {
	http *http.Client
}

// New creates a client.
func New() *Client {
	return &Client{http: &http.Client{Timeout: timeout}} //nolint:exhaustruct // stdlib defaults are fine
}

// Day returns all prices for the delivery date of day (as seen in day's location).
func (c *Client) Day(ctx context.Context, day time.Time) ([]Price, error) {
	date := day.Format(time.DateOnly)
	query := url.Values{
		"date":         {date},
		"market":       {"DayAhead"},
		"deliveryArea": {area},
		"currency":     {"EUR"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("nordpool %s: building request: %w", date, err)
	}

	resp, err := httpx.Do(c.http, req)
	if err != nil {
		return nil, fmt.Errorf("nordpool %s: %w", date, err)
	}

	if resp.Code == http.StatusNoContent || len(resp.Body) == 0 {
		return nil, fmt.Errorf("nordpool %s: %w", date, ErrNotPublished)
	}

	prices, err := parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("nordpool %s: %w", date, err)
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("nordpool %s: %w", date, ErrNotPublished)
	}

	return prices, nil
}

type entry struct {
	Start   time.Time          `json:"deliveryStart"`
	End     time.Time          `json:"deliveryEnd"`
	PerArea map[string]float64 `json:"entryPerArea"`
}

type dayAhead struct {
	Entries []entry `json:"multiAreaEntries"`
}

func parse(body []byte) ([]Price, error) {
	var data dayAhead

	err := json.Unmarshal(body, &data)
	if err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}

	prices := make([]Price, 0, len(data.Entries))
	for _, period := range data.Entries {
		eurMWh, ok := period.PerArea[area]
		if !ok {
			return nil, fmt.Errorf("%w: entry %s has no %s price", ErrMalformed, period.Start, area)
		}

		prices = append(prices, Price{Start: period.Start, End: period.End, EURPerKWh: eurMWh / mwhToKWh})
	}

	return prices, nil
}
