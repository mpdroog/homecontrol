package cald

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTagged(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "u" || pass != "p" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if got := r.URL.RawQuery; got != "days=8&from=2026-11-16&tag=auto&tag=geenauto" {
			t.Errorf("query %s", got)
		}

		_, err := w.Write([]byte(`{"events":[{"date":"2026-11-17","startTime":"06:30","title":"Klant","tags":["auto"]}]}`))
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	from := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	events, err := New(srv.URL+"/", "u", "p").Tagged(context.Background(), from, 8, "auto", "geenauto")
	if err != nil {
		t.Fatalf("tagged: %v", err)
	}

	if len(events) != 1 || events[0].Date != "2026-11-17" || events[0].StartTime != "06:30" {
		t.Fatalf("events %+v", events)
	}
}
