// Package httpx contains the one HTTP helper every client uses: send a
// request, always close the body, and turn non-2xx answers into errors.
package httpx

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxBody caps how much of a response we read (protects against runaway bodies).
const maxBody = 4 << 20

// maxErrBody caps how much of an error body ends up in an error message.
const maxErrBody = 300

// StatusError is returned for responses outside the 2xx range.
type StatusError struct {
	Method string
	URL    string
	Body   string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.URL, e.Code, e.Body)
}

// Response is a fully read HTTP response.
type Response struct {
	Header http.Header
	Body   []byte
	Code   int
}

// Do sends req, reads the whole body and closes it. Non-2xx answers return a
// *StatusError (together with the response, so callers can inspect it).
func Do(client *http.Client, req *http.Request) (*Response, error) {
	resp, err := client.Do(req) //nolint:gosec // G704: URLs are fixed API endpoints from code, never user input
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Redacted(), err)
	}

	body, err := ReadAndClose(resp)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Redacted(), err)
	}

	out := &Response{Header: resp.Header, Body: body, Code: resp.StatusCode}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return out, &StatusError{
			Method: req.Method,
			URL:    req.URL.Redacted(),
			Code:   resp.StatusCode,
			Body:   truncate(string(body), maxErrBody),
		}
	}

	return out, nil
}

// ReadAndClose reads the body of resp and always closes it; a close error is
// returned too (joined with a read error if both happen).
func ReadAndClose(resp *http.Response) ([]byte, error) {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	closeErr := resp.Body.Close()

	if readErr != nil || closeErr != nil {
		return nil, errors.Join(wrap("reading body", readErr), wrap("closing body", closeErr))
	}

	return body, nil
}

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("%s: %w", what, err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "…"
}
