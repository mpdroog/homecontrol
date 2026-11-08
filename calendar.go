package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mpdroog/homecontrol/cald"
)

const (
	// tagCharge / tagSkip: cald tags for an extra charge day and a skipped day.
	tagCharge = "auto"
	tagSkip   = "geenauto"
	// calendarRefresh: how often the calendar is read; calendarDays: how far ahead.
	calendarRefresh = 15 * time.Minute
	calendarDays    = 8
)

// Calendar returns the exceptions to the weekly charge days.
type Calendar interface {
	Overrides(ctx context.Context, now time.Time) (dayOverrides, error)
}

// calendarCache reads cald at most every calendarRefresh. On failure it keeps
// serving the last known overrides (empty before the first success) together
// with the error, until a fetch succeeds again.
type calendarCache struct {
	fetched time.Time
	lastErr error
	source  *cald.Client
	ov      dayOverrides
	readyBy time.Duration
}

func newCalendarCache(source *cald.Client, readyBy time.Duration) *calendarCache {
	return &calendarCache{source: source, readyBy: readyBy, fetched: time.Time{}, lastErr: nil, ov: dayOverrides{extra: nil, skip: nil}}
}

// Overrides implements Calendar.
func (c *calendarCache) Overrides(ctx context.Context, now time.Time) (dayOverrides, error) {
	if !c.fetched.IsZero() && now.Sub(c.fetched) < calendarRefresh {
		return c.ov, c.lastErr
	}

	c.fetched = now

	events, err := c.source.Tagged(ctx, now, calendarDays, tagCharge, tagSkip)
	if err != nil {
		c.lastErr = fmt.Errorf("calendar: %w", err)

		return c.ov, c.lastErr
	}

	c.ov, c.lastErr = toOverrides(events, c.readyBy), nil

	return c.ov, nil
}

// toOverrides turns tagged events into overrides. An extra day must be ready
// by ready_by, or earlier when the event starts earlier.
func toOverrides(events []cald.Event, readyBy time.Duration) dayOverrides {
	overrides := dayOverrides{extra: map[string]time.Duration{}, skip: map[string]bool{}}

	for _, event := range events {
		if hasTag(event.Tags, tagSkip) {
			overrides.skip[event.Date] = true
		}

		if !hasTag(event.Tags, tagCharge) {
			continue
		}

		readyAt := readyBy

		start, err := time.Parse("15:04", event.StartTime)
		if err == nil {
			readyAt = min(readyAt, time.Duration(start.Hour())*time.Hour+time.Duration(start.Minute())*time.Minute)
		}

		if prev, ok := overrides.extra[event.Date]; ok {
			readyAt = min(readyAt, prev)
		}

		overrides.extra[event.Date] = readyAt
	}

	return overrides
}

func hasTag(tags []string, want string) bool {
	return slices.ContainsFunc(tags, func(tag string) bool { return strings.EqualFold(tag, want) })
}
