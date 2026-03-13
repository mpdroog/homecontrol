// Package pushover provides a wrapper for sending Pushover notifications.
package pushover

import (
	"fmt"

	"github.com/gregdel/pushover"
)

// Client is the Pushover notification client.
type Client struct {
	app   *pushover.Pushover
	user  *pushover.Recipient
	debug bool
}

// NewClient creates a new Pushover client with the given API token and user/group key.
func NewClient(token string, userKey string) *Client {
	return &Client{
		app:  pushover.New(token),
		user: pushover.NewRecipient(userKey),
	}
}

// SetDebug enables or disables debug output.
func (c *Client) SetDebug(debug bool) {
	c.debug = debug
}

// Send sends a message to the configured user/group.
func (c *Client) Send(message string) error {
	msg := pushover.NewMessage(message)

	if c.debug {
		fmt.Printf("[DEBUG] Sending message\n")
	}

	resp, err := c.app.SendMessage(msg, c.user)
	if err != nil {
		return fmt.Errorf("sending message: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Response: %+v\n", resp)
	}

	return nil
}

// SendWithTitle sends a message with a custom title to the configured user/group.
func (c *Client) SendWithTitle(title, message string) error {
	msg := pushover.NewMessageWithTitle(message, title)

	if c.debug {
		fmt.Printf("[DEBUG] Sending message with title\n")
	}

	resp, err := c.app.SendMessage(msg, c.user)
	if err != nil {
		return fmt.Errorf("sending message: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] Response: %+v\n", resp)
	}

	return nil
}
