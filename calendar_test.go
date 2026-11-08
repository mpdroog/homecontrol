package main

import (
	"testing"
	"time"

	"github.com/mpdroog/homecontrol/cald"
)

func TestToOverrides(t *testing.T) {
	t.Parallel()

	ov := toOverrides([]cald.Event{
		{Date: "2026-11-16", StartTime: "06:00", Title: "Schiphol", Tags: []string{"auto"}},
		{Date: "2026-11-17", StartTime: "09:00", Title: "Klant", Tags: []string{"Auto"}},
		{Date: "2026-11-18", StartTime: "", Title: "Vrij", Tags: []string{"geenauto", "vrij"}},
	}, 7*time.Hour)

	if got := ov.extra["2026-11-16"]; got != 6*time.Hour {
		t.Errorf("early start: ready by %s, want 6h", got)
	}

	if got := ov.extra["2026-11-17"]; got != 7*time.Hour {
		t.Errorf("late start: ready by %s, want ready_by 7h", got)
	}

	if !ov.skip["2026-11-18"] || len(ov.extra) != 2 {
		t.Errorf("overrides %+v", ov)
	}
}
