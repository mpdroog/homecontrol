// Command homecontrol charges the car via a myenergi Zappi in the cheapest
// day-ahead price slots before a deadline (e.g. Wednesday 07:00).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // the Pi may lack zoneinfo

	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
	"github.com/mpdroog/homecontrol/pushover"
)

const tickEvery = time.Minute

// options are the command line flags.
type options struct {
	configPath string
	dryRun     bool
	once       bool
	debug      bool
}

func main() {
	var opts options

	flag.StringVar(&opts.configPath, "config", "config.toml", "path to config file")
	flag.BoolVar(&opts.dryRun, "dry-run", false, "log decisions, do not switch the Zappi, wake the car or send notifications")
	flag.BoolVar(&opts.once, "once", false, "run a single tick and exit")
	flag.BoolVar(&opts.debug, "debug", false, "debug logging")
	flag.Parse()

	err := run(opts)
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	level := slog.LevelInfo
	if opts.debug {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.InfoContext(ctx, "started", "days", rules.Days, "ready_by", rules.ReadyBy, "target", rules.TargetPercent, "dry_run", opts.dryRun)

	tick := func() {
		err := ctrl.Tick(ctx, time.Now().In(loc))
		if err != nil {
			log.ErrorContext(ctx, "tick", "err", err)
		}
	}

	tick()

	if opts.once {
		return nil
	}

	return loop(ctx, tick, log)
}

// loop calls tick every tickEvery until ctx is cancelled (SIGINT/SIGTERM).
func loop(ctx context.Context, tick func(), log *slog.Logger) error {
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "stopping")

			err := ctx.Err()
			if !errors.Is(err, context.Canceled) {
				return fmt.Errorf("context: %w", err)
			}

			return nil
		case <-ticker.C:
			tick()
		}
	}
}

func build(cfg *config, rules chargeRules, opts options, log *slog.Logger) (*controller, error) {
	var charger Charger = &zappi{client: myenergi.New(cfg.Zappi.Hub, cfg.Zappi.Key), serial: cfg.Zappi.Serial}

	var notifier Notifier = logNotifier{log: log}
	if cfg.Pushover != nil {
		notifier = pushoverNotifier{client: pushover.New(cfg.Pushover.Token, cfg.Pushover.User)}
	}

	var car Car

	if cfg.MySkoda != nil {
		client, err := myskoda.New(cfg.MySkoda.Username, cfg.MySkoda.Password, log)
		if err != nil {
			return nil, fmt.Errorf("myskoda: %w", err)
		}

		car = &skoda{client: client, vin: cfg.MySkoda.VIN}
	}

	var battery HomeBattery
	if cfg.AlphaESS != nil {
		battery = alphaess.New(cfg.AlphaESS.AppID, cfg.AlphaESS.Secret, cfg.AlphaESS.SN)
	}

	if opts.dryRun {
		charger = dryCharger{Charger: charger, log: log}
		notifier = logNotifier{log: log}

		if car != nil {
			car = dryCar{Car: car, log: log}
		}
	}

	return newController(rules, charger, car, battery, newPriceCache(nordpool.New(), log), notifier, log), nil
}

// zappi binds the myenergi client to one Zappi serial.
type zappi struct {
	client *myenergi.Client
	serial string
}

func (z *zappi) Status(ctx context.Context) (myenergi.Zappi, error) {
	s, err := z.client.Zappi(ctx, z.serial)
	if err != nil {
		return myenergi.Zappi{}, fmt.Errorf("zappi %s: %w", z.serial, err)
	}

	return s, nil
}

func (z *zappi) SetMode(ctx context.Context, mode myenergi.Mode) error {
	err := z.client.SetMode(ctx, z.serial, mode)
	if err != nil {
		return fmt.Errorf("zappi %s: %w", z.serial, err)
	}

	return nil
}

// skoda binds the MySkoda client to one VIN.
type skoda struct {
	client *myskoda.Client
	vin    string
}

func (s *skoda) BatteryPercent(ctx context.Context) (int, error) {
	p, err := s.client.BatteryPercent(ctx, s.vin)
	if err != nil {
		return 0, fmt.Errorf("skoda: %w", err)
	}

	return p, nil
}

func (s *skoda) StartCharging(ctx context.Context) error {
	err := s.client.StartCharging(ctx, s.vin)
	if err != nil {
		return fmt.Errorf("skoda: %w", err)
	}

	return nil
}

type pushoverNotifier struct {
	client *pushover.Client
}

func (p pushoverNotifier) Notify(ctx context.Context, title, message string) error {
	err := p.client.Send(ctx, title, message)
	if err != nil {
		return fmt.Errorf("pushover: %w", err)
	}

	return nil
}

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
