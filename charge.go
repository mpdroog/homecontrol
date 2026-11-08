package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/myenergi"
)

const (
	// chargingW: above this the car is considered to be charging.
	chargingW = 500
	// modeSettle: how long the Zappi may take to report a mode we just set.
	modeSettle = 3 * time.Minute
	// wakeAfter / alarmAfter: car not drawing power while it should.
	wakeAfter  = 10 * time.Minute
	alarmAfter = 20 * time.Minute
	// socInterval: how often we ask the car for its battery level.
	socInterval = 15 * time.Minute
	// socMaxAge: older battery levels are treated as unknown.
	socMaxAge = 2 * time.Hour
	// reminderLead / reminderHour: "car not plugged in" before a deadline.
	reminderLead = 11 * time.Hour
	reminderHour = 20
	// margin on the computed charge time (losses, slower charging near full).
	chargeMargin = 1.15
	// maxTickGap caps energy integration when ticks were missed.
	maxTickGap = 5 * time.Minute
	// zappiDownAfter / skodaDownAfter: notify after failing this long.
	zappiDownAfter = 30 * time.Minute
	skodaDownAfter = 24 * time.Hour
	caldDownAfter  = 6 * time.Hour
	// summaryMinKWh: no summary for sessions below this.
	summaryMinKWh = 0.1
	percent       = 100.0
	wattsPerKW    = 1000.0
)

// Charger is the Zappi.
type Charger interface {
	Status(ctx context.Context) (myenergi.Zappi, error)
	SetMode(ctx context.Context, mode myenergi.Mode) error
}

// Car reports its battery level and can be told to start charging.
type Car interface {
	BatteryPercent(ctx context.Context) (int, error)
	StartCharging(ctx context.Context) error
}

// HomeBattery reports live home battery power.
type HomeBattery interface {
	Power(ctx context.Context) (alphaess.Power, error)
}

// PriceSource returns the known day-ahead price slots.
type PriceSource interface {
	Slots(ctx context.Context, now time.Time) ([]slot, error)
}

// Notifier sends a message to the user.
type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

// session tracks one plug-in → unplug period.
type session struct {
	fastSince    time.Time // since when we want Fast (zero if not)
	zappiKWh     float64   // last "energy added" reported by the Zappi
	chargedKWh   float64   // integrated charge energy
	batteryKWh   float64   // part of it while the home battery discharged
	costEUR      float64   // EPEX cost of the charged energy
	active       bool
	wokeCar      bool
	alarmSent    bool
	overrideSent bool
	noPricesSent bool
}

// outage tracks a source that keeps failing, to notify once (and on recovery).
type outage struct {
	since time.Time // first failure of the current streak (zero = ok)
	sent  bool      // "down" notification sent
}

// controller decides every tick whether the car should charge now and sets
// the Zappi accordingly. It keeps only a little state; a restart is harmless.
type controller struct {
	zappiDown   outage
	skodaDown   outage
	caldDown    outage
	lastSetAt   time.Time
	lastTick    time.Time
	socTriedAt  time.Time
	socOKAt     time.Time
	remindedFor time.Time
	prices      PriceSource
	calendar    Calendar    // nil if not configured
	car         Car         // nil if not configured
	battery     HomeBattery // nil if not configured
	charger     Charger
	notifier    Notifier
	log         *slog.Logger
	sess        session
	rules       chargeRules
	lastSet     myenergi.Mode // 0 = we have not set a mode this session
	soc         int           // car battery %, valid while socKnown
	override    bool          // user changed the Zappi mode: hands off until unplug
}

// newSession returns the state for a fresh plug-in (active) or no car (inactive).
func newSession(active bool) session {
	return session{
		fastSince:    time.Time{},
		zappiKWh:     0,
		chargedKWh:   0,
		batteryKWh:   0,
		costEUR:      0,
		active:       active,
		wokeCar:      false,
		alarmSent:    false,
		overrideSent: false,
		noPricesSent: false,
	}
}

// newController wires a controller. car and battery may be nil.
func newController(rules chargeRules, charger Charger, car Car, battery HomeBattery,
	prices PriceSource, notifier Notifier, log *slog.Logger,
) *controller {
	return &controller{
		zappiDown:   outage{since: time.Time{}, sent: false},
		skodaDown:   outage{since: time.Time{}, sent: false},
		caldDown:    outage{since: time.Time{}, sent: false},
		lastSetAt:   time.Time{},
		lastTick:    time.Time{},
		socTriedAt:  time.Time{},
		socOKAt:     time.Time{},
		remindedFor: time.Time{},
		prices:      prices,
		calendar:    nil,
		car:         car,
		battery:     battery,
		charger:     charger,
		notifier:    notifier,
		log:         log,
		sess:        newSession(false),
		rules:       rules,
		lastSet:     0,
		soc:         0,
		override:    false,
	}
}

// Tick runs one control cycle. All problems are returned (joined); the
// controller still does whatever it safely can.
func (c *controller) Tick(ctx context.Context, now time.Time) error {
	elapsed := c.sinceLastTick(now)

	z, err := c.charger.Status(ctx)
	alertErr := c.track(ctx, now, &c.zappiDown, "Zappi", zappiDownAfter, err)

	if err != nil {
		return errors.Join(fmt.Errorf("zappi status: %w", err), alertErr)
	}

	if !z.Connected() {
		return c.disconnected(ctx, now)
	}

	errs := []error{alertErr}

	if !c.sess.active {
		c.sess = newSession(true)
		c.socTriedAt = time.Time{} // fresh battery level at plug-in
		c.log.InfoContext(ctx, "car plugged in", "mode", z.Mode)
	}

	errs = append(errs, c.refreshSoC(ctx, now), c.account(ctx, now, &z, elapsed))

	if c.detectOverride(ctx, now, &z) {
		return errors.Join(append(errs, c.notifyOverride(ctx))...)
	}

	want, reason, err := c.decide(ctx, now, &z)
	c.log.DebugContext(ctx, "decision", "want", want, "reason", reason, "zappi", z.Mode,
		"charge_w", z.ChargePowerW, "voltage", z.Voltage(), "soc_known", c.socKnown(now), "soc", c.soc)
	errs = append(errs, err, c.apply(ctx, now, &z, want, reason), c.watchFailure(ctx, now, &z, want))

	return errors.Join(errs...)
}

// sinceLastTick returns the time since the previous tick, or 0 when that is
// unknown or implausible (first tick, clock jump, long outage).
func (c *controller) sinceLastTick(now time.Time) time.Duration {
	elapsed := now.Sub(c.lastTick)
	if c.lastTick.IsZero() || elapsed > maxTickGap || elapsed < 0 {
		elapsed = 0
	}

	c.lastTick = now

	return elapsed
}

// apply switches the Zappi to want when needed.
func (c *controller) apply(ctx context.Context, now time.Time, z *myenergi.Zappi, want myenergi.Mode, reason string) error {
	settling := want == c.lastSet && now.Sub(c.lastSetAt) < modeSettle // Zappi still catching up

	if z.Mode == want || settling {
		if c.lastSet == 0 {
			c.lastSet, c.lastSetAt = want, now // adopt the current mode as ours
		}

		return nil
	}

	c.log.InfoContext(ctx, "set zappi mode", "from", z.Mode, "to", want, "reason", reason)

	err := c.charger.SetMode(ctx, want)
	if err != nil {
		return fmt.Errorf("set mode %s: %w", want, err)
	}

	c.lastSet, c.lastSetAt = want, now

	return nil
}

// disconnected handles the car not being plugged in: close the session and
// remind the user if a deadline is coming up.
func (c *controller) disconnected(ctx context.Context, now time.Time) error {
	var errs []error

	if c.sess.active {
		c.log.InfoContext(ctx, "car unplugged", "kwh", c.sessionKWh(), "battery_kwh", c.sess.batteryKWh)
		errs = append(errs, c.sendSummary(ctx))
	}

	c.sess = newSession(false)
	c.lastSet, c.override = 0, false

	deadline, ok, err := c.deadline(ctx, now)
	errs = append(errs, err)

	if !ok || deadline.Sub(now) > reminderLead || now.Hour() < reminderHour || c.remindedFor.Equal(deadline) {
		return errors.Join(errs...)
	}

	errs = append(errs, c.refreshSoC(ctx, now))
	if c.socKnown(now) && c.soc >= c.rules.TargetPercent {
		return errors.Join(errs...)
	}

	c.remindedFor = deadline
	msg := fmt.Sprintf("De auto is niet aangesloten. Hij moet %s om %s op %d%% zijn.",
		dayNL(deadline.Weekday()), deadline.Format("15:04"), c.rules.TargetPercent)

	if c.socKnown(now) {
		msg += fmt.Sprintf(" Nu: %d%%.", c.soc)
	}

	return errors.Join(append(errs, c.notify(ctx, "Auto niet aangesloten", msg))...)
}

// decide returns the mode we want now, with a human readable reason.
func (c *controller) decide(ctx context.Context, now time.Time, z *myenergi.Zappi) (myenergi.Mode, string, error) {
	if z.Status == myenergi.StatusComplete {
		return myenergi.ModeStop, "car reports full", nil
	}

	if c.socKnown(now) && c.soc >= c.rules.TargetPercent {
		return myenergi.ModeStop, fmt.Sprintf("battery %d%% >= target", c.soc), nil
	}

	deadline, ok, calErr := c.deadline(ctx, now)
	if !ok {
		return myenergi.ModeStop, "no charge day in the coming week", calErr
	}

	mode, reason, err := c.decideByPrice(ctx, now, deadline, c.hoursNeeded(now))

	return mode, reason, errors.Join(calErr, err)
}

// deadline returns the next charge deadline: weekly days plus calendar
// overrides. Without (working) calendar it falls back to the weekly days.
func (c *controller) deadline(ctx context.Context, now time.Time) (time.Time, bool, error) {
	var (
		overrides dayOverrides
		err       error
	)

	if c.calendar != nil {
		overrides, err = c.calendar.Overrides(ctx, now)
		err = errors.Join(err, c.track(ctx, now, &c.caldDown, "Agenda", caldDownAfter, err))
	}

	deadline, ok := nextDeadline(now, c.rules.Days, c.rules.ReadyBy, overrides)

	return deadline, ok, err
}

// decideByPrice charges in the cheapest slots that still make the deadline.
func (c *controller) decideByPrice(ctx context.Context, now, deadline time.Time, hours float64) (myenergi.Mode, string, error) {
	slots, err := c.prices.Slots(ctx, now)
	if err != nil {
		mode, reason, notifyErr := c.withoutPrices(ctx, now, deadline, hours)

		return mode, reason, errors.Join(fmt.Errorf("prices: %w", err), notifyErr)
	}

	if cur, ok := slotAt(slots, now); ok && cur.Price <= c.rules.CheapPrice {
		return myenergi.ModeFast, fmt.Sprintf("cheap price %.3f", cur.Price), nil
	}

	if !covers(slots, deadline) {
		return c.withoutPrices(ctx, now, deadline, hours)
	}

	plan := planSlots(slots, now, deadline, hours)
	if _, in := slotAt(plan, now); in {
		return myenergi.ModeFast, fmt.Sprintf("planned slot (%.1fh needed before %s)", hours, deadline.Format("Mon 15:04")), nil
	}

	return myenergi.ModeStop, fmt.Sprintf("not a planned slot (%.1fh needed before %s)", hours, deadline.Format("Mon 15:04")), nil
}

// withoutPrices decides when prices up to the deadline are missing (fetch
// failed or not published): wait, unless waiting longer would miss the deadline.
func (c *controller) withoutPrices(ctx context.Context, now, deadline time.Time, hours float64) (myenergi.Mode, string, error) {
	if deadline.Sub(now) > time.Duration((hours+1)*float64(time.Hour)) {
		return myenergi.ModeStop, "waiting for prices up to " + deadline.Format("Mon 15:04"), nil
	}

	if c.sess.noPricesSent {
		return myenergi.ModeFast, "no prices, deadline close", nil
	}

	c.sess.noPricesSent = true

	return myenergi.ModeFast, "no prices, deadline close", c.notify(ctx, "Laden zonder prijzen",
		fmt.Sprintf("Geen prijzen tot %s, ik laad nu om de deadline te halen.", deadline.Format("15:04")))
}

// hoursNeeded estimates the charge time to reach the target; with an unknown
// battery level it assumes an empty battery.
func (c *controller) hoursNeeded(now time.Time) float64 {
	from := 0
	if c.socKnown(now) {
		from = c.soc
	}

	kwh := float64(c.rules.TargetPercent-from) / percent * c.rules.BatteryKWh

	return kwh / c.rules.ChargeKW * chargeMargin
}

// detectOverride notices when someone changed the Zappi mode by hand.
func (c *controller) detectOverride(ctx context.Context, now time.Time, z *myenergi.Zappi) bool {
	if c.override {
		return true
	}

	if c.lastSet == 0 || z.Mode == c.lastSet || now.Sub(c.lastSetAt) < modeSettle {
		return false
	}

	c.override = true
	c.log.InfoContext(ctx, "manual mode change detected, hands off until unplug", "expected", c.lastSet, "got", z.Mode)

	return true
}

func (c *controller) notifyOverride(ctx context.Context) error {
	if c.sess.overrideSent {
		return nil
	}

	c.sess.overrideSent = true

	return c.notify(ctx, "Slim laden gepauzeerd",
		"De Zappi-stand is handmatig gewijzigd. Slim laden doet niets tot de auto wordt losgekoppeld.")
}

// account integrates charged energy, its cost and the home battery share.
func (c *controller) account(ctx context.Context, now time.Time, z *myenergi.Zappi, elapsed time.Duration) error {
	c.sess.zappiKWh = max(c.sess.zappiKWh, z.ChargeAddedKWh)

	if z.ChargePowerW < chargingW || elapsed == 0 {
		return nil
	}

	kwh := z.ChargePowerW / wattsPerKW * elapsed.Hours()
	c.sess.chargedKWh += kwh

	var errs []error

	slots, err := c.prices.Slots(ctx, now)
	if err != nil {
		errs = append(errs, fmt.Errorf("prices for cost: %w", err))
	} else if cur, ok := slotAt(slots, now); ok {
		c.sess.costEUR += kwh * cur.Price
	}

	if c.battery != nil {
		p, err := c.battery.Power(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("home battery: %w", err))
		} else if p.BatteryW > 0 {
			c.sess.batteryKWh += min(p.BatteryW, z.ChargePowerW) / wattsPerKW * elapsed.Hours()
		}
	}

	return errors.Join(errs...)
}

// watchFailure wakes the car, then alarms, when it does not draw power while
// we want it to charge.
func (c *controller) watchFailure(ctx context.Context, now time.Time, z *myenergi.Zappi, want myenergi.Mode) error {
	if want != myenergi.ModeFast || z.ChargePowerW >= chargingW {
		c.sess.fastSince = time.Time{}

		return nil
	}

	if c.sess.fastSince.IsZero() {
		c.sess.fastSince = now

		return nil
	}

	waited := now.Sub(c.sess.fastSince)

	if waited >= wakeAfter && !c.sess.wokeCar && c.car != nil {
		c.sess.wokeCar = true
		c.log.InfoContext(ctx, "car not charging, asking car to start")

		err := c.car.StartCharging(ctx)
		if err != nil {
			return fmt.Errorf("car start charging: %w", err)
		}
	}

	if waited >= alarmAfter && !c.sess.alarmSent {
		c.sess.alarmSent = true

		return c.notify(ctx, "Auto laadt niet",
			fmt.Sprintf("De Zappi staat al %d minuten op Fast, maar de auto laadt niet. Even kijken?", int(waited.Minutes())))
	}

	return nil
}

func (c *controller) refreshSoC(ctx context.Context, now time.Time) error {
	if c.car == nil || (!c.socTriedAt.IsZero() && now.Sub(c.socTriedAt) < socInterval) {
		return nil
	}

	c.socTriedAt = now

	level, err := c.car.BatteryPercent(ctx)
	alertErr := c.track(ctx, now, &c.skodaDown, "MySkoda", skodaDownAfter, err)

	if err != nil {
		return errors.Join(fmt.Errorf("car battery level: %w", err), alertErr)
	}

	c.soc, c.socOKAt = level, now

	return alertErr
}

// track notifies once when a source has failed for longer than after, and
// once when it works again.
func (c *controller) track(ctx context.Context, now time.Time, state *outage, name string, after time.Duration, err error) error {
	if err == nil {
		wasSent := state.sent
		*state = outage{since: time.Time{}, sent: false}

		if !wasSent {
			return nil
		}

		return c.notify(ctx, name+" werkt weer", name+" geeft weer antwoord.")
	}

	if state.since.IsZero() {
		state.since = now
	}

	if state.sent || now.Sub(state.since) < after {
		return nil
	}

	state.sent = true

	return c.notify(ctx, name+" werkt niet",
		fmt.Sprintf("%s geeft al %s fouten. Laatste: %v", name, now.Sub(state.since).Round(time.Minute), err))
}

// socKnown reports whether a recent battery level is available. Without it
// (no MySkoda, or MySkoda failing) an empty battery is assumed; the Zappi
// reporting "complete" stops the charge.
func (c *controller) socKnown(now time.Time) bool {
	return !c.socOKAt.IsZero() && now.Sub(c.socOKAt) <= socMaxAge
}

func (c *controller) sessionKWh() float64 {
	return max(c.sess.zappiKWh, c.sess.chargedKWh)
}

func (c *controller) sendSummary(ctx context.Context) error {
	kwh := c.sessionKWh()
	if kwh < summaryMinKWh {
		return nil
	}

	msg := fmt.Sprintf("Geladen: %.1f kWh.", kwh)
	if c.sess.chargedKWh > 0 {
		msg += fmt.Sprintf(" Gemiddelde EPEX-prijs: %.1f ct/kWh.", c.sess.costEUR/c.sess.chargedKWh*percent)
	}

	if c.battery != nil {
		msg += fmt.Sprintf(" Uit thuisbatterij: ~%.1f kWh.", c.sess.batteryKWh)
	}

	return c.notify(ctx, "Laadsessie klaar", msg)
}

func (c *controller) notify(ctx context.Context, title, msg string) error {
	err := c.notifier.Notify(ctx, title, msg)
	if err != nil {
		return fmt.Errorf("notify %q: %w", title, err)
	}

	return nil
}

func dayNL(wd time.Weekday) string {
	return [...]string{"zondag", "maandag", "dinsdag", "woensdag", "donderdag", "vrijdag", "zaterdag"}[wd]
}
