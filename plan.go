package main

import (
	"cmp"
	"slices"
	"time"
)

// slot is one day-ahead price period (15 minutes since Oct 2025).
type slot struct {
	Start time.Time
	End   time.Time
	Price float64 // EUR/kWh, excl. tax
}

// planSlots picks the cheapest slots between now and deadline that together
// give at least `hours` of charging. The slot currently running counts only
// for its remaining time. Slots do not have to be consecutive.
// Returns the chosen slots sorted by start time.
func planSlots(slots []slot, now, deadline time.Time, hours float64) []slot {
	if hours <= 0 {
		return nil
	}

	candidates := slices.DeleteFunc(slices.Clone(slots), func(candidate slot) bool {
		return !candidate.End.After(now) || candidate.End.After(deadline)
	})

	// Cheapest first; with equal prices the earlier slot wins.
	slices.SortStableFunc(candidates, func(a, b slot) int {
		return cmp.Or(cmp.Compare(a.Price, b.Price), a.Start.Compare(b.Start))
	})

	var chosen []slot

	need := time.Duration(hours * float64(time.Hour))

	for _, candidate := range candidates {
		if need <= 0 {
			break
		}

		chosen = append(chosen, candidate)
		need -= candidate.End.Sub(later(candidate.Start, now))
	}

	slices.SortFunc(chosen, func(a, b slot) int { return a.Start.Compare(b.Start) })

	return chosen
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// slotAt returns the slot that contains moment, if any.
func slotAt(slots []slot, moment time.Time) (slot, bool) {
	idx := slices.IndexFunc(slots, func(candidate slot) bool {
		return !moment.Before(candidate.Start) && moment.Before(candidate.End)
	})
	if idx < 0 {
		return slot{Start: time.Time{}, End: time.Time{}, Price: 0}, false
	}

	return slots[idx], true
}

// covers reports whether the price data runs up to (at least) moment.
func covers(slots []slot, moment time.Time) bool {
	return slices.ContainsFunc(slots, func(candidate slot) bool { return !candidate.End.Before(moment) })
}

// dayOverrides are calendar exceptions keyed by local date ("2006-01-02"):
// extra charge days with their ready-by time of day, and skipped days.
type dayOverrides struct {
	extra map[string]time.Duration
	skip  map[string]bool
}

// nextDeadline returns the first "ready by" moment strictly after now: on one
// of the given weekdays (e.g. Wednesday 07:00) or an extra calendar day, and
// not on a skipped day.
func nextDeadline(now time.Time, days []time.Weekday, readyBy time.Duration, overrides dayOverrides) (time.Time, bool) {
	const week = 8 // today plus the next 7 days

	for offset := range week {
		day := now.AddDate(0, 0, offset)
		key := day.Format(time.DateOnly)

		readyAt, extra := overrides.extra[key]
		if overrides.skip[key] || (!extra && !slices.Contains(days, day.Weekday())) {
			continue
		}

		if !extra {
			readyAt = readyBy
		}

		// time.Date instead of midnight+duration: stays right on DST switch days.
		deadline := time.Date(day.Year(), day.Month(), day.Day(), 0, int(readyAt/time.Minute), 0, 0, now.Location())
		if deadline.After(now) {
			return deadline, true
		}
	}

	return time.Time{}, false
}
