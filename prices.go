package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mpdroog/homecontrol/nordpool"
)

const (
	// priceRefresh: refetch prices at least this often.
	priceRefresh = time.Hour
	// tomorrowRetry: retry interval while tomorrow's prices are missing
	// after they are normally published (~13:00).
	tomorrowRetry = 15 * time.Minute
	publishHour   = 13
)

// priceCache serves day-ahead slots for today and (when published) tomorrow.
type priceCache struct {
	fetched time.Time
	source  *nordpool.Client
	log     *slog.Logger
	slots   []slot
}

func newPriceCache(source *nordpool.Client, log *slog.Logger) *priceCache {
	return &priceCache{source: source, log: log, fetched: time.Time{}, slots: nil}
}

// Slots implements PriceSource.
func (p *priceCache) Slots(ctx context.Context, now time.Time) ([]slot, error) {
	if !p.stale(now) {
		return p.slots, nil
	}

	today, err := p.source.Day(ctx, now)
	if err != nil {
		if _, ok := slotAt(p.slots, now); ok {
			p.log.WarnContext(ctx, "price refresh failed, using cached prices", "err", err)

			return p.slots, nil
		}

		return nil, fmt.Errorf("today: %w", err)
	}

	slots := toSlots(today)

	tomorrow, err := p.source.Day(ctx, now.AddDate(0, 0, 1))

	switch {
	case err == nil:
		slots = append(slots, toSlots(tomorrow)...)
	case errors.Is(err, nordpool.ErrNotPublished):
		p.log.DebugContext(ctx, "tomorrow's prices not published yet")
	default:
		p.log.WarnContext(ctx, "fetching tomorrow's prices failed", "err", err)
	}

	p.slots, p.fetched = slots, now

	return p.slots, nil
}

func (p *priceCache) stale(now time.Time) bool {
	if p.fetched.IsZero() || now.Sub(p.fetched) >= priceRefresh {
		return true
	}

	tomorrow := now.AddDate(0, 0, 1)
	endOfTomorrow := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 23, 0, 0, 0, now.Location())

	return now.Hour() >= publishHour && !covers(p.slots, endOfTomorrow) && now.Sub(p.fetched) >= tomorrowRetry
}

func toSlots(prices []nordpool.Price) []slot {
	out := make([]slot, 0, len(prices))
	for _, pr := range prices {
		out = append(out, slot{Start: pr.Start, End: pr.End, Price: pr.EURPerKWh})
	}

	return out
}
