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
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
    <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.3/dist/css/bootstrap.min.css" rel="stylesheet">
    <script src="https://unpkg.com/lucide@latest"></script>
    <style>
        :root {
            --bg-primary: #0f1419;
            --bg-secondary: #1a1f2e;
            --bg-card: #1e2433;
            --bg-card-header: #252b3b;
            --border-color: #2d3548;
            --text-primary: #e6edf3;
            --text-secondary: #8b949e;
            --text-muted: #6e7681;
            --accent-green: #3fb950;
            --accent-red: #f85149;
            --accent-blue: #58a6ff;
            --accent-yellow: #d29922;
            --accent-cyan: #22d3ee;
            --accent-orange: #f97316;
            --accent-purple: #a855f7;
        }
        * { font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif; }
        body {
            background: var(--bg-primary);
            color: var(--text-primary);
            line-height: 1.6;
        }
        .card {
            background: var(--bg-card);
            border: 1px solid var(--border-color);
            border-radius: 12px;
            box-shadow: 0 4px 6px -1px rgba(0, 0, 0, 0.2);
            transition: box-shadow 0.2s ease;
        }
        .card:hover {
            box-shadow: 0 8px 12px -2px rgba(0, 0, 0, 0.3);
        }
        .card-header {
            background: var(--bg-card-header);
            border-bottom: 1px solid var(--border-color);
            border-radius: 12px 12px 0 0 !important;
            font-weight: 600;
            padding: 1rem 1.25rem;
        }
        .card-body { padding: 1.25rem; }
        .list-group-item {
            background: transparent;
            border-color: var(--border-color);
            padding: 0.875rem 0;
        }
        .text-positive { color: var(--accent-green); }
        .text-negative { color: var(--accent-red); }
        .text-charging { color: var(--accent-blue); }
        .text-warning-custom { color: var(--accent-yellow); }
        .text-secondary { color: var(--text-secondary) !important; }
        .echart { width: 100%; height: 300px; }
        .soc-gradient {
            background: linear-gradient(90deg, var(--accent-red), var(--accent-yellow), var(--accent-green));
            border-radius: 6px;
        }
        .progress { background: var(--bg-secondary); border-radius: 6px; }
        /* Buttons */
        .btn {
            border-radius: 8px;
            font-weight: 500;
            padding: 0.5rem 1rem;
            transition: all 0.2s ease;
        }
        .btn-sm { padding: 0.375rem 0.75rem; font-size: 0.875rem; }
        .btn-outline-secondary {
            border-color: var(--border-color);
            color: var(--text-secondary);
        }
        .btn-outline-secondary:hover {
            background: var(--bg-secondary);
            border-color: var(--text-secondary);
            color: var(--text-primary);
        }
        /* Navbar */
        .navbar {
            background: var(--bg-secondary) !important;
            border-bottom: 1px solid var(--border-color) !important;
            padding: 0.875rem 0;
        }
        .navbar-brand { font-weight: 700; font-size: 1.25rem; }
        .badge {
            font-weight: 600;
            padding: 0.5rem 0.75rem;
            border-radius: 6px;
        }
        .summary-value { font-size: 1.5rem; font-weight: 700; letter-spacing: -0.02em; }
        .summary-label { font-size: 0.75rem; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; }
        /* Icon styling */
        .icon-header { width: 20px; height: 20px; stroke-width: 2; }
        .icon-sm { width: 16px; height: 16px; }
        /* Energy Flow Diagram */
        .energy-flow-svg { width: 100%; height: 320px; }
        .energy-node { fill: none; stroke-width: 3; }
        .energy-node-bg { fill: var(--bg-card); }
        .energy-node-icon { fill: var(--text-secondary); font-size: 24px; dominant-baseline: central; text-anchor: middle; }
        .energy-label { fill: var(--text-primary); font-size: 13px; font-weight: 600; text-anchor: middle; }
        .energy-sublabel { fill: var(--text-secondary); font-size: 11px; text-anchor: middle; }
        .flow-line { fill: none; stroke: var(--border-color); stroke-width: 3; }
        .flow-line-active { stroke-width: 3; stroke-linecap: round; }
        .flow-dots { fill: none; stroke-width: 4; stroke-linecap: round; stroke-dasharray: 0 12; }
        @keyframes flowForward { 0% { stroke-dashoffset: 24; } 100% { stroke-dashoffset: 0; } }
        @keyframes flowBackward { 0% { stroke-dashoffset: 0; } 100% { stroke-dashoffset: 24; } }
        .flow-animate-forward { animation: flowForward 0.8s linear infinite; }
        .flow-animate-backward { animation: flowBackward 0.8s linear infinite; }
        .node-house { stroke: var(--accent-purple); }
        .node-zappi { stroke: var(--accent-blue); }
        .node-grid { stroke: var(--accent-orange); }
        .node-solar { stroke: var(--accent-green); }
        .node-battery { stroke: var(--accent-cyan); }
        .node-center { stroke: var(--accent-green); }
        .flow-grid { stroke: var(--accent-orange); }
        .flow-solar { stroke: var(--accent-green); }
        .flow-zappi { stroke: var(--accent-blue); }
        .flow-house { stroke: var(--accent-purple); }
        .flow-battery { stroke: var(--accent-cyan); }
        /* Responsive */
        @media (max-width: 576px) {
            .energy-flow-svg { height: 280px; }
            .energy-label { font-size: 11px; }
            .energy-sublabel { font-size: 9px; }
            .summary-value { font-size: 1.25rem; }
        }
    </style>
</head>
<body>
    <nav class="navbar navbar-dark bg-dark border-bottom border-secondary mb-3">
        <div class="container-fluid">
            <span class="navbar-brand mb-0 h1">Home Control</span>
            <div class="d-flex align-items-center gap-3">
                {{if .CurrentPrice}}<span class="badge bg-secondary">{{formatPrice .CurrentPrice.PriceEUR}} /kWh</span>{{end}}
                <small class="text-secondary d-none d-md-inline">Updated: {{formatDateTime .LastUpdate}}</small>
                <a href="/api/refresh" class="btn btn-outline-secondary btn-sm">Refresh</a>
            </div>
        </div>
    </nav>

    <div class="container-fluid">
        <div class="row g-4">
            <!-- Energy Flow Diagram -->
            {{if or .Battery .Zappis}}
            <div class="col-12">
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

                            <!-- Animated flow dots: Solar to Center (from Zappi) -->
                            {{if .Zappis}}{{with index .Zappis 0}}{{if gt .SolarPower 0.0}}
                            <path class="flow-dots flow-solar flow-animate-forward" d="M240,270 L240,160" />
                            {{end}}{{end}}{{end}}

                            <!-- Animated flow dots: Grid (from Zappi) -->
                            {{if .Zappis}}{{with index .Zappis 0}}
                                {{if .IsImporting}}
                                <path class="flow-dots flow-grid flow-animate-forward" d="M420,160 L240,160" />
                                {{else if .IsExporting}}
                                <path class="flow-dots flow-grid flow-animate-backward" d="M420,160 L240,160" />
                                {{end}}
                            {{end}}{{end}}

                            <!-- Animated flow dots: Center to House -->
                            {{if .Zappis}}{{with index .Zappis 0}}
                                {{if gt .HouseConsumption 0.0}}
                                <path class="flow-dots flow-house flow-animate-backward" d="M240,50 L240,160" />
                                {{end}}
                            {{end}}{{end}}

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

                            <!-- House node (top) - Zappi house consumption minus battery power -->
                            <circle class="energy-node-bg" cx="240" cy="30" r="30" />
                            <circle class="energy-node node-house" cx="240" cy="30" r="30" />
                            <text x="240" y="28" class="energy-node-icon" style="fill:#da3cda; font-size:20px;">🏠</text>
                            {{if .Zappis}}{{with index .Zappis 0}}
                                <text x="300" y="25" class="energy-label" id="house-power">{{formatPower .HouseConsumption}}</text>
                                <text x="300" y="40" class="energy-sublabel">House</text>
                            {{end}}{{end}}

                            <!-- Zappi/Car node (left) - show vehicle SOC and Zappi status -->
                            {{if .Zappis}}{{with index .Zappis 0}}
                            <circle class="energy-node-bg" cx="40" cy="160" r="30" />
                            <circle class="energy-node node-zappi" cx="40" cy="160" r="30" />
                            <text x="40" y="158" class="energy-node-icon" style="fill:#3b82f6; font-size:20px;">🚗</text>
                            {{if $.Vehicles}}{{with index $.Vehicles 0}}{{if .Charging}}{{if .Charging.Status}}
                            <text x="40" y="100" class="energy-label">{{.Charging.Status.Battery.StateOfChargePercent}}%</text>
                            {{end}}{{end}}{{end}}{{end}}
                            <text x="40" y="115" class="energy-sublabel">{{formatPower .ChargerPower}}</text>
                            <text x="40" y="130" class="energy-sublabel">{{.Status}}</text>
                            {{end}}{{end}}

                            <!-- Grid node (right) - from Zappi -->
                            <circle class="energy-node-bg" cx="440" cy="160" r="30" />
                            <circle class="energy-node node-grid" cx="440" cy="160" r="30" />
                            <text x="440" y="158" class="energy-node-icon" style="fill:#f97316; font-size:20px;">🔌</text>
                            {{if .Zappis}}{{with index .Zappis 0}}
                            <text x="440" y="115" class="energy-label">{{formatPower (abs .GridPower)}}</text>
                            <text x="440" y="130" class="energy-sublabel">{{if .IsImporting}}Import{{else if .IsExporting}}Export{{else}}--{{end}}</text>
                            {{end}}{{end}}

                            <!-- Solar node (bottom center) - from Zappi -->
                            <circle class="energy-node-bg" cx="240" cy="290" r="30" />
                            <circle class="energy-node node-solar" cx="240" cy="290" r="30" />
                            <text x="240" y="288" class="energy-node-icon" style="fill:#3fb950; font-size:20px;">☀️</text>
                            {{if .Zappis}}{{with index .Zappis 0}}
                            <text x="300" y="285" class="energy-label">{{formatPower .SolarPower}}</text>
                            <text x="300" y="300" class="energy-sublabel">Solar</text>
                            {{end}}{{end}}

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
                        <span>🔌</span> <span>Zappi {{.Serial}}</span>
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
                            <a href="/api/skoda?action=wakeup&vin={{.Vehicle.VIN}}" class="btn btn-outline-warning btn-sm" title="Max 3x per day">Wake Up</a>
                        </div>
                    </div>
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
                        <div class="d-flex justify-content-between">
                            <span class="text-secondary">Battery Power</span>
                            <strong class="{{if gt .Battery.BatteryPower 0.0}}text-charging{{else if lt .Battery.BatteryPower 0.0}}text-negative{{end}}">{{formatPower (abs .Battery.BatteryPower)}} ({{batteryDirection .Battery.BatteryPower}})</strong>
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

        // Update house power by subtracting battery power
        {{if and .Zappis .Battery}}
        (function() {
            var housePowerEl = document.getElementById('house-power');
            if (housePowerEl) {
                var zappiHouse = {{with index .Zappis 0}}{{.HouseConsumption}}{{end}};
                var batteryPower = {{.Battery.BatteryPower}};
                var actualHouse = zappiHouse - batteryPower;

                function formatPower(p) {
                    if (p >= 1000 || p <= -1000) {
                        return (p/1000).toFixed(2) + ' kW';
                    }
                    return Math.round(p) + ' W';
                }
                housePowerEl.textContent = formatPower(actualHouse);
            }
        })();
        {{end}}

        setTimeout(function() { window.location.reload(); }, 60000);
    </script>
</body>
</html>`
