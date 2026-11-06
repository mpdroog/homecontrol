// Package pushover sends notifications via the Pushover API.
package pushover

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mpdroog/homecontrol/internal/httpx"
)

const (
	endpoint = "https://api.pushover.net/1/messages.json"
	timeout  = 30 * time.Second
)

// Client sends messages to one user or group.
type Client struct {
	http  *http.Client
	token string
	user  string
}

// New creates a client with the application token and user/group key.
func New(token, user string) *Client {
	return &Client{
		http:  &http.Client{Timeout: timeout}, //nolint:exhaustruct // stdlib defaults are fine
		token: token,
		user:  user,
	}
}

// Send delivers a message with a title.
func (c *Client) Send(ctx context.Context, title, message string) error {
	form := url.Values{
		"token":   {c.token},
		"user":    {c.user},
		"title":   {title},
		"message": {message},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("pushover: building request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	_, err = httpx.Do(c.http, req)
	if err != nil {
		return fmt.Errorf("pushover: %w", err)
	}

	return nil
}
