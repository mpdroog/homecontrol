package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const validConfig = `
[zappi]
hub = "h"
key = "k"
serial = "1"

[charge]
days = ["Wednesday", "thursday"]
ready_by = "07:30"
target_percent = 90
battery_kwh = 32.3
charge_kw = 3.4
cheap_price = 0.0
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	cfg, rules, err := loadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.MySkoda != nil || cfg.Pushover != nil || cfg.AlphaESS != nil {
		t.Error("optional sections set while absent")
	}

	if !slices.Equal(rules.Days, []time.Weekday{time.Wednesday, time.Thursday}) {
		t.Errorf("days %v", rules.Days)
	}

	if rules.ReadyBy != 7*time.Hour+30*time.Minute {
		t.Errorf("ready_by %s, want 7h30m", rules.ReadyBy)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, replace, with, want string
	}{
		{"unknown key", "cheap_price", "cheap_prise", "unknown keys"},
		{"unknown day", `"thursday"`, `"donderdag"`, "unknown day"},
		{"bad time", `"07:30"`, `"7u30"`, "ready_by"},
		{"target", "target_percent = 90", "target_percent = 120", "target_percent"},
		{"missing key", `key = "k"`, `key = ""`, "[zappi] key is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := loadConfig(writeConfig(t, strings.Replace(validConfig, tc.replace, tc.with, 1)))
			if !errors.Is(err, errConfig) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want errConfig mentioning %q", err, tc.want)
			}
		})
	}
}
