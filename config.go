package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// errConfig is returned for invalid configuration.
var errConfig = errors.New("invalid config")

// config is the contents of config.toml.
type config struct {
	AlphaESS *alphaESSConfig `toml:"alphaess"` // optional: measures battery use while charging
	MySkoda  *mySkodaConfig  `toml:"myskoda"`  // optional: car battery level
	Pushover *pushoverConfig `toml:"pushover"` // optional: notifications
	Zappi    zappiConfig     `toml:"zappi"`
	Charge   chargeConfig    `toml:"charge"`
}

// zappiConfig holds the myenergi credentials.
type zappiConfig struct {
	Hub    string `toml:"hub"`    // hub or vHub serial
	Key    string `toml:"key"`    // API key
	Serial string `toml:"serial"` // Zappi serial
}

// alphaESSConfig holds the AlphaESS Open API credentials.
type alphaESSConfig struct {
	AppID  string `toml:"app_id"`
	Secret string `toml:"secret"`
	SN     string `toml:"sn"`
}

// mySkodaConfig holds the MySkoda login.
type mySkodaConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
	VIN      string `toml:"vin"`
}

// pushoverConfig holds the Pushover keys.
type pushoverConfig struct {
	Token string `toml:"token"`
	User  string `toml:"user"`
}

// chargeConfig describes when the car must be charged.
type chargeConfig struct {
	ReadyBy       string   `toml:"ready_by"` // "07:00"
	Days          []string `toml:"days"`     // ["Wednesday", "Thursday"]
	TargetPercent int      `toml:"target_percent"`
	BatteryKWh    float64  `toml:"battery_kwh"` // usable capacity of the car battery
	ChargeKW      float64  `toml:"charge_kw"`   // what the car actually draws
	CheapPrice    float64  `toml:"cheap_price"` // EUR/kWh (EPEX): always charge at or below
}

// chargeRules is the validated, parsed form of chargeConfig.
type chargeRules struct {
	Days          []time.Weekday
	ReadyBy       time.Duration // since midnight
	TargetPercent int
	BatteryKWh    float64
	ChargeKW      float64
	CheapPrice    float64
}

const maxPercent = 100

// loadConfig reads and validates the config file. Unknown keys are an error,
// so typos do not silently fall back to defaults.
func loadConfig(path string) (*config, chargeRules, error) {
	var cfg config

	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, chargeRules{}, fmt.Errorf("reading %s: %w", path, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}

		return nil, chargeRules{}, fmt.Errorf("%w: unknown keys in %s: %s", errConfig, path, strings.Join(keys, ", "))
	}

	err = cfg.validate()
	if err != nil {
		return nil, chargeRules{}, fmt.Errorf("%s: %w", path, err)
	}

	rules, err := cfg.Charge.rules()
	if err != nil {
		return nil, chargeRules{}, fmt.Errorf("%s: %w", path, err)
	}

	return &cfg, rules, nil
}

func (c *config) validate() error {
	var errs []error

	need := func(section, key, value string) {
		if value == "" {
			errs = append(errs, fmt.Errorf("%w: [%s] %s is required", errConfig, section, key))
		}
	}

	need("zappi", "hub", c.Zappi.Hub)
	need("zappi", "key", c.Zappi.Key)
	need("zappi", "serial", c.Zappi.Serial)

	if c.AlphaESS != nil {
		need("alphaess", "app_id", c.AlphaESS.AppID)
		need("alphaess", "secret", c.AlphaESS.Secret)
		need("alphaess", "sn", c.AlphaESS.SN)
	}

	if c.MySkoda != nil {
		need("myskoda", "username", c.MySkoda.Username)
		need("myskoda", "password", c.MySkoda.Password)
		need("myskoda", "vin", c.MySkoda.VIN)
	}

	if c.Pushover != nil {
		need("pushover", "token", c.Pushover.Token)
		need("pushover", "user", c.Pushover.User)
	}

	return errors.Join(errs...)
}

func (c *chargeConfig) rules() (chargeRules, error) {
	var errs []error

	days := make([]time.Weekday, 0, len(c.Days))

	for _, name := range c.Days {
		wd, ok := parseWeekday(name)
		if !ok {
			errs = append(errs, fmt.Errorf("%w: [charge] days: unknown day %q", errConfig, name))

			continue
		}

		days = append(days, wd)
	}

	readyBy, err := time.Parse("15:04", c.ReadyBy)
	if err != nil {
		errs = append(errs, fmt.Errorf("%w: [charge] ready_by %q: %w", errConfig, c.ReadyBy, err))
	}

	if c.TargetPercent <= 0 || c.TargetPercent > maxPercent {
		errs = append(errs, fmt.Errorf("%w: [charge] target_percent must be 1..100", errConfig))
	}

	if c.BatteryKWh <= 0 {
		errs = append(errs, fmt.Errorf("%w: [charge] battery_kwh must be > 0", errConfig))
	}

	if c.ChargeKW <= 0 {
		errs = append(errs, fmt.Errorf("%w: [charge] charge_kw must be > 0", errConfig))
	}

	joined := errors.Join(errs...)
	if joined != nil {
		return chargeRules{}, joined
	}

	return chargeRules{
		Days:          days,
		ReadyBy:       time.Duration(readyBy.Hour())*time.Hour + time.Duration(readyBy.Minute())*time.Minute,
		TargetPercent: c.TargetPercent,
		BatteryKWh:    c.BatteryKWh,
		ChargeKW:      c.ChargeKW,
		CheapPrice:    c.CheapPrice,
	}, nil
}

func parseWeekday(name string) (time.Weekday, bool) {
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		if strings.EqualFold(wd.String(), name) {
			return wd, true
		}
	}

	return time.Sunday, false
}
