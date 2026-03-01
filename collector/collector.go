// Package collector provides a data collector that saves metrics every minute.
package collector

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
)

// Config holds collector configuration.
type Config struct {
	DataDir         string
	Interval        time.Duration

	// Credentials
	MySkodaUsername string
	MySkodaPassword string
	AlphaESSAppID   string
	AlphaESSSecret  string
	AlphaESSSN      string
	MyEnergiSerial  string
	MyEnergiPass    string
}

// DataPoint represents a single data point saved to JSON.
type DataPoint struct {
	Time         string  `json:"time"`
	Timestamp    int64   `json:"timestamp"`

	// AlphaESS
	BatterySOC   float64 `json:"battery_soc"`
	BatteryPower float64 `json:"battery_power"`
	GridPower    float64 `json:"grid_power"`
	PVPower      float64 `json:"pv_power"`
	LoadPower    float64 `json:"load_power"`

	// Zappi
	ZappiPower   float64 `json:"zappi_power"`
	ZappiSolar   float64 `json:"zappi_solar"`
	ZappiHouse   float64 `json:"zappi_house"`
	ZappiGrid    float64 `json:"zappi_grid"`
	ZappiMode    string  `json:"zappi_mode"`
	ZappiStatus  string  `json:"zappi_status"`
	ChargeAdded  float64 `json:"charge_added"`

	// NordPool
	EnergyPrice  float64 `json:"energy_price"`

	// MySkoda (first vehicle)
	CarSOC       float64 `json:"car_soc"`
	CarRange     float64 `json:"car_range"`
	CarCharging  bool    `json:"car_charging"`
}

// Collector collects data from all sources.
type Collector struct {
	config      Config
	npClient    *nordpool.Client
	aessClient  *alphaess.Client
	meClient    *myenergi.Client
	skodaClient *myskoda.Client
}

// NewCollector creates a new collector.
func NewCollector(cfg Config) *Collector {
	c := &Collector{
		config:   cfg,
		npClient: nordpool.NewClient(),
	}

	if cfg.AlphaESSAppID != "" && cfg.AlphaESSSecret != "" {
		c.aessClient = alphaess.NewClient(cfg.AlphaESSAppID, cfg.AlphaESSSecret)
		if cfg.AlphaESSSN != "" {
			c.aessClient.SetSN(cfg.AlphaESSSN)
		}
	}

	if cfg.MyEnergiSerial != "" && cfg.MyEnergiPass != "" {
		c.meClient = myenergi.NewClient(cfg.MyEnergiSerial, cfg.MyEnergiPass)
	}

	return c
}

// initSkodaClient initializes the Skoda client.
func (c *Collector) initSkodaClient() error {
	if c.config.MySkodaUsername == "" || c.config.MySkodaPassword == "" {
		return nil
	}

	client, err := myskoda.NewClient(c.config.MySkodaUsername, c.config.MySkodaPassword)
	if err != nil {
		return fmt.Errorf("creating MySkoda client: %w", err)
	}

	if err := client.Login(); err != nil {
		return fmt.Errorf("MySkoda login: %w", err)
	}

	c.skodaClient = client
	return nil
}

// collect gathers data from all sources and returns a DataPoint.
func (c *Collector) collect() DataPoint {
	now := time.Now()
	dp := DataPoint{
		Time:      now.Format("2006-01-02 15:04:05"),
		Timestamp: now.Unix(),
	}

	// AlphaESS
	if c.aessClient != nil {
		if power, err := c.aessClient.GetLastPowerData(); err == nil {
			dp.BatterySOC = power.SOC
			dp.BatteryPower = power.BatteryPower
			dp.GridPower = power.GridPower
			dp.PVPower = power.PVPower
			dp.LoadPower = power.LoadPower
		} else {
			log.Printf("AlphaESS error: %v", err)
		}
	}

	// Zappi
	if c.meClient != nil {
		if zappis, err := c.meClient.GetZappiStatus(); err == nil && len(zappis) > 0 {
			z := &zappis[0]
			dp.ZappiPower = z.ChargerPower()
			dp.ZappiSolar = z.SolarPower()
			dp.ZappiHouse = z.HouseConsumption()
			dp.ZappiGrid = z.GridPower
			dp.ZappiMode = z.Mode.String()
			dp.ZappiStatus = z.Status.String()
			dp.ChargeAdded = z.ChargeAdded
		} else if err != nil {
			log.Printf("MyEnergi error: %v", err)
		}
	}

	// NordPool
	if prices, err := c.npClient.GetPrices(); err == nil {
		if current := c.npClient.GetCurrentPrice(prices); current != nil {
			dp.EnergyPrice = current.PricePerKWh()
		}
	}

	// MySkoda
	if c.skodaClient != nil {
		if vehicles, err := c.skodaClient.GetVehicles(); err == nil && len(vehicles) > 0 {
			if charging, err := c.skodaClient.GetCharging(vehicles[0].VIN); err == nil && charging.Status != nil {
				dp.CarSOC = float64(charging.Status.Battery.StateOfChargePercent)
				dp.CarRange = float64(charging.Status.Battery.RemainingRangeMeters) / 1000.0
				dp.CarCharging = charging.Status.State == "CHARGING"
			}
		}
	}

	return dp
}

// save writes the data point to a JSON file.
func (c *Collector) save(dp DataPoint) error {
	// Ensure data directory exists
	if err := os.MkdirAll(c.config.DataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	// Create filename with timestamp
	filename := filepath.Join(c.config.DataDir, fmt.Sprintf("%d.json", dp.Timestamp))

	data, err := json.MarshalIndent(dp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling data: %w", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}

	return nil
}

// cleanup removes old data files (older than 7 days).
func (c *Collector) cleanup() {
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()

	files, err := filepath.Glob(filepath.Join(c.config.DataDir, "*.json"))
	if err != nil {
		return
	}

	for _, f := range files {
		// Extract timestamp from filename
		base := filepath.Base(f)
		var ts int64
		if _, err := fmt.Sscanf(base, "%d.json", &ts); err != nil {
			continue
		}

		if ts < cutoff {
			os.Remove(f)
		}
	}
}

// Run starts the collector loop.
func (c *Collector) Run() error {
	// Initialize Skoda client
	if err := c.initSkodaClient(); err != nil {
		log.Printf("Warning: MySkoda initialization failed: %v", err)
	}

	interval := c.config.Interval
	if interval == 0 {
		interval = time.Minute
	}

	log.Printf("Starting collector (interval: %v, data dir: %s)", interval, c.config.DataDir)

	// Run cleanup on start
	c.cleanup()

	// Initial collection
	dp := c.collect()
	if err := c.save(dp); err != nil {
		log.Printf("Error saving data: %v", err)
	} else {
		log.Printf("Collected: SOC=%.1f%%, PV=%.0fW, Grid=%.0fW, Load=%.0fW",
			dp.BatterySOC, dp.PVPower, dp.GridPower, dp.LoadPower)
	}

	ticker := time.NewTicker(interval)
	cleanupTicker := time.NewTicker(time.Hour) // Cleanup every hour

	for {
		select {
		case <-ticker.C:
			dp := c.collect()
			if err := c.save(dp); err != nil {
				log.Printf("Error saving data: %v", err)
			} else {
				log.Printf("Collected: SOC=%.1f%%, PV=%.0fW, Grid=%.0fW, Load=%.0fW, Price=€%.4f/kWh",
					dp.BatterySOC, dp.PVPower, dp.GridPower, dp.LoadPower, dp.EnergyPrice)
			}
		case <-cleanupTicker.C:
			c.cleanup()
		}
	}
}

// RunOnce collects and saves data once (for cron jobs).
func (c *Collector) RunOnce() error {
	if err := c.initSkodaClient(); err != nil {
		log.Printf("Warning: MySkoda initialization failed: %v", err)
	}

	dp := c.collect()
	if err := c.save(dp); err != nil {
		return fmt.Errorf("saving data: %w", err)
	}

	log.Printf("Collected: SOC=%.1f%%, PV=%.0fW, Grid=%.0fW, Load=%.0fW, Price=€%.4f/kWh",
		dp.BatterySOC, dp.PVPower, dp.GridPower, dp.LoadPower, dp.EnergyPrice)

	return nil
}
