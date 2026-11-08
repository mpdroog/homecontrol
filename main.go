// Command homecontrol charges the car via a myenergi Zappi in the cheapest
// day-ahead price slots before a deadline (e.g. Wednesday 07:00).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
	_ "time/tzdata" // the Pi may lack zoneinfo

	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/cald"
	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
	"github.com/mpdroog/homecontrol/pushover"
)

const tickEvery = time.Minute

// options are the command line flags.
type options struct {
	configPath string
	write      bool
	verbose    bool
}

func main() {
	var opts options

	flag.StringVar(&opts.configPath, "c", "config.toml", "path to config file")
	flag.BoolVar(&opts.write, "w", false, "act for real: switch the Zappi, wake the car, notify (default: dry-run, only log)")
	flag.BoolVar(&opts.verbose, "v", false, "verbose (debug) logging")
	flag.Parse()

	err := run(opts)
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})) //nolint:exhaustruct // defaults

	cfg, rules, err := loadConfig(opts.configPath)
	if err != nil {
		return err
	}

	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		return fmt.Errorf("loading timezone: %w", err)
	}

	ctrl, err := build(cfg, rules, opts, log)
	if err != nil {
		return err
	}

	ctx := context.Background()

	log.InfoContext(ctx, "started", "days", rules.Days, "ready_by", rules.ReadyBy, "target", rules.TargetPercent, "dry_run", !opts.write)

	if !opts.write {
		log.WarnContext(ctx, "dry-run: not switching the Zappi, waking the car or sending notifications; start with -w to act")
	}

	ticker := time.NewTicker(tickEvery)

	for {
		err := ctrl.Tick(ctx, time.Now().In(loc))
		if err != nil {
			log.ErrorContext(ctx, "tick", "err", err)
		}

		<-ticker.C
	}
}

func build(cfg *config, rules chargeRules, opts options, log *slog.Logger) (*controller, error) {
	var charger Charger = myenergi.New(cfg.Zappi.Hub, cfg.Zappi.Key, cfg.Zappi.Serial)

	var notifier Notifier = logNotifier{log: log}
	if cfg.Pushover != nil {
		notifier = pushover.New(cfg.Pushover.Token, cfg.Pushover.User)
	}

	var car Car

	if cfg.MySkoda != nil {
		client, err := myskoda.New(cfg.MySkoda.Username, cfg.MySkoda.Password, cfg.MySkoda.VIN, log)
		if err != nil {
			return nil, fmt.Errorf("myskoda: %w", err)
		}

		car = client
	}

	var battery HomeBattery
	if cfg.AlphaESS != nil {
		battery = alphaess.New(cfg.AlphaESS.AppID, cfg.AlphaESS.Secret, cfg.AlphaESS.SN)
	}

	if !opts.write {
		charger = dryCharger{Charger: charger, log: log}
		notifier = logNotifier{log: log}

		if car != nil {
			car = dryCar{Car: car, log: log}
		}
	}

	ctrl := newController(rules, charger, car, battery, newPriceCache(nordpool.New(), log), notifier, log)
	if cfg.Cald != nil {
		ctrl.calendar = newCalendarCache(cald.New(cfg.Cald.URL, cfg.Cald.User, cfg.Cald.Pass), rules.ReadyBy)
	}

	return ctrl, nil
}
