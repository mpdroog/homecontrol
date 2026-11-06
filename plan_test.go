package main

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("loading timezone: %v", err)
	}

	return loc
}

// hourly builds one-hour slots starting at start with the given prices.
func hourly(start time.Time, prices ...float64) []slot {
	out := make([]slot, 0, len(prices))
	for i, price := range prices {
		begin := start.Add(time.Duration(i) * time.Hour)
		out = append(out, slot{Start: begin, End: begin.Add(time.Hour), Price: price})
	}

	return out
}

func TestPlanSlotsPicksCheapestBeforeDeadline(t *testing.T) {
	t.Parallel()

	loc := mustLoc(t)
	start := time.Date(2026, 11, 10, 20, 0, 0, 0, loc) // Tue 20:00
	// 20 21 22 23 00 01 02 03 04 05 06 07 08
	slots := hourly(start, 30, 25, 20, 18, 12, 10, 11, 19, 13, 22, 26, 1, 1)
	deadline := time.Date(2026, 11, 11, 7, 0, 0, 0, loc)

	got := planSlots(slots, start, deadline, 3)

	// Cheapest before 07:00 are 01:00 (10), 02:00 (11) and 00:00 (12); the 1.00 at 07:00+ is too late.
	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("got %d slots, want %d: %v", len(got), len(want), got)
	}

	for i, h := range want {
		if got[i].Start.Hour() != h {
			t.Errorf("slot %d starts %s, want %02d:00", i, got[i].Start.Format("15:04"), h)
		}
	}

	for _, s := range got {
		if s.End.After(deadline) {
			t.Errorf("slot %s ends after the deadline", s.Start.Format("15:04"))
		}
	}
}

func TestPlanSlotsCountsOnlyRemainingTimeOfCurrentSlot(t *testing.T) {
	t.Parallel()

	loc := mustLoc(t)
	start := time.Date(2026, 11, 10, 22, 0, 0, 0, loc)
	slots := hourly(start, 1, 2, 3)
	now := start.Add(45 * time.Minute) // only 15 minutes left of the cheapest slot

	got := planSlots(slots, now, start.Add(3*time.Hour), 1)

	if len(got) != 2 {
		t.Fatalf("got %d slots, want 2 (15 min of the current + next): %v", len(got), got)
	}
}

func TestPlanSlotsNothingNeeded(t *testing.T) {
	t.Parallel()

	loc := mustLoc(t)
	start := time.Date(2026, 11, 10, 22, 0, 0, 0, loc)

	if got := planSlots(hourly(start, 1, 2), start, start.Add(2*time.Hour), 0); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestNextDeadline(t *testing.T) {
	t.Parallel()

	loc := mustLoc(t)
	days := []time.Weekday{time.Wednesday, time.Thursday}
	readyBy := 7 * time.Hour

	tests := []struct {
		now  time.Time
		want time.Time
		name string
	}{
		{time.Date(2026, 11, 10, 20, 0, 0, 0, loc), time.Date(2026, 11, 11, 7, 0, 0, 0, loc), "tuesday evening"},
		{time.Date(2026, 11, 11, 6, 59, 0, 0, loc), time.Date(2026, 11, 11, 7, 0, 0, 0, loc), "wednesday just before"},
		{time.Date(2026, 11, 11, 7, 0, 0, 0, loc), time.Date(2026, 11, 12, 7, 0, 0, 0, loc), "wednesday at deadline"},
		{time.Date(2026, 11, 12, 8, 0, 0, 0, loc), time.Date(2026, 11, 18, 7, 0, 0, 0, loc), "thursday after"},
		{time.Date(2027, 3, 27, 12, 0, 0, 0, loc), time.Date(2027, 3, 31, 7, 0, 0, 0, loc), "over DST switch"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := nextDeadline(tc.now, days, readyBy)
			if !ok || !got.Equal(tc.want) {
				t.Fatalf("nextDeadline(%s) = %s, %v; want %s", tc.now, got, ok, tc.want)
			}

			if got.Hour() != 7 {
				t.Fatalf("deadline hour %d, want 7 (local)", got.Hour())
			}
		})
	}
}

func TestNextDeadlineNoDays(t *testing.T) {
	t.Parallel()

	if _, ok := nextDeadline(time.Now(), nil, time.Hour); ok {
		t.Fatal("want no deadline without days")
	}
}
