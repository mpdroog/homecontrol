package main

import (
	"context"
	"log/slog"

	"github.com/mpdroog/homecontrol/myenergi"
)

// logNotifier is used without Pushover config and in dry-run mode.
type logNotifier struct {
	log *slog.Logger
}

func (l logNotifier) Notify(ctx context.Context, title, message string) error {
	l.log.InfoContext(ctx, "notification", "title", title, "message", message)

	return nil
}

// dryCharger reads the real Zappi but only logs mode changes.
type dryCharger struct {
	Charger

	log *slog.Logger
}

func (d dryCharger) SetMode(ctx context.Context, mode myenergi.Mode) error {
	d.log.InfoContext(ctx, "dry-run: would set zappi mode", "mode", mode)

	return nil
}

// dryCar reads the real car but does not wake it.
type dryCar struct {
	Car

	log *slog.Logger
}

func (d dryCar) StartCharging(ctx context.Context) error {
	d.log.InfoContext(ctx, "dry-run: would ask car to start charging")

	return nil
}
