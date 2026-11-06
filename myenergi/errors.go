package myenergi

import "errors"

// ErrUnexpected is returned when the API answers with an unexpected shape.
var ErrUnexpected = errors.New("unexpected response")
