package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/myenergi"
)

var errFake = errors.New("fake failure")

type fakeCharger struct {
	statusErr error
	setErr    error
	sets      []myenergi.Mode
	status    myenergi.Zappi
}

func (f *fakeCharger) Status(context.Context) (myenergi.Zappi, error) { return f.status, f.statusErr }

func (f *fakeCharger) SetMode(_ context.Context, mode myenergi.Mode) error {
	if f.setErr != nil {
		return f.setErr
	}

	f.sets = append(f.sets, mode)
	f.status.Mode = mode

	return nil
}

type fakeCar struct {
	err     error
	percent int
	starts  int
}

func (f *fakeCar) BatteryPercent(context.Context) (int, error) { return f.percent, f.err }

func (f *fakeCar) StartCharging(context.Context) error {
	f.starts++

	return nil
}

type fakeBattery struct {
	power alphaess.Power
}

func (f *fakeBattery) Power(context.Context) (alphaess.Power, error) { return f.power, nil }

type fakePrices struct {
	err   error
	slots []slot
}

func (f *fakePrices) Slots(context.Context, time.Time) ([]slot, error) { return f.slots, f.err }

type fakeNotifier struct {
	titles []string
	msgs   []string
}

func (f *fakeNotifier) Notify(_ context.Context, title, message string) error {
	f.titles = append(f.titles, title)
	f.msgs = append(f.msgs, message)

	return nil
}

type rig struct {
	ctrl     *controller
	charger  *fakeCharger
	car      *fakeCar
	battery  *fakeBattery
	prices   *fakePrices
	notifier *fakeNotifier
	loc      *time.Location
}

// newRig: Wednesday 07:00 deadline, 90% target, 32 kWh car at 3.2 kW (so
// 10 h to charge from empty). Prices: Tuesday 18:00 until Wednesday 09:00,
// cheapest between 01:00 and 05:00.
func newRig(t *testing.T) *rig {
	t.Helper()

	loc := mustLoc(t)
	start := time.Date(2026, 11, 10, 18, 0, 0, 0, loc) // Tuesday
	// 18 19 20 21 22 23 00 01 02 03 04 05 06 07 08
	prices := hourly(start, 0.30, 0.29, 0.28, 0.27, 0.26, 0.25, 0.15, 0.05, 0.04, 0.03, 0.06, 0.16, 0.24, 0.01, 0.01)

	r := &rig{
		charger: &fakeCharger{statusErr: nil, setErr: nil, sets: nil, status: myenergi.Zappi{
			Plug: "B1", Serial: 1, Mode: myenergi.ModeStop, Status: myenergi.StatusPaused,
			ChargeAddedKWh: 0, ChargePowerW: 0, VoltageDeciV: 2300, FrequencyHz: 50,
		}},
		car:      &fakeCar{err: nil, percent: 70, starts: 0},
		battery:  &fakeBattery{power: alphaess.Power{SOC: 50, BatteryW: 0, GridW: 0, LoadW: 0}},
		prices:   &fakePrices{err: nil, slots: prices},
		notifier: &fakeNotifier{titles: nil, msgs: nil},
		loc:      loc,
		ctrl:     nil,
	}

	rules := chargeRules{
		Days:          []time.Weekday{time.Wednesday},
		ReadyBy:       7 * time.Hour,
		TargetPercent: 90,
		BatteryKWh:    32,
		ChargeKW:      3.2,
		CheapPrice:    0,
	}
	log := slog.New(slog.DiscardHandler)
	r.ctrl = newController(rules, r.charger, r.car, r.battery, r.prices, r.notifier, log)

	return r
}

func (r *rig) at(hour, minute int) time.Time {
	day := 10
	if hour < 12 {
		day = 11
	}

	return time.Date(2026, 11, day, hour, minute, 0, 0, r.loc)
}

func (r *rig) tick(t *testing.T, now time.Time) {
	t.Helper()

	err := r.ctrl.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick %s: %v", now.Format("Mon 15:04"), err)
	}
}

func (r *rig) lastSet(t *testing.T) myenergi.Mode {
	t.Helper()

	if len(r.charger.sets) == 0 {
		t.Fatal("no mode was set")
	}

	return r.charger.sets[len(r.charger.sets)-1]
}

func TestStopsOutsidePlannedSlot(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Mode = myenergi.ModeEcoPlus // the old problem mode

	r.tick(t, r.at(21, 0))

	if got := r.lastSet(t); got != myenergi.ModeStop {
		t.Fatalf("mode %s at 21:00, want Stop", got)
	}
}

func TestChargesInCheapestSlot(t *testing.T) {
	t.Parallel()

	r := newRig(t)

	// 70% → 90% of 32 kWh at 3.2 kW = 2 h (+15% margin): 03:00 and 02:00 suffice,
	// the 3rd cheapest (01:00) covers the margin.
	r.tick(t, r.at(3, 0))

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s at 03:00, want Fast", got)
	}
}

func TestWaitsForCheaperSlotLater(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.tick(t, r.at(22, 0))

	if len(r.charger.sets) != 0 {
		t.Fatalf("mode set to %v at 22:00, want untouched (already Stop, cheaper at night)", r.charger.sets)
	}
}

func TestCatchesUpWhenBehindSchedule(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.tick(t, r.at(5, 30)) // still 70%: not enough time left to be picky

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s at 05:30 while behind schedule, want Fast", got)
	}
}

func TestStopsAtTarget(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Mode = myenergi.ModeFast
	r.car.percent = 90

	r.tick(t, r.at(3, 0))

	if got := r.lastSet(t); got != myenergi.ModeStop {
		t.Fatalf("mode %s at target, want Stop", got)
	}
}

func TestChargesAtCheapPriceOutsidePlan(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.prices.slots[4].Price = -0.02 // 22:00: negative, at or below cheap_price (0)

	r.tick(t, r.at(22, 0))

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s at a negative price, want Fast", got)
	}
}

func TestWaitsWhenPricesDoNotReachDeadline(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Mode = myenergi.ModeFast
	r.prices.slots = r.prices.slots[:6] // only until Tuesday midnight

	r.tick(t, r.at(19, 0))

	if got := r.lastSet(t); got != myenergi.ModeStop {
		t.Fatalf("mode %s without prices up to the deadline, want Stop", got)
	}
}

func TestChargesWithoutTomorrowPricesWhenDeadlineClose(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.prices.slots = r.prices.slots[:6] // tomorrow never published / fetch failed

	r.tick(t, r.at(4, 0)) // 70% → 90% needs ~2.3 h, 3 h left
	r.tick(t, r.at(4, 1))

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s without prices close to the deadline, want Fast", got)
	}

	if len(r.notifier.titles) != 1 || r.notifier.titles[0] != "Laden zonder prijzen" {
		t.Fatalf("notifications %v, want one no-prices notice", r.notifier.titles)
	}
}

func TestZappiDownNotifiesOnceAndOnRecovery(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.statusErr = errFake

	for minute := range 40 {
		err := r.ctrl.Tick(context.Background(), r.at(21, minute))
		if !errors.Is(err, errFake) {
			t.Fatalf("tick: %v, want the Zappi error", err)
		}
	}

	r.charger.statusErr = nil
	r.tick(t, r.at(21, 40))

	want := []string{"Zappi werkt niet", "Zappi werkt weer"}
	if !slices.Equal(r.notifier.titles, want) {
		t.Fatalf("notifications %v, want %v", r.notifier.titles, want)
	}
}

func TestMySkodaDownNotifiesAfterADay(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.car.err = errFake
	start := r.at(18, 0)

	for step := range 4 * 25 { // every 15 minutes for 25 hours
		err := r.ctrl.Tick(context.Background(), start.Add(time.Duration(step)*15*time.Minute))
		if err != nil && !errors.Is(err, errFake) {
			t.Fatalf("tick: %v", err)
		}
	}

	if n := slices.Index(r.notifier.titles, "MySkoda werkt niet"); n < 0 {
		t.Fatalf("notifications %v, want a MySkoda notice", r.notifier.titles)
	}

	if strings.Count(strings.Join(r.notifier.titles, "|"), "MySkoda") != 1 {
		t.Fatalf("notifications %v, want exactly one MySkoda notice", r.notifier.titles)
	}
}

type fakeCalendar struct {
	ov dayOverrides
}

func (f *fakeCalendar) Overrides(context.Context, time.Time) (dayOverrides, error) { return f.ov, nil }

func TestSkippedDayFromCalendar(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.ctrl.calendar = &fakeCalendar{ov: dayOverrides{extra: nil, skip: map[string]bool{"2026-11-11": true}}}

	r.tick(t, r.at(3, 0)) // normally a planned slot for Wednesday 07:00

	if len(r.charger.sets) != 0 {
		t.Fatalf("mode set %v on a skipped Wednesday, want untouched (Stop)", r.charger.sets)
	}

	r.charger.status.Plug = "A"
	r.tick(t, r.at(20, 0)) // Tuesday 20:00: no reminder, next deadline is Thursday

	if len(r.notifier.titles) != 0 {
		t.Fatalf("notifications %v, want none", r.notifier.titles)
	}
}

func TestPriceErrorFarFromDeadlineStopsAndReports(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Mode = myenergi.ModeFast
	r.prices.err = errFake

	err := r.ctrl.Tick(context.Background(), r.at(19, 0))
	if !errors.Is(err, errFake) {
		t.Fatalf("error %v, want the price error", err)
	}

	if got := r.lastSet(t); got != myenergi.ModeStop {
		t.Fatalf("mode %s, want Stop", got)
	}
}

func TestPriceErrorCloseToDeadlineCharges(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.prices.err = errFake

	err := r.ctrl.Tick(context.Background(), r.at(5, 0))
	if !errors.Is(err, errFake) {
		t.Fatalf("error %v, want the price error", err)
	}

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s, want Fast", got)
	}
}

func TestManualOverrideIsRespected(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.tick(t, r.at(3, 0)) // we set Fast

	r.charger.status.Mode = myenergi.ModeStop // user stops it in the app
	r.tick(t, r.at(3, 10))
	r.tick(t, r.at(3, 11))

	if len(r.charger.sets) != 1 {
		t.Fatalf("mode set %v times after manual change, want only the first Fast", r.charger.sets)
	}

	if len(r.notifier.titles) != 1 || !strings.Contains(r.notifier.titles[0], "gepauzeerd") {
		t.Fatalf("notifications %v, want one pause notice", r.notifier.titles)
	}

	// Unplug and plug in again: control resumes.
	r.charger.status.Plug = "A"
	r.tick(t, r.at(3, 20))
	r.charger.status.Plug = "B1"
	r.tick(t, r.at(3, 21))

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s after re-plug, want Fast", got)
	}
}

func TestDoesNotRepeatSetWhileZappiCatchesUp(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.tick(t, r.at(3, 0))

	r.charger.status.Mode = myenergi.ModeStop // Zappi still reports the old mode
	r.tick(t, r.at(3, 1))

	if len(r.charger.sets) != 1 {
		t.Fatalf("mode set %v, want one call while settling", r.charger.sets)
	}
}

func TestFailureWakesCarThenAlarmsOnce(t *testing.T) {
	t.Parallel()

	r := newRig(t)

	for minute := range 26 {
		r.tick(t, r.at(2, minute)) // Fast wanted, car draws nothing
	}

	if r.car.starts != 1 {
		t.Fatalf("car start requests %d, want 1", r.car.starts)
	}

	if len(r.notifier.titles) != 1 || r.notifier.titles[0] != "Auto laadt niet" {
		t.Fatalf("notifications %v, want one alarm", r.notifier.titles)
	}
}

func TestCarFullIsNotAFailure(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Mode = myenergi.ModeFast
	r.charger.status.Status = myenergi.StatusComplete

	for minute := range 30 {
		r.tick(t, r.at(2, minute))
	}

	if len(r.notifier.titles) != 0 {
		t.Fatalf("notifications %v, want none when the car is full", r.notifier.titles)
	}

	if got := r.lastSet(t); got != myenergi.ModeStop {
		t.Fatalf("mode %s with a full car, want Stop", got)
	}
}

func TestUnknownBatteryLevelChargesAsIfEmpty(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.car.err = errFake // MySkoda down

	// 70% would wait for the night, but "empty" needs ~10.4 h and only 9 h are left: charge now.
	err := r.ctrl.Tick(context.Background(), r.at(22, 0))
	if !errors.Is(err, errFake) {
		t.Fatalf("error %v, want the MySkoda error reported", err)
	}

	if got := r.lastSet(t); got != myenergi.ModeFast {
		t.Fatalf("mode %s with unknown battery level, want Fast", got)
	}

	if len(r.notifier.titles) != 0 {
		t.Fatalf("notifications %v, want none", r.notifier.titles)
	}
}

func TestSummaryCountsHomeBatteryShare(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.ChargePowerW = 3000
	r.battery.power.BatteryW = 1000 // battery delivers 1 kW of the 3 kW

	for minute := range 61 {
		r.tick(t, r.at(2, minute))
	}

	r.charger.status.Plug = "A"
	r.tick(t, r.at(4, 0))

	if len(r.notifier.msgs) != 1 {
		t.Fatalf("notifications %v, want one summary", r.notifier.titles)
	}

	msg := r.notifier.msgs[0]
	for _, want := range []string{"Geladen: 3.0 kWh", "Uit thuisbatterij: ~1.0 kWh", "4.0 ct/kWh"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary %q lacks %q", msg, want)
		}
	}
}

func TestReminderWhenNotPluggedIn(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Plug = "A"

	r.tick(t, r.at(19, 0)) // too early
	r.tick(t, r.at(20, 0))
	r.tick(t, r.at(21, 0)) // only once

	if len(r.notifier.titles) != 1 || r.notifier.titles[0] != "Auto niet aangesloten" {
		t.Fatalf("notifications %v, want one reminder", r.notifier.titles)
	}

	if !strings.Contains(r.notifier.msgs[0], "woensdag om 07:00 op 90%") {
		t.Errorf("reminder %q lacks the deadline", r.notifier.msgs[0])
	}
}

func TestNoReminderWhenCarIsFull(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.status.Plug = "A"
	r.car.percent = 95

	r.tick(t, r.at(20, 0))

	if len(r.notifier.titles) != 0 {
		t.Fatalf("notifications %v, want none", r.notifier.titles)
	}
}

func TestSetModeErrorIsReturned(t *testing.T) {
	t.Parallel()

	r := newRig(t)
	r.charger.setErr = errFake

	err := r.ctrl.Tick(context.Background(), r.at(3, 0))
	if !errors.Is(err, errFake) {
		t.Fatalf("error %v, want set mode error", err)
	}
}
