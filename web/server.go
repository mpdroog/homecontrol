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
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Home Control Dashboard</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            background: #1a1a2e;
            color: #eee;
            padding: 20px;
            min-height: 100vh;
        }
        .header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 20px;
            padding-bottom: 10px;
            border-bottom: 1px solid #333;
        }
        .header h1 { font-size: 1.5rem; }
        .last-update { color: #888; font-size: 0.9rem; }
        .refresh-btn {
            background: #4a4a6a;
            color: #fff;
            border: none;
            padding: 8px 16px;
            border-radius: 4px;
            cursor: pointer;
            text-decoration: none;
        }
        .refresh-btn:hover { background: #5a5a7a; }
        .grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
            gap: 20px;
        }
        .card {
            background: #16213e;
            border-radius: 12px;
            padding: 20px;
            box-shadow: 0 4px 6px rgba(0,0,0,0.3);
        }
        .card h2 {
            font-size: 1.1rem;
            margin-bottom: 15px;
            color: #7f8fa6;
            display: flex;
            align-items: center;
            gap: 8px;
        }
        .card h2 .icon { font-size: 1.3rem; }
        .stat {
            display: flex;
            justify-content: space-between;
            padding: 8px 0;
            border-bottom: 1px solid #2a2a4a;
        }
        .stat:last-child { border-bottom: none; }
        .stat-label { color: #888; }
        .stat-value { font-weight: 600; }
        .stat-value.positive { color: #4cd137; }
        .stat-value.negative { color: #e84118; }
        .stat-value.charging { color: #00a8ff; }
        .stat-value.warning { color: #fbc531; }
        .buttons {
            display: flex;
            flex-wrap: wrap;
            gap: 8px;
            margin-top: 15px;
        }
        .btn {
            background: #4a4a6a;
            color: #fff;
            border: none;
            padding: 8px 16px;
            border-radius: 6px;
            cursor: pointer;
            text-decoration: none;
            font-size: 0.9rem;
            transition: background 0.2s;
        }
        .btn:hover { background: #5a5a7a; }
        .btn.primary { background: #00a8ff; }
        .btn.primary:hover { background: #0097e6; }
        .btn.danger { background: #e84118; }
        .btn.danger:hover { background: #c23616; }
        .btn.success { background: #4cd137; }
        .btn.success:hover { background: #44bd32; }
        .price-list {
            max-height: 300px;
            overflow-y: auto;
        }
        .price-row {
            display: flex;
            justify-content: space-between;
            padding: 4px 0;
            font-size: 0.85rem;
        }
        .price-row.current { background: #2a2a4a; border-radius: 4px; padding: 4px 8px; }
        .price-row.low { color: #4cd137; }
        .price-row.high { color: #e84118; }
        .soc-bar {
            height: 8px;
            background: #2a2a4a;
            border-radius: 4px;
            margin-top: 8px;
            overflow: hidden;
        }
        .soc-fill {
            height: 100%;
            background: linear-gradient(90deg, #e84118, #fbc531, #4cd137);
            border-radius: 4px;
            transition: width 0.3s;
        }
        .power-flow {
            display: flex;
            justify-content: space-around;
            text-align: center;
            margin: 15px 0;
        }
        .power-item {
            display: flex;
            flex-direction: column;
            align-items: center;
        }
        .power-icon { font-size: 2rem; margin-bottom: 5px; }
        .power-value { font-size: 1.2rem; font-weight: 600; }
        .power-label { font-size: 0.8rem; color: #888; }
        .chart-container {
            width: 100%;
            height: 200px;
            margin-top: 15px;
        }
        .echart {
            width: 100%;
            height: 280px;
        }
        .card.wide {
            grid-column: span 2;
        }
        @media (max-width: 768px) {
            .grid { grid-template-columns: 1fr; }
            body { padding: 10px; }
            .card.wide { grid-column: span 1; }
        }
    </style>
</head>
<body>
    <div class="header">
        <h1>Home Control</h1>
        <div>
            <span class="last-update">Updated: {{formatDateTime .LastUpdate}}</span>
            <a href="/api/refresh" class="refresh-btn">Refresh</a>
        </div>
    </div>

    <div class="grid">
        {{if .Prices}}
        <div class="card">
            <h2><span class="icon">⚡</span> Energy Prices</h2>
            {{if .CurrentPrice}}
            <div class="stat">
                <span class="stat-label">Current ({{formatTime .CurrentPrice.Period}})</span>
                <span class="stat-value">€{{formatPrice .CurrentPrice.PriceEUR}}/kWh</span>
            </div>
            {{end}}
            {{if .NextPrice}}
            <div class="stat">
                <span class="stat-label">Next Hour</span>
                <span class="stat-value">€{{formatPrice .NextPrice.PriceEUR}}/kWh</span>
            </div>
            {{end}}
            {{if .LowestPrice}}
            <div class="stat">
                <span class="stat-label">Lowest ({{formatTime .LowestPrice.Period}})</span>
                <span class="stat-value positive">€{{formatPrice .LowestPrice.PriceEUR}}/kWh</span>
            </div>
            {{end}}
            {{if .HighestPrice}}
            <div class="stat">
                <span class="stat-label">Highest ({{formatTime .HighestPrice.Period}})</span>
                <span class="stat-value negative">€{{formatPrice .HighestPrice.PriceEUR}}/kWh</span>
            </div>
            {{end}}
        </div>
        {{end}}

        {{if .Battery}}
        <div class="card">
            <h2><span class="icon">🔋</span> Home Battery</h2>
            <div class="stat">
                <span class="stat-label">State of Charge</span>
                <span class="stat-value">{{printf "%.1f" .Battery.SOC}}%</span>
            </div>
            <div class="soc-bar">
                <div class="soc-fill" style="width: {{printf "%.0f" .Battery.SOC}}%"></div>
            </div>
            <div class="power-flow">
                <div class="power-item">
                    <span class="power-icon">☀️</span>
                    <span class="power-value positive">{{formatPower .Battery.PVPower}}</span>
                    <span class="power-label">Solar</span>
                </div>
                <div class="power-item">
                    <span class="power-icon">🏠</span>
                    <span class="power-value">{{formatPower .Battery.LoadPower}}</span>
                    <span class="power-label">Load</span>
                </div>
                <div class="power-item">
                    <span class="power-icon">🔌</span>
                    <span class="power-value {{if gt .Battery.GridPower 0.0}}negative{{else if lt .Battery.GridPower 0.0}}positive{{end}}">{{formatPower (abs .Battery.GridPower)}}</span>
                    <span class="power-label">Grid ({{powerDirection .Battery.GridPower}})</span>
                </div>
            </div>
            <div class="stat">
                <span class="stat-label">Battery Power</span>
                <span class="stat-value {{if gt .Battery.BatteryPower 0.0}}charging{{else if lt .Battery.BatteryPower 0.0}}negative{{end}}">{{formatPower (abs .Battery.BatteryPower)}} ({{batteryDirection .Battery.BatteryPower}})</span>
            </div>
        </div>
        {{end}}

        {{range .Zappis}}
        <div class="card">
            <h2><span class="icon">🚗</span> Zappi {{.Serial}}</h2>
            <div class="stat">
                <span class="stat-label">Mode</span>
                <span class="stat-value">{{.Mode}}</span>
            </div>
            <div class="stat">
                <span class="stat-label">Status</span>
                <span class="stat-value">{{.Status}}</span>
            </div>
            <div class="stat">
                <span class="stat-label">Plug</span>
                <span class="stat-value">{{.PlugStatus}}</span>
            </div>
            <div class="power-flow">
                <div class="power-item">
                    <span class="power-icon">☀️</span>
                    <span class="power-value positive">{{formatPower .SolarPower}}</span>
                    <span class="power-label">Solar</span>
                </div>
                <div class="power-item">
                    <span class="power-icon">🏠</span>
                    <span class="power-value">{{formatPower .HouseConsumption}}</span>
                    <span class="power-label">House</span>
                </div>
                <div class="power-item">
                    <span class="power-icon">🔌</span>
                    <span class="power-value {{if .IsImporting}}negative{{else if .IsExporting}}positive{{end}}">{{formatPower (abs .GridPower)}}</span>
                    <span class="power-label">Grid{{if .IsImporting}} (import){{else if .IsExporting}} (export){{end}}</span>
                </div>
            </div>
            <div class="stat">
                <span class="stat-label">EV Charger</span>
                <span class="stat-value {{if gt .ChargerPower 0.0}}charging{{end}}">{{formatPower .ChargerPower}}</span>
            </div>
            <div class="stat">
                <span class="stat-label">Session Added</span>
                <span class="stat-value">{{printf "%.2f" .ChargeAdded}} kWh</span>
            </div>
            <div class="buttons">
                <a href="/api/zappi?action=fast&serial={{.Serial}}" class="btn success">Fast</a>
                <a href="/api/zappi?action=eco&serial={{.Serial}}" class="btn primary">Eco</a>
                <a href="/api/zappi?action=eco%2B&serial={{.Serial}}" class="btn">Eco+</a>
                <a href="/api/zappi?action=stop&serial={{.Serial}}" class="btn danger">Stop</a>
                <a href="/api/zappi?action=boost&serial={{.Serial}}&kwh=5" class="btn">Boost 5kWh</a>
            </div>
        </div>
        {{end}}

        {{range .Vehicles}}
        <div class="card">
            <h2><span class="icon">🚙</span> {{.Vehicle.Name}}</h2>
            {{if .Charging}}{{if .Charging.Status}}
            <div class="stat">
                <span class="stat-label">Battery</span>
                <span class="stat-value">{{.Charging.Status.Battery.StateOfChargePercent}}%</span>
            </div>
            <div class="soc-bar">
                <div class="soc-fill" style="width: {{.Charging.Status.Battery.StateOfChargePercent}}%"></div>
            </div>
            <div class="stat">
                <span class="stat-label">Range</span>
                <span class="stat-value">{{printf "%.0f" (divideBy .Charging.Status.Battery.RemainingRangeMeters 1000.0)}} km</span>
            </div>
            <div class="stat">
                <span class="stat-label">Charging State</span>
                <span class="stat-value {{if eq .Charging.Status.State "CHARGING"}}charging{{end}}">{{.Charging.Status.State}}</span>
            </div>
            {{if eq .Charging.Status.State "CHARGING"}}
            <div class="stat">
                <span class="stat-label">Charge Power</span>
                <span class="stat-value charging">{{printf "%.1f" .Charging.Status.ChargePowerKW}} kW</span>
            </div>
            <div class="stat">
                <span class="stat-label">Time to Full</span>
                <span class="stat-value">{{.Charging.Status.RemainingMinutesToFullyCharged}} min</span>
            </div>
            {{end}}
            {{end}}{{end}}
            {{if .Status}}
            <div class="stat">
                <span class="stat-label">Odometer</span>
                <span class="stat-value">{{.Status.Mileage}} km</span>
            </div>
            {{if .Status.Doors.OverallStatus}}
            <div class="stat">
                <span class="stat-label">Doors</span>
                <span class="stat-value {{if not .Status.Doors.Locked}}warning{{end}}">{{if .Status.Doors.Locked}}Locked{{else}}Unlocked{{end}}</span>
            </div>
            {{end}}
            {{end}}
            <div class="buttons">
                <a href="/api/skoda?action=start&vin={{.Vehicle.VIN}}" class="btn success">Start Charging</a>
                <a href="/api/skoda?action=stop&vin={{.Vehicle.VIN}}" class="btn danger">Stop Charging</a>
            </div>
        </div>
        {{end}}

        {{if .Prices}}
        <div class="card wide">
            <h2><span class="icon">📊</span> Energy Prices</h2>
            <div style="margin-bottom: 10px;">
                <label style="cursor: pointer; color: #888; font-size: 0.9rem;">
                    <input type="checkbox" id="tax-toggle" style="margin-right: 6px;">
                    Include 21% BTW
                </label>
            </div>
            <div id="price-chart" class="echart"></div>
        </div>
        {{end}}
    </div>

    <script src="https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js"></script>
    <script>
        {{if .Prices}}
        (function() {
            var chartDom = document.getElementById('price-chart');
            var chart = echarts.init(chartDom, 'dark');
            var taxToggle = document.getElementById('tax-toggle');
            var TAX_RATE = 1.21;

            // Raw price data with day labels
            var allRaw = [
                {{range .Prices.Today}}
                { hour: '{{formatTime .Period}}', price: {{printf "%.6f" .PricePerKWh}}, day: 'Today' },
                {{end}}
                {{if .Prices.Tomorrow}}
                {{range .Prices.Tomorrow}}
                { hour: '{{formatTime .Period}}', price: {{printf "%.6f" .PricePerKWh}}, day: 'Tomorrow' },
                {{end}}
                {{end}}
            ];

            var currentHour = '{{if .CurrentPrice}}{{formatTime .CurrentPrice.Period}}{{end}}';
            var todayCount = {{len .Prices.Today}};
            var hasTomorrow = allRaw.length > todayCount;

            function renderChart() {
                var includeTax = taxToggle.checked;
                var taxMult = includeTax ? TAX_RATE : 1;

                // Apply tax to all data
                var allData = allRaw.map(function(d) {
                    return { hour: d.hour, price: d.price * taxMult, day: d.day };
                });

                // Find min/max for coloring
                var prices = allData.map(function(d) { return d.price; });
                var minPrice = Math.min.apply(null, prices);
                var maxPrice = Math.max.apply(null, prices);
                var range = maxPrice - minPrice;

                function getColor(price) {
                    if (range === 0) return '#fbc531';
                    var pos = (price - minPrice) / range;
                    if (pos < 0.33) return '#4cd137';
                    if (pos < 0.66) return '#fbc531';
                    return '#e84118';
                }

                // Build x-axis labels and bar data
                var xLabels = [];
                var barData = [];

                allData.forEach(function(d, i) {
                    var isCurrent = (d.day === 'Today' && d.hour === currentHour);

                    // Label format: show day at 00:00, otherwise just hour
                    if (d.hour === '00:00') {
                        xLabels.push(d.day + '\n00:00');
                    } else {
                        xLabels.push(d.hour);
                    }

                    barData.push({
                        value: d.price,
                        day: d.day,
                        hour: d.hour,
                        itemStyle: {
                            color: getColor(d.price),
                            borderRadius: [2, 2, 0, 0],
                            borderColor: isCurrent ? '#fff' : 'transparent',
                            borderWidth: isCurrent ? 2 : 0
                        }
                    });
                });

                // Mark line to separate days
                var markLineData = [];
                if (hasTomorrow) {
                    markLineData.push({
                        xAxis: todayCount - 0.5,
                        lineStyle: { color: '#888', type: 'solid', width: 2 }
                    });
                }

                var option = {
                    backgroundColor: 'transparent',
                    tooltip: {
                        trigger: 'item',
                        backgroundColor: 'rgba(0,0,0,0.9)',
                        borderColor: '#555',
                        padding: [10, 14],
                        textStyle: { color: '#fff', fontSize: 13 },
                        formatter: function(params) {
                            var d = params.data;
                            var taxLabel = includeTax ? ' (incl. BTW)' : ' (excl. BTW)';
                            return '<div style="font-weight:600; margin-bottom:4px;">' + d.day + ' ' + d.hour + '</div>' +
                                   '<span style="font-size:1.3em; font-weight:700; color:' + d.itemStyle.color + '">€' + d.value.toFixed(4) + '</span>' +
                                   '<span style="color:#aaa"> /kWh</span>' +
                                   '<div style="color:#888; font-size:11px; margin-top:4px;">' + taxLabel + '</div>';
                        }
                    },
                    grid: {
                        left: 50,
                        right: 15,
                        top: 20,
                        bottom: 50
                    },
                    xAxis: {
                        type: 'category',
                        data: xLabels,
                        axisLabel: {
                            color: '#999',
                            fontSize: 11,
                            interval: function(index) {
                                // Show label every 4 hours, and at day boundaries
                                var hour = allData[index].hour;
                                if (hour === '00:00') return true;
                                var hourNum = parseInt(hour.split(':')[0]);
                                return hourNum % 4 === 0;
                            }
                        },
                        axisLine: { lineStyle: { color: '#444' } },
                        axisTick: { show: false }
                    },
                    yAxis: {
                        type: 'value',
                        name: includeTax ? '€/kWh +BTW' : '€/kWh',
                        nameTextStyle: { color: '#888', fontSize: 11 },
                        nameGap: 8,
                        axisLabel: {
                            color: '#888',
                            fontSize: 11,
                            formatter: function(v) { return v.toFixed(2); }
                        },
                        axisLine: { show: false },
                        splitLine: { lineStyle: { color: '#2a2a4a' } }
                    },
                    series: [{
                        type: 'bar',
                        data: barData,
                        barCategoryGap: '20%',
                        markLine: {
                            silent: true,
                            symbol: 'none',
                            label: {
                                show: hasTomorrow,
                                position: 'insideStartTop',
                                formatter: 'Tomorrow',
                                color: '#888',
                                fontSize: 10
                            },
                            data: markLineData
                        }
                    }]
                };

                chart.setOption(option, true);
            }

            // Initial render
            renderChart();

            // Toggle tax
            taxToggle.addEventListener('change', renderChart);

            // Resize handler
            window.addEventListener('resize', function() { chart.resize(); });
        })();
        {{end}}

        // Auto-refresh every 60 seconds
        setTimeout(function() {
            window.location.reload();
        }, 60000);
    </script>
</body>
</html>`
