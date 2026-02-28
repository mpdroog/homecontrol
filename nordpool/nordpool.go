package nordpool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PricePoint represents one 15-min price slot
type PricePoint struct {
	Period   time.Time
	PriceEUR float64 // EUR/MWh
}

// Prices holds today and tomorrow prices
type Prices struct {
	Today    []PricePoint
	Tomorrow []PricePoint
}

// apiResponse represents the NordPool API response
type apiResponse struct {
	DeliveryDateCET   string `json:"deliveryDateCET"`
	MultiAreaEntries  []struct {
		DeliveryStart string             `json:"deliveryStart"`
		DeliveryEnd   string             `json:"deliveryEnd"`
		EntryPerArea  map[string]float64 `json:"entryPerArea"`
	} `json:"multiAreaEntries"`
	AreaAverages []struct {
		AreaCode string  `json:"areaCode"`
		Price    float64 `json:"price"`
	} `json:"areaAverages"`
}

// Client fetches electricity prices from NordPool
type Client struct {
	debug bool
}

// NewClient creates a new NordPool client
func NewClient() *Client {
	return &Client{}
}

// SetDebug enables debug output
func (c *Client) SetDebug(debug bool) {
	c.debug = debug
}

// GetPrices fetches prices for today and tomorrow
func (c *Client) GetPrices() (*Prices, error) {
	prices := &Prices{}

	// Fetch today's prices
	today := time.Now()
	todayPrices, err := c.fetchPrices(today)
	if err != nil {
		return nil, fmt.Errorf("fetching today's prices: %w", err)
	}
	prices.Today = todayPrices

	// Fetch tomorrow's prices (may not be available yet)
	tomorrow := today.AddDate(0, 0, 1)
	tomorrowPrices, err := c.fetchPrices(tomorrow)
	if err == nil {
		prices.Tomorrow = tomorrowPrices
	}
	// Ignore errors for tomorrow - prices may not be published yet

	return prices, nil
}

// fetchPrices fetches prices for a specific date
func (c *Client) fetchPrices(date time.Time) ([]PricePoint, error) {
	dateStr := date.Format("2006-01-02")

	url := fmt.Sprintf(
		"https://dataportal-api.nordpoolgroup.com/api/DayAheadPrices?date=%s&market=DayAhead&deliveryArea=NL&currency=EUR",
		dateStr,
	)

	if c.debug {
		fmt.Println("Fetching:", url)
	}

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status: %s", resp.Status)
	}

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("JSON decode error: %w", err)
	}

	var prices []PricePoint
	loc, _ := time.LoadLocation("Europe/Amsterdam")

	for _, entry := range apiResp.MultiAreaEntries {
		price, ok := entry.EntryPerArea["NL"]
		if !ok {
			continue
		}

		// Parse delivery start time (UTC)
		t, err := time.Parse(time.RFC3339, entry.DeliveryStart)
		if err != nil {
			continue
		}

		// Convert to local time
		localTime := t.In(loc)

		// Only include prices for the requested date
		if localTime.Format("2006-01-02") != dateStr {
			continue
		}

		prices = append(prices, PricePoint{
			Period:   localTime,
			PriceEUR: price,
		})
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("no prices found for %s", dateStr)
	}

	return prices, nil
}

// GetCurrentPrice returns the price for the current 15-min slot
func (c *Client) GetCurrentPrice(prices *Prices) *PricePoint {
	now := time.Now()
	for i, p := range prices.Today {
		// Check if current time falls within this 15-min slot
		if now.Hour() == p.Period.Hour() && now.Minute()/15 == p.Period.Minute()/15 {
			return &prices.Today[i]
		}
	}
	return nil
}

// GetNextPrice returns the price for the next 15-min slot
func (c *Client) GetNextPrice(prices *Prices) *PricePoint {
	now := time.Now()
	// Find the next slot (15 minutes from now)
	nextSlot := now.Add(15 * time.Minute)

	for i, p := range prices.Today {
		if nextSlot.Hour() == p.Period.Hour() && nextSlot.Minute()/15 == p.Period.Minute()/15 {
			return &prices.Today[i]
		}
	}

	// Check tomorrow if next slot crosses midnight
	if len(prices.Tomorrow) > 0 && nextSlot.Day() != now.Day() {
		return &prices.Tomorrow[0]
	}
	return nil
}

// GetLowestPrice returns the lowest price across today and tomorrow
func (c *Client) GetLowestPrice(prices *Prices) *PricePoint {
	var lowest *PricePoint

	all := append(prices.Today, prices.Tomorrow...)
	for i, p := range all {
		if lowest == nil || p.PriceEUR < lowest.PriceEUR {
			lowest = &all[i]
		}
	}
	return lowest
}

// GetHighestPrice returns the highest price across today and tomorrow
func (c *Client) GetHighestPrice(prices *Prices) *PricePoint {
	var highest *PricePoint

	all := append(prices.Today, prices.Tomorrow...)
	for i, p := range all {
		if highest == nil || p.PriceEUR > highest.PriceEUR {
			highest = &all[i]
		}
	}
	return highest
}

// PricePerKWh converts MWh price to kWh price
func (p *PricePoint) PricePerKWh() float64 {
	return p.PriceEUR / 1000.0
}
