// Package cald reads tagged events from a cald-web calendar
// (github.com/mpdroog/cald, GET /api/tagged).
package cald

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mpdroog/homecontrol/internal/httpx"
)

const timeout = 30 * time.Second

// Event is a calendar event that has one of the requested tags.
type Event struct {
	Date      string   `json:"date"`      // "2006-01-02"
	StartTime string   `json:"startTime"` // "08:30", empty for all-day
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
}

// Client reads one cald-web instance (behind HTTP basic auth).
type Client struct {
	http *http.Client
	url  string
	user string
	pass string
}

// New creates a client for the cald-web base URL, e.g. https://ical.rootdev.nl.
func New(baseURL, user, pass string) *Client {
	return &Client{
		http: &http.Client{Timeout: timeout}, //nolint:exhaustruct // stdlib defaults are fine
		url:  strings.TrimRight(baseURL, "/"),
		user: user,
		pass: pass,
	}
}

// Tagged returns the events with any of tags on the days starting at from.
func (c *Client) Tagged(ctx context.Context, from time.Time, days int, tags ...string) ([]Event, error) {
	query := url.Values{"tag": tags, "from": {from.Format(time.DateOnly)}, "days": {strconv.Itoa(days)}}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/api/tagged?"+query.Encode(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("cald: building request: %w", err)
	}

	req.SetBasicAuth(c.user, c.pass)

	resp, err := httpx.Do(c.http, req)
	if err != nil {
		return nil, fmt.Errorf("cald: %w", err)
	}

	var out struct {
		Events []Event `json:"events"`
	}

	err = json.Unmarshal(resp.Body, &out)
	if err != nil {
		return nil, fmt.Errorf("cald: decoding: %w", err)
	}

	return out.Events, nil
}
