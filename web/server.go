// Package web provides an HTTP server for the homecontrol dashboard.
package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/autocharge"
	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
	"github.com/mpdroog/homecontrol/pushover"
	"github.com/mpdroog/homecontrol/weather"
)

// Config holds the server configuration.
type Config struct {
	ListenAddr string
	DataDir    string // Directory for JSON data files

	// Credentials (optional, if not set those sections won't be displayed)
	MySkodaUsername string
	MySkodaPassword string
	AlphaESSAppID   string
	AlphaESSSecret  string
	AlphaESSSN      string
	MyEnergiSerial  string
	MyEnergiPass    string

	// Weather location
	WeatherLat float64
	WeatherLon float64

	// AutoCharge scheduler config
	AutoChargeZappiSerial  string
	AutoChargeSkodaVIN     string
	AutoChargeEnergyMarkup float64
	PushoverToken          string
	PushoverUser           string
}

// DashboardData holds all data for the dashboard template.
type DashboardData struct {
	LastUpdate time.Time
	Error      string

	// NordPool prices
	Prices        *nordpool.Prices
	CurrentPrice  *nordpool.PricePoint
	NextPrice     *nordpool.PricePoint
	LowestPrice   *nordpool.PricePoint
	HighestPrice  *nordpool.PricePoint

	// AlphaESS battery
	Battery       *alphaess.PowerData
	ChargeConfig  *alphaess.ChargeConfigData
	DischargeConf *alphaess.DischargeConfigData

	// Zappi charger
	Zappis []myenergi.Zappi

	// MySkoda vehicles
	Vehicles []VehicleData

	// Weather
	Weather *weather.Weather
}

// VehicleData holds vehicle information.
type VehicleData struct {
	Vehicle  myskoda.Vehicle
	Charging *myskoda.Charging
	Status   *myskoda.VehicleStatus
	AC       *myskoda.AirConditioning
	Position *myskoda.Position
	Health   *myskoda.Health
}

// ChartDataPoint represents a single data point for charts.
type ChartDataPoint struct {
	Time         string  `json:"time"`
	BatterySOC   float64 `json:"battery_soc"`
	BatteryPower float64 `json:"battery_power"`
	GridPower    float64 `json:"grid_power"`
	PVPower      float64 `json:"pv_power"`
	LoadPower    float64 `json:"load_power"`
	ZappiPower   float64 `json:"zappi_power"`
	EnergyPrice  float64 `json:"energy_price"`
	CarSOC       float64 `json:"car_soc"`
}

// Server is the HTTP server.
type Server struct {
	config        Config
	npClient      *nordpool.Client
	aessClient    *alphaess.Client
	meClient      *myenergi.Client
	skodaClient   *myskoda.Client
	weatherClient *weather.Client
	scheduler     *autocharge.Scheduler

	mu         sync.RWMutex
	data       DashboardData
}

// NewServer creates a new HTTP server.
func NewServer(cfg Config) *Server {
	s := &Server{
		config:   cfg,
		npClient: nordpool.NewClient(),
	}

	if cfg.AlphaESSAppID != "" && cfg.AlphaESSSecret != "" {
		s.aessClient = alphaess.NewClient(cfg.AlphaESSAppID, cfg.AlphaESSSecret)
		if cfg.AlphaESSSN != "" {
			s.aessClient.SetSN(cfg.AlphaESSSN)
		}
	}

	if cfg.MyEnergiSerial != "" && cfg.MyEnergiPass != "" {
		s.meClient = myenergi.NewClient(cfg.MyEnergiSerial, cfg.MyEnergiPass)
	}

	if cfg.WeatherLat != 0 && cfg.WeatherLon != 0 {
		s.weatherClient = weather.NewClient(cfg.WeatherLat, cfg.WeatherLon)
	}

	// Initialize AutoCharge scheduler if configured
	if s.meClient != nil && cfg.AutoChargeZappiSerial != "" {
		// Create a function to get (or re-login) the Skoda client
		var getSkodaClient autocharge.SkodaClientFunc
		if cfg.MySkodaUsername != "" && cfg.MySkodaPassword != "" {
			getSkodaClient = func() (*myskoda.Client, error) {
				if s.skodaClient != nil {
					return s.skodaClient, nil
				}
				if err := s.initSkodaClient(); err != nil {
					return nil, err
				}
				return s.skodaClient, nil
			}
		}

		// Create a function to get prices from server cache
		getPrices := func() *nordpool.Prices {
			s.mu.RLock()
			defer s.mu.RUnlock()
			return s.data.Prices
		}

		// Create a function to get config values
		getConfig := func() autocharge.Config {
			return autocharge.Config{
				ZappiSerial:  s.config.AutoChargeZappiSerial,
				SkodaVIN:     s.config.AutoChargeSkodaVIN,
				EnergyMarkup: s.config.AutoChargeEnergyMarkup,
			}
		}

		// Create a function to get Zappi status from server cache
		getZappis := func() []myenergi.Zappi {
			s.mu.RLock()
			defer s.mu.RUnlock()
			return s.data.Zappis
		}

		// Create a function to get Skoda charging status from server cache
		getCharging := func(vin string) *myskoda.Charging {
			s.mu.RLock()
			defer s.mu.RUnlock()
			for _, v := range s.data.Vehicles {
				if v.Vehicle.VIN == vin {
					return v.Charging
				}
			}
			return nil
		}

		// Create pushover client if configured
		var pushClient *pushover.Client
		if cfg.PushoverToken != "" && cfg.PushoverUser != "" {
			pushClient = pushover.NewClient(cfg.PushoverToken, cfg.PushoverUser)
		}

		s.scheduler = autocharge.NewScheduler(
			s.meClient,
			pushClient,
			getSkodaClient,
			getPrices,
			getConfig,
			getZappis,
			getCharging,
		)
	}

	return s
}

// initSkodaClient initializes the Skoda client (requires login).
func (s *Server) initSkodaClient() error {
	if s.config.MySkodaUsername == "" || s.config.MySkodaPassword == "" {
		return nil
	}

	client, err := myskoda.NewClient(s.config.MySkodaUsername, s.config.MySkodaPassword)
	if err != nil {
		return fmt.Errorf("creating MySkoda client: %w", err)
	}

	if err := client.Login(); err != nil {
		return fmt.Errorf("MySkoda login: %w", err)
	}

	s.skodaClient = client
	return nil
}

// refreshData fetches fresh data from all sources.
func (s *Server) refreshData() {
	data := DashboardData{
		LastUpdate: time.Now(),
	}

	// Fetch NordPool prices
	if prices, err := s.npClient.GetPrices(); err == nil {
		data.Prices = prices
		data.CurrentPrice = s.npClient.GetCurrentPrice(prices)
		data.NextPrice = s.npClient.GetNextPrice(prices)
		data.LowestPrice = s.npClient.GetLowestPrice(prices)
		data.HighestPrice = s.npClient.GetHighestPrice(prices)
	} else {
		log.Printf("NordPool error: %v", err)
	}

	// Fetch AlphaESS data
	if s.aessClient != nil {
		if power, err := s.aessClient.GetLastPowerData(); err == nil {
			data.Battery = power
		} else {
			log.Printf("AlphaESS power error: %v", err)
		}
		if charge, err := s.aessClient.GetChargeConfig(); err == nil {
			data.ChargeConfig = charge
		} else {
			log.Printf("AlphaESS charge config error: %v", err)
		}
		if discharge, err := s.aessClient.GetDischargeConfig(); err == nil {
			data.DischargeConf = discharge
		} else {
			log.Printf("AlphaESS discharge config error: %v", err)
		}
	}

	// Fetch Zappi data
	if s.meClient != nil {
		if zappis, err := s.meClient.GetZappiStatus(); err == nil {
			data.Zappis = zappis
		} else {
			log.Printf("MyEnergi error: %v", err)
		}
	}

	// Fetch MySkoda data (client handles re-login internally)
	if s.skodaClient != nil {
		vehicles, err := s.skodaClient.GetVehicles()
		if err != nil {
			log.Printf("MySkoda GetVehicles failed: %v", err)
		} else {
			for _, v := range vehicles {
				vd := VehicleData{Vehicle: v}
				if charging, err := s.skodaClient.GetCharging(v.VIN); err == nil {
					vd.Charging = charging
				} else {
					log.Printf("MySkoda GetCharging error for %s: %v", v.VIN, err)
				}
				if status, err := s.skodaClient.GetStatus(v.VIN); err == nil {
					vd.Status = status
				} else {
					log.Printf("MySkoda GetStatus error for %s: %v", v.VIN, err)
				}
				if ac, err := s.skodaClient.GetAirConditioning(v.VIN); err == nil {
					vd.AC = ac
				} else {
					log.Printf("MySkoda GetAirConditioning error for %s: %v", v.VIN, err)
				}
				if pos, err := s.skodaClient.GetPosition(v.VIN); err == nil {
					vd.Position = pos
				} else {
					log.Printf("MySkoda GetPosition error for %s: %v", v.VIN, err)
				}
				if health, err := s.skodaClient.GetHealth(v.VIN); err == nil {
					vd.Health = health
				} else {
					log.Printf("MySkoda GetHealth error for %s: %v", v.VIN, err)
				}
				data.Vehicles = append(data.Vehicles, vd)
			}
		}
	}

	// Fetch weather data
	if s.weatherClient != nil {
		if w, err := s.weatherClient.GetCurrent(); err == nil {
			data.Weather = w
		} else {
			log.Printf("Weather fetch error: %v", err)
		}
	}

	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
}

// getData returns the current dashboard data.
func (s *Server) getData() DashboardData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// handleDashboard serves the main dashboard page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.getData()

	tmpl := template.Must(template.New("dashboard.html").Funcs(template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.Format("15:04")
		},
		"formatDateTime": func(t time.Time) string {
			return t.Format("2006-01-02 15:04:05")
		},
		"formatPrice": func(p float64) string {
			return fmt.Sprintf("%.4f", p/1000.0)
		},
		"formatPower": func(p float64) string {
			if p >= 1000 || p <= -1000 {
				return fmt.Sprintf("%.2f kW", p/1000.0)
			}
			return fmt.Sprintf("%.0f W", p)
		},
		"powerDirection": func(p float64) string {
			if p > 0 {
				return "importing"
			} else if p < 0 {
				return "exporting"
			}
			return ""
		},
		"batteryDirection": func(p float64) string {
			if p > 0 {
				return "charging"
			} else if p < 0 {
				return "discharging"
			}
			return ""
		},
		"abs": func(p float64) float64 {
			if p < 0 {
				return -p
			}
			return p
		},
		"divideBy": func(a int, b float64) float64 {
			return float64(a) / b
		},
		"subtract": func(a, b float64) float64 {
			return a - b
		},
		"voltageClass": func(v float64) string {
			// EU nominal: 230V ±10% (207-253V)
			if v < 207 || v > 253 {
				return "bg-danger" // Red: outside tolerance
			}
			if v < 216 || v > 245 {
				return "bg-warning text-dark" // Yellow: approaching limits
			}
			return "bg-success" // Green: good range
		},
		"windDirection": weather.WindDirectionToString,
	}).ParseFiles("templates/dashboard.html"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleAPI returns JSON data for AJAX updates.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	data := s.getData()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

// handleRefresh triggers a data refresh.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.refreshData()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleZappiControl handles Zappi control actions.
func (s *Server) handleZappiControl(w http.ResponseWriter, r *http.Request) {
	if s.meClient == nil {
		http.Error(w, "MyEnergi not configured", http.StatusBadRequest)
		return
	}

	action := r.URL.Query().Get("action")
	serial := r.URL.Query().Get("serial")

	if serial == "" {
		// Use first Zappi if serial not specified
		zappis, err := s.meClient.GetZappiStatus()
		if err != nil || len(zappis) == 0 {
			http.Error(w, "No Zappi found", http.StatusBadRequest)
			return
		}
		serial = fmt.Sprintf("%d", zappis[0].Serial)
	}

	var err error
	switch action {
	case "fast":
		err = s.meClient.SetZappiMode(serial, myenergi.ZappiModeFast)
	case "eco":
		err = s.meClient.SetZappiMode(serial, myenergi.ZappiModeEco)
	case "eco+":
		err = s.meClient.SetZappiMode(serial, myenergi.ZappiModeEcoPlus)
	case "stop":
		err = s.meClient.SetZappiMode(serial, myenergi.ZappiModeStopped)
	case "boost":
		kwhStr := r.URL.Query().Get("kwh")
		kwh, _ := strconv.Atoi(kwhStr)
		if kwh <= 0 {
			kwh = 5
		}
		err = s.meClient.BoostZappi(serial, kwh)
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Refresh data after action
	s.refreshData()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleSkodaControl handles Skoda charging control.
func (s *Server) handleSkodaControl(w http.ResponseWriter, r *http.Request) {
	if s.skodaClient == nil {
		http.Error(w, "MySkoda not configured", http.StatusBadRequest)
		return
	}

	action := r.URL.Query().Get("action")
	vin := r.URL.Query().Get("vin")

	if vin == "" {
		vehicles, err := s.skodaClient.GetVehicles()
		if err != nil || len(vehicles) == 0 {
			http.Error(w, "No vehicles found", http.StatusBadRequest)
			return
		}
		vin = vehicles[0].VIN
	}

	var err error
	switch action {
	case "start":
		err = s.skodaClient.StartCharging(vin)
	case "stop":
		err = s.skodaClient.StopCharging(vin)
	case "limit":
		limitStr := r.URL.Query().Get("percent")
		limit, _ := strconv.Atoi(limitStr)
		if limit < 50 || limit > 100 {
			http.Error(w, "Limit must be between 50 and 100", http.StatusBadRequest)
			return
		}
		err = s.skodaClient.SetChargeLimit(vin, limit)
	case "wakeup":
		err = s.skodaClient.WakeUp(vin)
	default:
		http.Error(w, "Unknown action", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.refreshData()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleChartData returns historical data for charts.
func (s *Server) handleChartData(w http.ResponseWriter, r *http.Request) {
	// Read all JSON files from data directory
	files, err := filepath.Glob(filepath.Join(s.config.DataDir, "*.json"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Sort files by name (they're timestamped)
	sort.Strings(files)

	// Only use last 24 hours of data (1440 minutes)
	if len(files) > 1440 {
		files = files[len(files)-1440:]
	}

	var points []ChartDataPoint
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}

		var point ChartDataPoint
		if err := json.Unmarshal(data, &point); err != nil {
			continue
		}
		points = append(points, point)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(points)
}

// handleAutoChargeControl handles autocharge scheduler control.
func (s *Server) handleAutoChargeControl(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		http.Error(w, "AutoCharge scheduler not configured", http.StatusBadRequest)
		return
	}

	action := r.URL.Query().Get("action")

	switch action {
	case "enable":
		s.scheduler.Enable()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Scheduler enabled",
		})
	case "disable":
		s.scheduler.Disable()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Scheduler disabled (current session continues)",
		})
	case "status":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.scheduler.GetStatus())
	default:
		http.Error(w, "Unknown action. Use: enable, disable, or status", http.StatusBadRequest)
	}
}

// Run starts the HTTP server.
func (s *Server) Run() error {
	// Initialize Skoda client (requires login)
	if err := s.initSkodaClient(); err != nil {
		log.Printf("Warning: MySkoda initialization failed: %v", err)
	}

	// Initial data fetch
	log.Println("Fetching initial data...")
	s.refreshData()

	// Start background refresh (every 60 seconds)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		for range ticker.C {
			s.refreshData()
		}
	}()

	// Start AutoCharge scheduler if configured
	if s.scheduler != nil {
		log.Println("Starting AutoCharge scheduler...")
		go func() {
			if err := s.scheduler.Run(); err != nil {
				log.Printf("AutoCharge scheduler error: %v", err)
			}
		}()
	}

	// Setup routes
	http.HandleFunc("/", s.handleDashboard)
	http.HandleFunc("/api/data", s.handleAPI)
	http.HandleFunc("/api/refresh", s.handleRefresh)
	http.HandleFunc("/api/zappi", s.handleZappiControl)
	http.HandleFunc("/api/skoda", s.handleSkodaControl)
	http.HandleFunc("/api/chart", s.handleChartData)
	http.HandleFunc("/api/autocharge", s.handleAutoChargeControl)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	log.Printf("Starting server on %s", s.config.ListenAddr)

	// Notify systemd that we're ready
	daemon.SdNotify(false, daemon.SdNotifyReady)

	return http.ListenAndServe(s.config.ListenAddr, nil)
}
