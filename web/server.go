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

	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
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
	config     Config
	npClient   *nordpool.Client
	aessClient *alphaess.Client
	meClient   *myenergi.Client
	skodaClient *myskoda.Client

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
	}

	// Fetch AlphaESS data
	if s.aessClient != nil {
		if power, err := s.aessClient.GetLastPowerData(); err == nil {
			data.Battery = power
		}
		if charge, err := s.aessClient.GetChargeConfig(); err == nil {
			data.ChargeConfig = charge
		}
		if discharge, err := s.aessClient.GetDischargeConfig(); err == nil {
			data.DischargeConf = discharge
		}
	}

	// Fetch Zappi data
	if s.meClient != nil {
		if zappis, err := s.meClient.GetZappiStatus(); err == nil {
			data.Zappis = zappis
		}
	}

	// Fetch MySkoda data
	if s.skodaClient != nil {
		if vehicles, err := s.skodaClient.GetVehicles(); err == nil {
			for _, v := range vehicles {
				vd := VehicleData{Vehicle: v}
				if charging, err := s.skodaClient.GetCharging(v.VIN); err == nil {
					vd.Charging = charging
				}
				if status, err := s.skodaClient.GetStatus(v.VIN); err == nil {
					vd.Status = status
				}
				if ac, err := s.skodaClient.GetAirConditioning(v.VIN); err == nil {
					vd.AC = ac
				}
				if pos, err := s.skodaClient.GetPosition(v.VIN); err == nil {
					vd.Position = pos
				}
				if health, err := s.skodaClient.GetHealth(v.VIN); err == nil {
					vd.Health = health
				}
				data.Vehicles = append(data.Vehicles, vd)
			}
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

	tmpl := template.Must(template.New("dashboard").Funcs(template.FuncMap{
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
	}).Parse(dashboardTemplate))

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

	// Setup routes
	http.HandleFunc("/", s.handleDashboard)
	http.HandleFunc("/api/data", s.handleAPI)
	http.HandleFunc("/api/refresh", s.handleRefresh)
	http.HandleFunc("/api/zappi", s.handleZappiControl)
	http.HandleFunc("/api/skoda", s.handleSkodaControl)
	http.HandleFunc("/api/chart", s.handleChartData)

	log.Printf("Starting server on %s", s.config.ListenAddr)
	return http.ListenAndServe(s.config.ListenAddr, nil)
}

const dashboardTemplate = `<!DOCTYPE html>
<html lang="en" data-bs-theme="dark">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Home Control Dashboard</title>
    <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.3/dist/css/bootstrap.min.css" rel="stylesheet">
    <style>
        body { background: #0d1117; }
        .card { background: #161b22; border: 1px solid #30363d; }
        .card-header { background: #21262d; border-bottom: 1px solid #30363d; }
        .list-group-item { background: #161b22; border-color: #30363d; }
        .text-positive { color: #3fb950; }
        .text-negative { color: #f85149; }
        .text-charging { color: #58a6ff; }
        .text-warning-custom { color: #d29922; }
        .power-flow { display: flex; justify-content: space-around; text-align: center; padding: 1rem 0; }
        .power-item { display: flex; flex-direction: column; align-items: center; }
        .power-icon { font-size: 2rem; margin-bottom: 0.25rem; }
        .power-value { font-size: 1.1rem; font-weight: 600; }
        .power-label { font-size: 0.75rem; color: #8b949e; }
        .echart { width: 100%; height: 300px; }
        .soc-gradient {
            background: linear-gradient(90deg, #f85149, #d29922, #3fb950);
            border-radius: 0.25rem;
        }
        /* Energy Flow Diagram */
        .energy-flow-svg { width: 100%; height: 320px; }
        .energy-node {
            fill: none;
            stroke-width: 3;
        }
        .energy-node-bg {
            fill: #161b22;
        }
        .energy-node-icon {
            fill: #8b949e;
            font-size: 24px;
            dominant-baseline: central;
            text-anchor: middle;
        }
        .energy-label {
            fill: #c9d1d9;
            font-size: 13px;
            font-weight: 600;
            text-anchor: middle;
        }
        .energy-sublabel {
            fill: #8b949e;
            font-size: 11px;
            text-anchor: middle;
        }
        .flow-line {
            fill: none;
            stroke: #30363d;
            stroke-width: 3;
        }
        .flow-line-active {
            stroke-width: 3;
            stroke-linecap: round;
        }
        .flow-dots {
            fill: none;
            stroke-width: 4;
            stroke-linecap: round;
            stroke-dasharray: 0 12;
        }
        @keyframes flowForward {
            0% { stroke-dashoffset: 24; }
            100% { stroke-dashoffset: 0; }
        }
        @keyframes flowBackward {
            0% { stroke-dashoffset: 0; }
            100% { stroke-dashoffset: 24; }
        }
        .flow-animate-forward {
            animation: flowForward 0.8s linear infinite;
        }
        .flow-animate-backward {
            animation: flowBackward 0.8s linear infinite;
        }
        .node-house { stroke: #da3cda; }
        .node-zappi { stroke: #3b82f6; }
        .node-grid { stroke: #f97316; }
        .node-solar { stroke: #3fb950; }
        .node-battery { stroke: #22d3ee; }
        .node-center { stroke: #3fb950; }
        .flow-grid { stroke: #f97316; }
        .flow-solar { stroke: #3fb950; }
        .flow-zappi { stroke: #3b82f6; }
        .flow-house { stroke: #da3cda; }
        .flow-battery { stroke: #22d3ee; }
    </style>
</head>
<body>
    <nav class="navbar navbar-dark bg-dark border-bottom border-secondary mb-4">
        <div class="container-fluid">
            <span class="navbar-brand mb-0 h1">Home Control</span>
            <div class="d-flex align-items-center gap-3">
                <small class="text-secondary">Updated: {{formatDateTime .LastUpdate}}</small>
                <a href="/api/refresh" class="btn btn-outline-secondary btn-sm">Refresh</a>
            </div>
        </div>
    </nav>

    <div class="container-fluid">
        <div class="row g-4">
            {{if .Prices}}
            <div class="col-12 col-md-6 col-lg-4">
                <div class="card h-100">
                    <div class="card-header d-flex align-items-center gap-2">
                        <span>⚡</span> <span>Energy Prices</span>
                    </div>
                    <ul class="list-group list-group-flush">
                        {{if .CurrentPrice}}
                        <li class="list-group-item d-flex justify-content-between">
                            <span class="text-secondary">Current ({{formatTime .CurrentPrice.Period}})</span>
                            <strong>€{{formatPrice .CurrentPrice.PriceEUR}}/kWh</strong>
                        </li>
                        {{end}}
                        {{if .NextPrice}}
                        <li class="list-group-item d-flex justify-content-between">
                            <span class="text-secondary">Next Hour</span>
                            <strong>€{{formatPrice .NextPrice.PriceEUR}}/kWh</strong>
                        </li>
                        {{end}}
                        {{if .LowestPrice}}
                        <li class="list-group-item d-flex justify-content-between">
                            <span class="text-secondary">Lowest ({{formatTime .LowestPrice.Period}})</span>
                            <strong class="text-positive">€{{formatPrice .LowestPrice.PriceEUR}}/kWh</strong>
                        </li>
                        {{end}}
                        {{if .HighestPrice}}
                        <li class="list-group-item d-flex justify-content-between">
                            <span class="text-secondary">Highest ({{formatTime .HighestPrice.Period}})</span>
                            <strong class="text-negative">€{{formatPrice .HighestPrice.PriceEUR}}/kWh</strong>
                        </li>
                        {{end}}
                    </ul>
                </div>
            </div>
            {{end}}

            {{if .Battery}}
            <div class="col-12 col-md-6 col-lg-4">
                <div class="card h-100">
                    <div class="card-header d-flex align-items-center gap-2">
                        <span>🔋</span> <span>Home Battery</span>
                    </div>
                    <div class="card-body">
                        <div class="d-flex justify-content-between mb-2">
                            <span class="text-secondary">State of Charge</span>
                            <strong>{{printf "%.1f" .Battery.SOC}}%</strong>
                        </div>
                        <div class="progress mb-3" style="height: 8px;">
                            <div class="progress-bar soc-gradient" style="width: {{printf "%.0f" .Battery.SOC}}%"></div>
                        </div>
                        <div class="power-flow">
                            <div class="power-item">
                                <span class="power-icon">☀️</span>
                                <span class="power-value text-positive">{{formatPower .Battery.PVPower}}</span>
                                <span class="power-label">Solar</span>
                            </div>
                            <div class="power-item">
                                <span class="power-icon">🏠</span>
                                <span class="power-value">{{formatPower .Battery.LoadPower}}</span>
                                <span class="power-label">Load</span>
                            </div>
                            <div class="power-item">
                                <span class="power-icon">🔌</span>
                                <span class="power-value {{if gt .Battery.GridPower 0.0}}text-negative{{else if lt .Battery.GridPower 0.0}}text-positive{{end}}">{{formatPower (abs .Battery.GridPower)}}</span>
                                <span class="power-label">Grid ({{powerDirection .Battery.GridPower}})</span>
                            </div>
                        </div>
                        <div class="d-flex justify-content-between border-top border-secondary pt-2">
                            <span class="text-secondary">Battery Power</span>
                            <strong class="{{if gt .Battery.BatteryPower 0.0}}text-charging{{else if lt .Battery.BatteryPower 0.0}}text-negative{{end}}">{{formatPower (abs .Battery.BatteryPower)}} ({{batteryDirection .Battery.BatteryPower}})</strong>
                        </div>
                    </div>
                </div>
            </div>
            {{end}}

            <!-- Energy Flow Diagram -->
            {{if or .Battery .Zappis}}
            <div class="col-12 col-lg-8">
                <div class="card">
                    <div class="card-header d-flex align-items-center gap-2">
                        <span>⚡</span> <span>Energy Flow</span>
                    </div>
                    <div class="card-body p-0">
                        <svg class="energy-flow-svg" viewBox="0 0 480 320">
                            <!-- Flow lines (background) -->
                            <path class="flow-line" d="M240,160 L240,50" />
                            <path class="flow-line" d="M240,160 L60,160" />
                            <path class="flow-line" d="M240,160 L420,160" />
                            <path class="flow-line" d="M240,160 L240,270" />
                            {{if .Battery}}
                            <path class="flow-line" d="M240,160 L90,260" />
                            {{end}}

                            <!-- Animated flow dots: Solar to Center -->
                            {{if .Battery}}{{if gt .Battery.PVPower 0.0}}
                            <path class="flow-dots flow-solar flow-animate-forward" d="M240,270 L240,160" />
                            {{end}}{{else}}{{if .Zappis}}{{with index .Zappis 0}}{{if gt .SolarPower 0.0}}
                            <path class="flow-dots flow-solar flow-animate-forward" d="M240,270 L240,160" />
                            {{end}}{{end}}{{end}}{{end}}

                            <!-- Animated flow dots: Grid -->
                            {{if .Battery}}
                                {{if gt .Battery.GridPower 0.0}}
                                <path class="flow-dots flow-grid flow-animate-forward" d="M420,160 L240,160" />
                                {{else if lt .Battery.GridPower 0.0}}
                                <path class="flow-dots flow-grid flow-animate-backward" d="M420,160 L240,160" />
                                {{end}}
                            {{else}}{{if .Zappis}}{{with index .Zappis 0}}
                                {{if .IsImporting}}
                                <path class="flow-dots flow-grid flow-animate-forward" d="M420,160 L240,160" />
                                {{else if .IsExporting}}
                                <path class="flow-dots flow-grid flow-animate-backward" d="M420,160 L240,160" />
                                {{end}}
                            {{end}}{{end}}{{end}}

                            <!-- Animated flow dots: Center to House -->
                            {{if .Battery}}{{if gt .Battery.LoadPower 0.0}}
                            <path class="flow-dots flow-house flow-animate-backward" d="M240,50 L240,160" />
                            {{end}}{{else}}{{if .Zappis}}{{with index .Zappis 0}}{{if gt .HouseConsumption 0.0}}
                            <path class="flow-dots flow-house flow-animate-backward" d="M240,50 L240,160" />
                            {{end}}{{end}}{{end}}{{end}}

                            <!-- Animated flow dots: Center to Zappi -->
                            {{if .Zappis}}{{with index .Zappis 0}}{{if gt .ChargerPower 0.0}}
                            <path class="flow-dots flow-zappi flow-animate-backward" d="M60,160 L240,160" />
                            {{end}}{{end}}{{end}}

                            <!-- Animated flow dots: Battery -->
                            {{if .Battery}}
                                {{if gt .Battery.BatteryPower 0.0}}
                                <path class="flow-dots flow-battery flow-animate-backward" d="M90,260 L240,160" />
                                {{else if lt .Battery.BatteryPower 0.0}}
                                <path class="flow-dots flow-battery flow-animate-forward" d="M90,260 L240,160" />
                                {{end}}
                            {{end}}

                            <!-- Center hub -->
                            <circle class="energy-node-bg" cx="240" cy="160" r="22" />
                            <circle class="energy-node node-center" cx="240" cy="160" r="22" />
                            <text class="energy-node-icon" x="240" y="160" style="fill:#3fb950;">●</text>

                            <!-- House node (top) -->
                            <circle class="energy-node-bg" cx="240" cy="30" r="30" />
                            <circle class="energy-node node-house" cx="240" cy="30" r="30" />
                            <text x="240" y="28" class="energy-node-icon" style="fill:#da3cda; font-size:20px;">🏠</text>
                            {{if .Battery}}
                            <text x="300" y="25" class="energy-label">{{formatPower .Battery.LoadPower}}</text>
                            <text x="300" y="40" class="energy-sublabel">House</text>
                            {{else}}{{if .Zappis}}{{with index .Zappis 0}}
                            <text x="300" y="25" class="energy-label">{{formatPower .HouseConsumption}}</text>
                            <text x="300" y="40" class="energy-sublabel">House</text>
                            {{end}}{{end}}{{end}}

                            <!-- Zappi node (left) -->
                            {{if .Zappis}}{{with index .Zappis 0}}
                            <circle class="energy-node-bg" cx="40" cy="160" r="30" />
                            <circle class="energy-node node-zappi" cx="40" cy="160" r="30" />
                            <text x="40" y="158" class="energy-node-icon" style="fill:#3b82f6; font-size:20px;">🚗</text>
                            <text x="40" y="115" class="energy-label">{{formatPower .ChargerPower}}</text>
                            <text x="40" y="130" class="energy-sublabel">{{.Status}}</text>
                            {{end}}{{end}}

                            <!-- Grid node (right) -->
                            <circle class="energy-node-bg" cx="440" cy="160" r="30" />
                            <circle class="energy-node node-grid" cx="440" cy="160" r="30" />
                            <text x="440" y="158" class="energy-node-icon" style="fill:#f97316; font-size:20px;">🔌</text>
                            {{if .Battery}}
                            <text x="440" y="115" class="energy-label">{{formatPower (abs .Battery.GridPower)}}</text>
                            <text x="440" y="130" class="energy-sublabel">{{if gt .Battery.GridPower 0.0}}Import{{else if lt .Battery.GridPower 0.0}}Export{{else}}--{{end}}</text>
                            {{else}}{{if .Zappis}}{{with index .Zappis 0}}
                            <text x="440" y="115" class="energy-label">{{formatPower (abs .GridPower)}}</text>
                            <text x="440" y="130" class="energy-sublabel">{{if .IsImporting}}Import{{else if .IsExporting}}Export{{else}}--{{end}}</text>
                            {{end}}{{end}}{{end}}

                            <!-- Solar node (bottom center) -->
                            <circle class="energy-node-bg" cx="240" cy="290" r="30" />
                            <circle class="energy-node node-solar" cx="240" cy="290" r="30" />
                            <text x="240" y="288" class="energy-node-icon" style="fill:#3fb950; font-size:20px;">☀️</text>
                            {{if .Battery}}
                            <text x="300" y="285" class="energy-label">{{formatPower .Battery.PVPower}}</text>
                            <text x="300" y="300" class="energy-sublabel">Solar</text>
                            {{else}}{{if .Zappis}}{{with index .Zappis 0}}
                            <text x="300" y="285" class="energy-label">{{formatPower .SolarPower}}</text>
                            <text x="300" y="300" class="energy-sublabel">Solar</text>
                            {{end}}{{end}}{{end}}

                            <!-- Battery node (bottom left) -->
                            {{if .Battery}}
                            <circle class="energy-node-bg" cx="70" cy="270" r="30" />
                            <circle class="energy-node node-battery" cx="70" cy="270" r="30" />
                            <text x="70" y="268" class="energy-node-icon" style="fill:#22d3ee; font-size:20px;">🔋</text>
                            <text x="130" y="255" class="energy-label">{{printf "%.0f" .Battery.SOC}}%</text>
                            <text x="130" y="270" class="energy-sublabel">{{formatPower (abs .Battery.BatteryPower)}}</text>
                            <text x="130" y="285" class="energy-sublabel">{{if gt .Battery.BatteryPower 0.0}}Charging{{else if lt .Battery.BatteryPower 0.0}}Discharging{{else}}Idle{{end}}</text>
                            {{end}}
                        </svg>
                    </div>
                </div>
            </div>
            {{end}}

            {{range .Zappis}}
            <div class="col-12 col-md-6 col-lg-4">
                <div class="card h-100">
                    <div class="card-header d-flex align-items-center gap-2">
                        <span>🚗</span> <span>Zappi {{.Serial}}</span>
                    </div>
                    <div class="card-body">
                        <ul class="list-group list-group-flush mb-3">
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Mode</span>
                                <strong>{{.Mode}}</strong>
                            </li>
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Status</span>
                                <strong>{{.Status}}</strong>
                            </li>
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Plug</span>
                                <strong>{{.PlugStatus}}</strong>
                            </li>
                        </ul>
                        <div class="power-flow">
                            <div class="power-item">
                                <span class="power-icon">☀️</span>
                                <span class="power-value text-positive">{{formatPower .SolarPower}}</span>
                                <span class="power-label">Solar</span>
                            </div>
                            <div class="power-item">
                                <span class="power-icon">🏠</span>
                                <span class="power-value">{{formatPower .HouseConsumption}}</span>
                                <span class="power-label">House</span>
                            </div>
                            <div class="power-item">
                                <span class="power-icon">🔌</span>
                                <span class="power-value {{if .IsImporting}}text-negative{{else if .IsExporting}}text-positive{{end}}">{{formatPower (abs .GridPower)}}</span>
                                <span class="power-label">Grid{{if .IsImporting}} (import){{else if .IsExporting}} (export){{end}}</span>
                            </div>
                        </div>
                        <ul class="list-group list-group-flush mb-3">
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">EV Charger</span>
                                <strong class="{{if gt .ChargerPower 0.0}}text-charging{{end}}">{{formatPower .ChargerPower}}</strong>
                            </li>
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Session Added</span>
                                <strong>{{printf "%.2f" .ChargeAdded}} kWh</strong>
                            </li>
                        </ul>
                        <div class="d-flex flex-wrap gap-2">
                            <a href="/api/zappi?action=fast&serial={{.Serial}}" class="btn btn-success btn-sm">Fast</a>
                            <a href="/api/zappi?action=eco&serial={{.Serial}}" class="btn btn-primary btn-sm">Eco</a>
                            <a href="/api/zappi?action=eco%2B&serial={{.Serial}}" class="btn btn-secondary btn-sm">Eco+</a>
                            <a href="/api/zappi?action=stop&serial={{.Serial}}" class="btn btn-danger btn-sm">Stop</a>
                            <a href="/api/zappi?action=boost&serial={{.Serial}}&kwh=5" class="btn btn-outline-secondary btn-sm">Boost 5kWh</a>
                        </div>
                    </div>
                </div>
            </div>
            {{end}}

            {{range .Vehicles}}
            <div class="col-12 col-md-6 col-lg-4">
                <div class="card h-100">
                    <div class="card-header d-flex align-items-center gap-2">
                        <span>🚙</span> <span>{{.Vehicle.Name}}</span>
                    </div>
                    <div class="card-body">
                        {{if .Charging}}{{if .Charging.Status}}
                        <div class="d-flex justify-content-between mb-2">
                            <span class="text-secondary">Battery</span>
                            <strong>{{.Charging.Status.Battery.StateOfChargePercent}}%</strong>
                        </div>
                        <div class="progress mb-3" style="height: 8px;">
                            <div class="progress-bar soc-gradient" style="width: {{.Charging.Status.Battery.StateOfChargePercent}}%"></div>
                        </div>
                        <ul class="list-group list-group-flush mb-3">
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Range</span>
                                <strong>{{printf "%.0f" (divideBy .Charging.Status.Battery.RemainingRangeMeters 1000.0)}} km</strong>
                            </li>
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Charging State</span>
                                <strong class="{{if eq .Charging.Status.State "CHARGING"}}text-charging{{end}}">{{.Charging.Status.State}}</strong>
                            </li>
                            {{if eq .Charging.Status.State "CHARGING"}}
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Charge Power</span>
                                <strong class="text-charging">{{printf "%.1f" .Charging.Status.ChargePowerKW}} kW</strong>
                            </li>
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Time to Full</span>
                                <strong>{{.Charging.Status.RemainingMinutesToFullyCharged}} min</strong>
                            </li>
                            {{end}}
                        </ul>
                        {{end}}{{end}}
                        {{if .Status}}
                        <ul class="list-group list-group-flush mb-3">
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Odometer</span>
                                <strong>{{.Status.Mileage}} km</strong>
                            </li>
                            {{if .Status.Doors.OverallStatus}}
                            <li class="list-group-item d-flex justify-content-between px-0">
                                <span class="text-secondary">Doors</span>
                                <strong class="{{if not .Status.Doors.Locked}}text-warning-custom{{end}}">{{if .Status.Doors.Locked}}Locked{{else}}Unlocked{{end}}</strong>
                            </li>
                            {{end}}
                        </ul>
                        {{end}}
                        <div class="d-flex flex-wrap gap-2">
                            <a href="/api/skoda?action=start&vin={{.Vehicle.VIN}}" class="btn btn-success btn-sm">Start Charging</a>
                            <a href="/api/skoda?action=stop&vin={{.Vehicle.VIN}}" class="btn btn-danger btn-sm">Stop Charging</a>
                        </div>
                    </div>
                </div>
            </div>
            {{end}}

            {{if .Prices}}
            <div class="col-12">
                <div class="card">
                    <div class="card-header d-flex justify-content-between align-items-center">
                        <div class="d-flex align-items-center gap-2">
                            <span>📊</span> <span>Energy Prices</span>
                        </div>
                        <div class="form-check form-switch mb-0">
                            <input class="form-check-input" type="checkbox" id="tax-toggle">
                            <label class="form-check-label text-secondary" for="tax-toggle">Include 21% BTW</label>
                        </div>
                    </div>
                    <div class="card-body">
                        <div id="price-chart" class="echart"></div>
                    </div>
                </div>
            </div>
            {{end}}
        </div>
    </div>

    <script src="https://cdn.jsdelivr.net/npm/bootstrap@5.3.3/dist/js/bootstrap.bundle.min.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js"></script>
    <script>
        {{if .Prices}}
        (function() {
            var chartDom = document.getElementById('price-chart');
            var chart = echarts.init(chartDom, 'dark');
            var taxToggle = document.getElementById('tax-toggle');
            var TAX_AMOUNT = 0.21; // EUR per kWh

            var allRaw = [
                {{range .Prices.Today}}
                { hour: '{{formatTime .Period}}', price: parseFloat('{{printf "%.6f" .PricePerKWh}}'), day: 'Today' },
                {{end}}
                {{if .Prices.Tomorrow}}
                {{range .Prices.Tomorrow}}
                { hour: '{{formatTime .Period}}', price: parseFloat('{{printf "%.6f" .PricePerKWh}}'), day: 'Tomorrow' },
                {{end}}
                {{end}}
            ];

            var currentHour = '{{if .CurrentPrice}}{{formatTime .CurrentPrice.Period}}{{end}}';
            var todayCount = {{len .Prices.Today}};
            var hasTomorrow = allRaw.length > todayCount;

            function renderChart() {
                var includeTax = taxToggle.checked;

                var allData = allRaw.map(function(d) {
                    var price = includeTax ? d.price + TAX_AMOUNT : d.price;
                    return { hour: d.hour, price: price, day: d.day };
                });

                var prices = allData.map(function(d) { return d.price; });
                var minPrice = Math.min.apply(null, prices);
                var maxPrice = Math.max.apply(null, prices);
                var range = maxPrice - minPrice;

                function getColor(price) {
                    if (range === 0) return '#d29922';
                    var pos = (price - minPrice) / range;
                    if (pos < 0.33) return '#3fb950';
                    if (pos < 0.66) return '#d29922';
                    return '#f85149';
                }

                var xLabels = [];
                var barData = [];

                allData.forEach(function(d, i) {
                    var isCurrent = (d.day === 'Today' && d.hour === currentHour);
                    xLabels.push(i);
                    barData.push({
                        value: d.price,
                        day: d.day,
                        hour: d.hour,
                        itemStyle: {
                            color: getColor(d.price),
                            borderRadius: [3, 3, 0, 0],
                            borderColor: isCurrent ? '#fff' : 'transparent',
                            borderWidth: isCurrent ? 2 : 0
                        }
                    });
                });

                var markLineData = [];
                if (hasTomorrow) {
                    markLineData.push({
                        xAxis: todayCount - 0.5,
                        lineStyle: { color: '#484f58', type: 'solid', width: 2 }
                    });
                }

                var option = {
                    backgroundColor: 'transparent',
                    tooltip: {
                        trigger: 'item',
                        backgroundColor: '#21262d',
                        borderColor: '#30363d',
                        padding: [12, 16],
                        textStyle: { color: '#c9d1d9', fontSize: 13 },
                        formatter: function(params) {
                            var d = params.data;
                            var taxLabel = includeTax ? 'incl. 21% BTW' : 'excl. BTW';
                            return '<div style="font-weight:600; margin-bottom:6px;">' + d.day + ' ' + d.hour + '</div>' +
                                   '<span style="font-size:1.4em; font-weight:700; color:' + d.itemStyle.color + '">€' + d.value.toFixed(4) + '</span>' +
                                   '<span style="color:#8b949e"> /kWh</span>' +
                                   '<div style="color:#8b949e; font-size:11px; margin-top:6px;">' + taxLabel + '</div>';
                        }
                    },
                    grid: { left: 55, right: 20, top: 20, bottom: 55 },
                    xAxis: {
                        type: 'category',
                        data: allData.map(function(d) {
                            var hourNum = parseInt(d.hour.split(':')[0]);
                            if (hourNum === 0) return d.day.substr(0, 3) + ' 00:00';
                            return d.hour;
                        }),
                        axisLabel: {
                            color: '#8b949e',
                            fontSize: 11,
                            hideOverlap: true
                        },
                        axisLine: { lineStyle: { color: '#30363d' } },
                        axisTick: { show: false }
                    },
                    yAxis: {
                        type: 'value',
                        name: includeTax ? '€/kWh (incl. BTW)' : '€/kWh',
                        nameTextStyle: { color: '#8b949e', fontSize: 11 },
                        nameGap: 10,
                        axisLabel: {
                            color: '#8b949e',
                            fontSize: 11,
                            formatter: function(v) { return v.toFixed(2); }
                        },
                        axisLine: { show: false },
                        splitLine: { lineStyle: { color: '#21262d' } }
                    },
                    series: [{
                        type: 'bar',
                        data: barData,
                        barCategoryGap: '25%',
                        markLine: {
                            silent: true,
                            symbol: 'none',
                            label: { show: hasTomorrow, position: 'insideStartTop', formatter: 'Tomorrow', color: '#8b949e', fontSize: 11 },
                            data: markLineData
                        }
                    }]
                };

                chart.clear();
                chart.setOption(option);
            }

            renderChart();
            taxToggle.addEventListener('change', renderChart);
            window.addEventListener('resize', function() { chart.resize(); });
        })();
        {{end}}

        setTimeout(function() { window.location.reload(); }, 60000);
    </script>
</body>
</html>`
