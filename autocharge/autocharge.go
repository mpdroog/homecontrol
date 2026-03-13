// Package autocharge provides automated EV charging based on electricity prices.
package autocharge

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
	"github.com/mpdroog/homecontrol/pushover"
)

// State represents the current state of the charging scheduler.
type State int

const (
	StateIdle        State = iota // Not actively managing charging
	StateWaitingForCar            // Waiting for car to connect
	StateScheduled                // Charging session is scheduled
	StateStartingCharge                 // Actively charging
	StateMonitoring               // Monitoring charging progress
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateWaitingForCar:
		return "WaitingForCar"
	case StateScheduled:
		return "Scheduled"
	case StateStartingCharge:
		return "StartingCharge"
	case StateMonitoring:
		return "Monitoring"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// ChargingWindow represents a scheduled charging window.
type ChargingWindow struct {
	Start       time.Time
	End         time.Time
	IsFallback  bool    // True if using fallback hours (no prices available)
	AvgPrice    float64 // Average price during window (EUR/MWh)
}

// Session tracks the current charging session.
type Session struct {
	Window           *ChargingWindow
	StartedAt        time.Time
	ChargeAddedStart float64 // kWh at session start
	ChargeAddedEnd   float64 // kWh at session end (for summary)
	TotalCost        float64 // Estimated cost
	ChargingStarted  bool
	SkodaWakeupSent  bool
	FailureNotified  bool
}

// SkodaClientFunc is a function that returns the Skoda client (for lazy init/re-login).
type SkodaClientFunc func() (*myskoda.Client, error)

// Scheduler manages automated EV charging.
type Scheduler struct {
	// Config values
	zappiSerial  string
	skodaVIN     string
	energyMarkup float64

	enabled bool
	state   State
	session *Session

	// Clients (passed in from main app)
	zappiClient    *myenergi.Client
	nordpoolClient *nordpool.Client
	pushClient     *pushover.Client
	getSkodaClient SkodaClientFunc

	// State tracking
	lastCarConnected  bool
	lastPrices        *nordpool.Prices
	lastPricesFetched time.Time
	scheduledWindow   *ChargingWindow

	// Timing
	chargingStartedAt     time.Time
	lastChargingCheckAt   time.Time
	chargingNotStartedFor time.Duration

	// Synchronization
	mu       sync.RWMutex
	stopChan chan struct{}
	wg       sync.WaitGroup

	debug bool
}

// NewScheduler creates a new autocharge scheduler using existing clients.
func NewScheduler(
	zappiClient *myenergi.Client,
	nordpoolClient *nordpool.Client,
	pushClient *pushover.Client,
	getSkodaClient SkodaClientFunc,
	zappiSerial string,
	skodaVIN string,
	energyMarkup float64,
) *Scheduler {
	return &Scheduler{
		zappiSerial:    zappiSerial,
		skodaVIN:       skodaVIN,
		energyMarkup:   energyMarkup,
		enabled:        true,
		state:          StateIdle,
		stopChan:       make(chan struct{}),
		zappiClient:    zappiClient,
		nordpoolClient: nordpoolClient,
		pushClient:     pushClient,
		getSkodaClient: getSkodaClient,
	}
}

// SetDebug enables debug logging.
func (s *Scheduler) SetDebug(debug bool) {
	s.debug = debug
}

// Run starts the scheduler main loop.
func (s *Scheduler) Run() error {
	s.log("Starting autocharge scheduler")

	// Main polling ticker (every 10 minutes)
	pollTicker := time.NewTicker(10 * time.Minute)
	defer pollTicker.Stop()

	// Minute ticker for time-based events
	minuteTicker := time.NewTicker(1 * time.Minute)
	defer minuteTicker.Stop()

	s.wg.Add(1)
	defer s.wg.Done()

	// Initial poll
	s.poll()

	for {
		select {
		case <-s.stopChan:
			s.log("Scheduler stopped")
			return nil

		case <-pollTicker.C:
			s.poll()

		case <-minuteTicker.C:
			s.checkScheduledEvents()
		}
	}
}

// Stop stops the scheduler.
func (s *Scheduler) Stop() {
	close(s.stopChan)
	s.wg.Wait()
}

// Enable enables the scheduler.
func (s *Scheduler) Enable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = true
	s.log("Scheduler enabled")
}

// Disable disables the scheduler (current session continues).
func (s *Scheduler) Disable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = false
	s.log("Scheduler disabled (current session continues)")
}

// IsEnabled returns whether the scheduler is enabled.
func (s *Scheduler) IsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// GetState returns the current scheduler state.
func (s *Scheduler) GetState() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// GetStatus returns a status summary.
func (s *Scheduler) GetStatus() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	status := map[string]interface{}{
		"enabled":      s.enabled,
		"state":        s.state.String(),
		"carConnected": s.lastCarConnected,
	}

	if s.scheduledWindow != nil {
		status["scheduledWindow"] = map[string]interface{}{
			"start":      s.scheduledWindow.Start.Format("15:04"),
			"end":        s.scheduledWindow.End.Format("15:04"),
			"isFallback": s.scheduledWindow.IsFallback,
			"avgPrice":   s.scheduledWindow.AvgPrice,
		}
	}

	if s.session != nil {
		status["session"] = map[string]interface{}{
			"startedAt":       s.session.StartedAt.Format(time.RFC3339),
			"chargingStarted": s.session.ChargingStarted,
		}
	}

	return status
}

// poll checks Zappi status and handles state transitions.
func (s *Scheduler) poll() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled && s.state != StateStartingCharge && s.state != StateMonitoring {
		return
	}

	// Get Zappi status
	zappis, err := s.zappiClient.GetZappiStatus()
	if err != nil {
		s.log("Error getting Zappi status: %v", err)
		return
	}

	if len(zappis) == 0 {
		s.log("No Zappi devices found")
		return
	}

	zappi := s.findZappi(zappis)
	if zappi == nil {
		s.log("Configured Zappi %s not found", s.zappiSerial)
		return
	}

	// Check car connection status
	carConnected := s.isCarConnected(zappi)
	wasConnected := s.lastCarConnected
	s.lastCarConnected = carConnected

	// Handle re-attachment
	if carConnected && !wasConnected {
		s.handleCarReattached()
	}

	// Handle disconnection
	if !carConnected && wasConnected {
		s.handleCarDisconnected()
	}

	// State machine processing
	switch s.state {
	case StateScheduled:
		s.processScheduledState(zappi)
	case StateStartingCharge:
		s.processStartingChargeState(zappi)
	case StateMonitoring:
		s.processMonitoringState(zappi)
	}
}

// checkScheduledEvents checks for time-based events (19:00, 20:00, 06:00, etc.)
func (s *Scheduler) checkScheduledEvents() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled {
		return
	}

	loc, _ := time.LoadLocation("Europe/Amsterdam")
	now := time.Now().In(loc)

	// Check if we need to fetch new prices (around 13:00)
	if now.Hour() == 13 && now.Minute() == 5 {
		s.fetchPrices()
	}

	// 19:00 - Announce cheapest hours
	if now.Hour() == 19 && now.Minute() == 0 {
		s.handleAnnounceTime()
	}

	// 20:00 - Warn if car not connected
	if now.Hour() == 20 && now.Minute() == 0 {
		s.handleCarNotConnectedWarning()
	}

	// 06:00 - Morning summary (Tue-Fri)
	if now.Hour() == 6 && now.Minute() == 0 {
		s.handleMorningSummary(now)
	}

	// Check if we're entering the charging window
	if s.scheduledWindow != nil && s.state == StateScheduled {
		if now.After(s.scheduledWindow.Start) || now.Equal(s.scheduledWindow.Start) {
			if now.Before(s.scheduledWindow.End) {
				s.startChargingSession()
			}
		}
	}

	// Check if charging window has ended
	if s.scheduledWindow != nil && (s.state == StateStartingCharge || s.state == StateMonitoring) {
		if now.After(s.scheduledWindow.End) || now.Equal(s.scheduledWindow.End) {
			s.endChargingSession()
		}
	}
}

// findZappi finds the configured Zappi in the list.
func (s *Scheduler) findZappi(zappis []myenergi.Zappi) *myenergi.Zappi {
	for i, z := range zappis {
		if fmt.Sprintf("%d", z.Serial) == s.zappiSerial {
			return &zappis[i]
		}
	}
	// Return first Zappi if no serial configured
	if s.zappiSerial == "" && len(zappis) > 0 {
		return &zappis[0]
	}
	return nil
}

// isCarConnected checks if a car is connected to the Zappi.
func (s *Scheduler) isCarConnected(zappi *myenergi.Zappi) bool {
	// PlugStatus: A=Disconnected, B1/B2/C1/C2=Connected in various states
	return zappi.PlugStatus != "A"
}

// isCarCharging checks if the car is currently charging.
func (s *Scheduler) isCarCharging(zappi *myenergi.Zappi) bool {
	return zappi.Status == myenergi.ZappiStatusCharging ||
	       zappi.Status == myenergi.ZappiStatusFastCharging
}

// handleCarReattached handles when the car is reconnected.
func (s *Scheduler) handleCarReattached() {
	s.log("Car connected to Zappi")

	// Re-calculate cheapest hours
	window := s.calculateCheapestWindow()
	if window != nil {
		s.scheduledWindow = window
		s.state = StateScheduled

		// Notify about new scheduled hours
		msg := fmt.Sprintf("Car connected, charging planned for %s-%s",
			window.Start.Format("15:04"), window.End.Format("15:04"))
		s.sendNotification("EV Charging Scheduled", msg)
	}
}

// handleCarDisconnected handles when the car is disconnected.
func (s *Scheduler) handleCarDisconnected() {
	s.log("Car disconnected from Zappi")

	// If we were charging, record the interruption
	if s.state == StateStartingCharge || s.state == StateMonitoring {
		s.sendNotification("Charging Interrupted", "Car was disconnected during charging session")
	}

	s.state = StateWaitingForCar
	s.session = nil
}

// handleAnnounceTime handles the 19:00 announcement.
func (s *Scheduler) handleAnnounceTime() {
	// Fetch latest prices
	s.fetchPrices()

	window := s.calculateCheapestWindow()
	if window == nil {
		return
	}

	s.scheduledWindow = window

	var msg string
	if window.IsFallback {
		msg = fmt.Sprintf("Prices unavailable, automatic charging will run from %s to %s",
			window.Start.Format("15:04"), window.End.Format("15:04"))
	} else {
		msg = fmt.Sprintf("The 4 cheapest energy hours to charge the car are from %s to %s (avg %.2f EUR/MWh)",
			window.Start.Format("15:04"), window.End.Format("15:04"), window.AvgPrice)
	}

	s.sendNotification("EV Charging Schedule", msg)

	// If car is connected, move to scheduled state
	if s.lastCarConnected {
		s.state = StateScheduled
	} else {
		s.state = StateWaitingForCar
	}
}

// handleCarNotConnectedWarning handles the 20:00 warning.
func (s *Scheduler) handleCarNotConnectedWarning() {
	if !s.lastCarConnected && s.scheduledWindow != nil {
		s.sendNotification("EV Charging Warning", "Car is not connected to the Zappi charger!")
	}
}

// handleMorningSummary handles the 06:00 summary notification.
func (s *Scheduler) handleMorningSummary(now time.Time) {
	// Only on Tuesday, Wednesday, Thursday, Friday
	weekday := now.Weekday()
	if weekday != time.Tuesday && weekday != time.Wednesday &&
	   weekday != time.Thursday && weekday != time.Friday {
		return
	}

	// Get Skoda battery status
	if s.getSkodaClient == nil {
		s.log("Skoda client not configured for summary")
		return
	}

	skodaClient, err := s.getSkodaClient()
	if err != nil {
		s.log("Error getting Skoda client for summary: %v", err)
		return
	}

	charging, err := skodaClient.GetCharging(s.skodaVIN)
	if err != nil {
		s.log("Error getting Skoda charging status: %v", err)
		return
	}

	if charging.Status == nil {
		return
	}

	soc := charging.Status.Battery.StateOfChargePercent
	rangeKm := charging.Status.Battery.RemainingRangeMeters / 1000

	// Build summary message
	msg := fmt.Sprintf("Battery: %d%%\nRange: %d km", soc, rangeKm)

	// Add session info if we charged last night
	if s.session != nil && s.session.ChargeAddedEnd > s.session.ChargeAddedStart {
		kwhCharged := s.session.ChargeAddedEnd - s.session.ChargeAddedStart
		msg += fmt.Sprintf("\nCharged: %.1f kWh", kwhCharged)

		if s.session.TotalCost > 0 {
			msg += fmt.Sprintf("\nCost: %.2f EUR", s.session.TotalCost)
		}
	}

	s.sendNotification("Morning EV Status", msg)

	// Reset session after summary
	s.session = nil
	s.scheduledWindow = nil
	s.state = StateIdle
}

// fetchPrices fetches the latest electricity prices.
func (s *Scheduler) fetchPrices() {
	prices, err := s.nordpoolClient.GetPrices()
	if err != nil {
		s.log("Error fetching prices: %v", err)
		return
	}
	s.lastPrices = prices
	s.lastPricesFetched = time.Now()
}

// calculateCheapestWindow finds the cheapest 4 consecutive hours.
func (s *Scheduler) calculateCheapestWindow() *ChargingWindow {
	loc, _ := time.LoadLocation("Europe/Amsterdam")
	now := time.Now().In(loc)

	// Check if we have tomorrow's prices
	if s.lastPrices == nil || len(s.lastPrices.Tomorrow) == 0 {
		s.fetchPrices()
	}

	// If still no tomorrow's prices, use fallback
	if s.lastPrices == nil || len(s.lastPrices.Tomorrow) == 0 {
		return s.getFallbackWindow(now)
	}

	// Combine today's remaining prices and tomorrow's prices
	allPrices := s.combineAvailablePrices(now)
	if len(allPrices) < 4 {
		return s.getFallbackWindow(now)
	}

	// Find cheapest 4 consecutive hours
	window := s.findCheapestConsecutiveWindow(allPrices, 4)
	if window == nil {
		return s.getFallbackWindow(now)
	}

	return window
}

// combineAvailablePrices combines today's remaining and tomorrow's prices.
func (s *Scheduler) combineAvailablePrices(now time.Time) []nordpool.PricePoint {
	var prices []nordpool.PricePoint

	// Add today's remaining prices (future hours only)
	for _, p := range s.lastPrices.Today {
		if p.Period.After(now) {
			prices = append(prices, p)
		}
	}

	// Add all of tomorrow's prices
	prices = append(prices, s.lastPrices.Tomorrow...)

	return prices
}

// findCheapestConsecutiveWindow finds the cheapest N consecutive hours.
func (s *Scheduler) findCheapestConsecutiveWindow(prices []nordpool.PricePoint, hours int) *ChargingWindow {
	if len(prices) < hours {
		return nil
	}

	// Sort prices by time
	sort.Slice(prices, func(i, j int) bool {
		return prices[i].Period.Before(prices[j].Period)
	})

	// Group prices by hour (they come in 15-min slots)
	hourlyPrices := make(map[time.Time][]float64)
	for _, p := range prices {
		hourStart := time.Date(p.Period.Year(), p.Period.Month(), p.Period.Day(),
			p.Period.Hour(), 0, 0, 0, p.Period.Location())
		hourlyPrices[hourStart] = append(hourlyPrices[hourStart], p.PriceEUR)
	}

	// Convert to sorted slice of hourly averages
	type hourPrice struct {
		hour  time.Time
		price float64
	}
	var hourlyAvgs []hourPrice
	for hour, priceList := range hourlyPrices {
		var sum float64
		for _, p := range priceList {
			sum += p
		}
		hourlyAvgs = append(hourlyAvgs, hourPrice{hour: hour, price: sum / float64(len(priceList))})
	}
	sort.Slice(hourlyAvgs, func(i, j int) bool {
		return hourlyAvgs[i].hour.Before(hourlyAvgs[j].hour)
	})

	if len(hourlyAvgs) < hours {
		return nil
	}

	// Find cheapest consecutive window
	var bestStart int
	var bestAvg float64 = -1

	for i := 0; i <= len(hourlyAvgs)-hours; i++ {
		var sum float64
		for j := 0; j < hours; j++ {
			sum += hourlyAvgs[i+j].price
		}
		avg := sum / float64(hours)

		if bestAvg < 0 || avg < bestAvg {
			bestAvg = avg
			bestStart = i
		}
	}

	return &ChargingWindow{
		Start:      hourlyAvgs[bestStart].hour,
		End:        hourlyAvgs[bestStart+hours-1].hour.Add(time.Hour),
		IsFallback: false,
		AvgPrice:   bestAvg,
	}
}

// getFallbackWindow returns the default fallback window (02:00-06:00).
func (s *Scheduler) getFallbackWindow(now time.Time) *ChargingWindow {
	loc, _ := time.LoadLocation("Europe/Amsterdam")

	// Calculate next occurrence of 02:00
	fallbackStart := time.Date(now.Year(), now.Month(), now.Day(), 2, 0, 0, 0, loc)
	if now.Hour() >= 2 {
		// Move to tomorrow
		fallbackStart = fallbackStart.AddDate(0, 0, 1)
	}

	return &ChargingWindow{
		Start:      fallbackStart,
		End:        fallbackStart.Add(4 * time.Hour),
		IsFallback: true,
		AvgPrice:   0,
	}
}

// startChargingSession starts the charging session.
func (s *Scheduler) startChargingSession() {
	if !s.lastCarConnected {
		s.log("Cannot start charging: car not connected")
		return
	}

	// Check SOC
	if s.getSkodaClient != nil {
		if skodaClient, err := s.getSkodaClient(); err == nil {
			charging, err := skodaClient.GetCharging(s.skodaVIN)
			if err == nil && charging.Status != nil {
				soc := charging.Status.Battery.StateOfChargePercent
				if soc >= 95 {
					s.log("Skipping charging: SOC is already at %d%%", soc)
					s.state = StateIdle
					return
				}
			}
		}
	}

	s.log("Starting charging session")

	// Get current charge level from Zappi
	zappis, _ := s.zappiClient.GetZappiStatus()
	var chargeAdded float64
	if zappi := s.findZappi(zappis); zappi != nil {
		chargeAdded = zappi.ChargeAdded
	}

	s.session = &Session{
		Window:           s.scheduledWindow,
		StartedAt:        time.Now(),
		ChargeAddedStart: chargeAdded,
	}

	// Set Zappi to Fast mode
	serial := s.zappiSerial
	if serial == "" && len(zappis) > 0 {
		serial = fmt.Sprintf("%d", zappis[0].Serial)
	}

	if err := s.zappiClient.SetZappiMode(serial, myenergi.ZappiModeFast); err != nil {
		s.log("Error setting Zappi to Fast mode: %v", err)
	}

	s.state = StateStartingCharge
	s.chargingStartedAt = time.Now()
	s.chargingNotStartedFor = 0
}

// processScheduledState processes the scheduled state.
func (s *Scheduler) processScheduledState(zappi *myenergi.Zappi) {
	// Just waiting for the charging window to start
	// The checkScheduledEvents function handles transitioning to charging
}

// processStartingChargeState processes the charging state.
func (s *Scheduler) processStartingChargeState(zappi *myenergi.Zappi) {
	if s.session == nil {
		s.state = StateIdle
		return
	}

	isCharging := s.isCarCharging(zappi)

	if isCharging {
		s.session.ChargingStarted = true
		s.state = StateMonitoring
		s.chargingNotStartedFor = 0
		return
	}

	// Charging hasn't started yet
	elapsed := time.Since(s.chargingStartedAt)

	// After 5 minutes, try Skoda wakeup
	if elapsed >= 5*time.Minute && !s.session.SkodaWakeupSent {
		s.log("Charging not started after 5 minutes, sending Skoda start command")
		if s.getSkodaClient != nil {
			if skodaClient, err := s.getSkodaClient(); err == nil {
				if err := skodaClient.StartCharging(s.skodaVIN); err != nil {
					s.log("Error sending Skoda start command: %v", err)
				}
			}
		}
		s.session.SkodaWakeupSent = true
	}

	// After 10 minutes, send failure notification
	if elapsed >= 10*time.Minute && !s.session.FailureNotified {
		s.log("Charging not started after 10 minutes, sending failure notification")
		s.sendNotification("EV Charging Failed", "Car won't charge, manual intervention needed")
		s.session.FailureNotified = true
	}
}

// processMonitoringState processes the monitoring state.
func (s *Scheduler) processMonitoringState(zappi *myenergi.Zappi) {
	if s.session == nil {
		s.state = StateIdle
		return
	}

	isCharging := s.isCarCharging(zappi)

	if !isCharging && s.session.ChargingStarted {
		// Charging was interrupted
		s.log("Charging interrupted, attempting to resume")
		s.sendNotification("Charging Interrupted", "Attempting to resume charging")

		// Try to restart
		serial := s.zappiSerial
		if serial == "" {
			serial = fmt.Sprintf("%d", zappi.Serial)
		}
		if err := s.zappiClient.SetZappiMode(serial, myenergi.ZappiModeFast); err != nil {
			s.log("Error restarting charging: %v", err)
		}

		s.state = StateStartingCharge
		s.chargingStartedAt = time.Now()
		s.session.SkodaWakeupSent = false
		s.session.FailureNotified = false
	}

	// Update session charge tracking
	s.session.ChargeAddedEnd = zappi.ChargeAdded
}

// endChargingSession ends the charging session.
func (s *Scheduler) endChargingSession() {
	s.log("Ending charging session")

	// Stop Zappi
	serial := s.zappiSerial
	zappis, _ := s.zappiClient.GetZappiStatus()
	if serial == "" && len(zappis) > 0 {
		serial = fmt.Sprintf("%d", zappis[0].Serial)
	}

	if err := s.zappiClient.SetZappiMode(serial, myenergi.ZappiModeStopped); err != nil {
		s.log("Error stopping Zappi: %v", err)
	}

	// Update session with final values
	if s.session != nil && len(zappis) > 0 {
		if zappi := s.findZappi(zappis); zappi != nil {
			s.session.ChargeAddedEnd = zappi.ChargeAdded
		}

		// Calculate estimated cost
		if s.scheduledWindow != nil && !s.scheduledWindow.IsFallback {
			kwhCharged := s.session.ChargeAddedEnd - s.session.ChargeAddedStart
			pricePerKwh := s.scheduledWindow.AvgPrice/1000 + s.energyMarkup
			s.session.TotalCost = kwhCharged * pricePerKwh
		}
	}

	s.state = StateIdle
}

// sendNotification sends a push notification.
func (s *Scheduler) sendNotification(title, message string) {
	if s.pushClient == nil {
		s.log("Pushover not configured, skipping notification: %s - %s", title, message)
		return
	}

	if err := s.pushClient.SendWithTitle(title, message); err != nil {
		s.log("Error sending notification: %v", err)
	}
}

// log logs a message with timestamp.
func (s *Scheduler) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[autocharge] %s", msg)
}
