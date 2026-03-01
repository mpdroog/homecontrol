package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mpdroog/homecontrol/alphaess"
	"github.com/mpdroog/homecontrol/collector"
	"github.com/mpdroog/homecontrol/myenergi"
	"github.com/mpdroog/homecontrol/myskoda"
	"github.com/mpdroog/homecontrol/nordpool"
	"github.com/mpdroog/homecontrol/web"
)

type Config struct {
	MySkoda   MySkodaConfig   `toml:"myskoda"`
	AlphaESS  AlphaESSConfig  `toml:"alphaess"`
	MyEnergi  MyEnergiConfig  `toml:"myenergi"`
	Server    ServerConfig    `toml:"server"`
}

type ServerConfig struct {
	Listen  string `toml:"listen"`
	DataDir string `toml:"datadir"`
}

type MySkodaConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

type AlphaESSConfig struct {
	AppID     string `toml:"appid"`
	AppSecret string `toml:"appsecret"`
	SN        string `toml:"sn"`
}

type MyEnergiConfig struct {
	HubSerial string `toml:"hubserial"`
	Password  string `toml:"password"`
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: homecontrol [options] [command]

Commands:
  status              Show battery/charging status (default)
  start [VIN]         Start charging (uses first vehicle if VIN not specified)
  stop [VIN]          Stop charging (uses first vehicle if VIN not specified)
  limit [VIN] PCT     Set charge limit to PCT percent
  prices              Show hourly energy prices (today & tomorrow)
  battery             Show AlphaESS home battery status
  zappi               Show Zappi EV charger status
  zappi-start         Start Zappi charging (Fast mode)
  zappi-stop          Stop Zappi charging
  zappi-eco           Set Zappi to Eco mode
  zappi-eco+          Set Zappi to Eco+ mode
  zappi-boost KWH     Boost charge for KWH kilowatt-hours
  server              Start HTTP dashboard server
  collect             Start data collector (runs every minute)
  collect-once        Collect data once and exit (for cron)

Options:
`)
	flag.PrintDefaults()
}

func main() {
	configPath := flag.String("config", "config.toml", "path to config file")
	debug := flag.Bool("debug", false, "enable debug output")
	flag.Usage = usage
	flag.Parse()

	// Load config
	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("Reading config %s: %v", *configPath, err)
	}

	// Parse command
	args := flag.Args()
	cmd := "status"
	if len(args) > 0 {
		cmd = args[0]
	}

	// Handle prices command separately (doesn't need MySkoda)
	if cmd == "prices" {
		npClient := nordpool.NewClient()
		npClient.SetDebug(*debug)
		showPrices(npClient)
		return
	}

	// Handle server command
	if cmd == "server" {
		listenAddr := cfg.Server.Listen
		if listenAddr == "" {
			listenAddr = ":8080"
		}
		dataDir := cfg.Server.DataDir
		if dataDir == "" {
			dataDir = "./data"
		}

		srv := web.NewServer(web.Config{
			ListenAddr:      listenAddr,
			DataDir:         dataDir,
			MySkodaUsername: cfg.MySkoda.Username,
			MySkodaPassword: cfg.MySkoda.Password,
			AlphaESSAppID:   cfg.AlphaESS.AppID,
			AlphaESSSecret:  cfg.AlphaESS.AppSecret,
			AlphaESSSN:      cfg.AlphaESS.SN,
			MyEnergiSerial:  cfg.MyEnergi.HubSerial,
			MyEnergiPass:    cfg.MyEnergi.Password,
		})
		if err := srv.Run(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
		return
	}

	// Handle collect command
	if cmd == "collect" || cmd == "collect-once" {
		dataDir := cfg.Server.DataDir
		if dataDir == "" {
			dataDir = "./data"
		}

		coll := collector.NewCollector(collector.Config{
			DataDir:         dataDir,
			Interval:        time.Minute,
			MySkodaUsername: cfg.MySkoda.Username,
			MySkodaPassword: cfg.MySkoda.Password,
			AlphaESSAppID:   cfg.AlphaESS.AppID,
			AlphaESSSecret:  cfg.AlphaESS.AppSecret,
			AlphaESSSN:      cfg.AlphaESS.SN,
			MyEnergiSerial:  cfg.MyEnergi.HubSerial,
			MyEnergiPass:    cfg.MyEnergi.Password,
		})

		if cmd == "collect-once" {
			if err := coll.RunOnce(); err != nil {
				log.Fatalf("Collector error: %v", err)
			}
		} else {
			if err := coll.Run(); err != nil {
				log.Fatalf("Collector error: %v", err)
			}
		}
		return
	}

	// Handle battery command (AlphaESS)
	if cmd == "battery" {
		if cfg.AlphaESS.AppID == "" || cfg.AlphaESS.AppSecret == "" {
			fmt.Fprintln(os.Stderr, "Error: alphaess.appid and alphaess.appsecret must be set in config.toml")
			os.Exit(1)
		}
		if *debug {
			fmt.Printf("[DEBUG] AlphaESS config: appid=%s, sn=%s\n", cfg.AlphaESS.AppID, cfg.AlphaESS.SN)
		}
		aClient := alphaess.NewClient(cfg.AlphaESS.AppID, cfg.AlphaESS.AppSecret)
		aClient.SetDebug(*debug)
		if cfg.AlphaESS.SN != "" {
			aClient.SetSN(cfg.AlphaESS.SN)
		}
		showBatteryStatus(aClient)
		return
	}

	// Handle zappi commands (myenergi)
	if cmd == "zappi" || cmd == "zappi-start" || cmd == "zappi-stop" || cmd == "zappi-eco" || cmd == "zappi-eco+" || cmd == "zappi-boost" {
		if cfg.MyEnergi.HubSerial == "" || cfg.MyEnergi.Password == "" {
			fmt.Fprintln(os.Stderr, "Error: myenergi.hubserial and myenergi.password must be set in config.toml")
			os.Exit(1)
		}
		if *debug {
			fmt.Printf("[DEBUG] MyEnergi config: hubserial=%s\n", cfg.MyEnergi.HubSerial)
		}
		meClient := myenergi.NewClient(cfg.MyEnergi.HubSerial, cfg.MyEnergi.Password)
		meClient.SetDebug(*debug)

		switch cmd {
		case "zappi":
			showZappiStatus(meClient)
		case "zappi-start":
			controlZappi(meClient, myenergi.ZappiModeFast, "Starting charging (Fast mode)")
		case "zappi-stop":
			controlZappi(meClient, myenergi.ZappiModeStopped, "Stopping charging")
		case "zappi-eco":
			controlZappi(meClient, myenergi.ZappiModeEco, "Setting Eco mode")
		case "zappi-eco+":
			controlZappi(meClient, myenergi.ZappiModeEcoPlus, "Setting Eco+ mode")
		case "zappi-boost":
			var kwh int
			if len(args) >= 2 {
				fmt.Sscanf(args[1], "%d", &kwh)
			}
			if kwh <= 0 {
				fmt.Fprintln(os.Stderr, "Error: zappi-boost requires KWH argument (e.g., zappi-boost 10)")
				os.Exit(1)
			}
			boostZappi(meClient, kwh)
		}
		return
	}

	// All other commands need MySkoda
	if cfg.MySkoda.Username == "" || cfg.MySkoda.Password == "" {
		fmt.Fprintln(os.Stderr, "Error: myskoda.username and myskoda.password must be set in config.toml")
		os.Exit(1)
	}

	// Create client
	client, err := myskoda.NewClient(cfg.MySkoda.Username, cfg.MySkoda.Password)
	if err != nil {
		log.Fatalf("Creating client: %v", err)
	}

	// Login
	fmt.Println("Logging in...")
	if err := client.LoginWithDebug(*debug); err != nil {
		log.Fatalf("Login failed: %v", err)
	}
	fmt.Println("Login successful!")

	// Get vehicles
	vehicles, err := client.GetVehicles()
	if err != nil {
		log.Fatalf("Getting vehicles: %v", err)
	}

	if len(vehicles) == 0 {
		log.Fatal("No vehicles found in your garage")
	}

	// Helper to get VIN (from args or first vehicle)
	getVIN := func(argIndex int) string {
		if len(args) > argIndex && len(args[argIndex]) > 10 {
			return args[argIndex]
		}
		return vehicles[0].VIN
	}

	switch cmd {
	case "status":
		showStatus(client, vehicles)

	case "start":
		vin := getVIN(1)
		fmt.Printf("Starting charging for %s...\n", vin)
		if err := client.StartCharging(vin); err != nil {
			log.Fatalf("Failed to start charging: %v", err)
		}
		fmt.Println("Charging started!")

	case "stop":
		vin := getVIN(1)
		fmt.Printf("Stopping charging for %s...\n", vin)
		if err := client.StopCharging(vin); err != nil {
			log.Fatalf("Failed to stop charging: %v", err)
		}
		fmt.Println("Charging stopped!")

	case "limit":
		vin := getVIN(1)
		var pct int
		if len(args) >= 3 {
			fmt.Sscanf(args[2], "%d", &pct)
		} else if len(args) >= 2 {
			fmt.Sscanf(args[1], "%d", &pct)
			vin = vehicles[0].VIN
		}
		if pct < 50 || pct > 100 {
			log.Fatal("Charge limit must be between 50 and 100")
		}
		fmt.Printf("Setting charge limit to %d%% for %s...\n", pct, vin)
		if err := client.SetChargeLimit(vin, pct); err != nil {
			log.Fatalf("Failed to set charge limit: %v", err)
		}
		fmt.Println("Charge limit set!")

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func showStatus(client *myskoda.Client, vehicles []myskoda.Vehicle) {
	fmt.Printf("\nFound %d vehicle(s):\n", len(vehicles))
	for _, v := range vehicles {
		fmt.Printf("  - %s (VIN: %s, Plate: %s)\n", v.Name, v.VIN, v.LicensePlate)
	}

	for _, v := range vehicles {
		fmt.Printf("\n=== %s ===\n", v.Name)

		// Battery & Charging
		charging, err := client.GetCharging(v.VIN)
		if err != nil {
			fmt.Printf("  [Charging] Error: %v\n", err)
		} else if charging.Status != nil {
			bat := charging.Status.Battery
			fmt.Printf("\n[Battery]\n")
			fmt.Printf("  State of Charge: %d%% (~%.1f kWh)\n", bat.StateOfChargePercent, bat.EstimatedKWh())
			fmt.Printf("  Remaining Range: %d km\n", bat.RemainingRangeMeters/1000)
			fmt.Printf("  Charging State:  %s\n", charging.Status.State)
			if charging.Status.ChargeType != "" {
				fmt.Printf("  Charge Type:     %s\n", charging.Status.ChargeType)
			}
			if charging.Status.State == "CHARGING" {
				fmt.Printf("  Charge Power:    %.1f kW\n", charging.Status.ChargePowerKW)
				fmt.Printf("  Charging Rate:   %.0f km/h\n", charging.Status.ChargingRateKmPerHour)
				fmt.Printf("  Time to Full:    %d min\n", charging.Status.RemainingMinutesToFullyCharged)
			}
			if charging.IsVehicleInSavedLocation {
				fmt.Printf("  Location:        Home\n")
			}
		}

		// Vehicle Status (doors, windows, mileage) - may not be available on all models
		status, err := client.GetStatus(v.VIN)
		if err == nil && status != nil {
			fmt.Printf("\n[Vehicle]\n")
			if status.Mileage > 0 {
				fmt.Printf("  Odometer:        %d km\n", status.Mileage)
			}
			if status.Doors.OverallStatus != "" {
				if status.Doors.Locked {
					fmt.Printf("  Doors:           Locked\n")
				} else {
					fmt.Printf("  Doors:           Unlocked\n")
				}
			}
			if status.Windows.OverallStatus != "" {
				fmt.Printf("  Windows:         %s\n", status.Windows.OverallStatus)
			}
			if status.Lights.OverallStatus != "" {
				fmt.Printf("  Lights:          %s\n", status.Lights.OverallStatus)
			}
		}

		// Position
		pos, err := client.GetPosition(v.VIN)
		if err != nil {
			fmt.Printf("\n[Position] Error: %v\n", err)
		} else if pos != nil && (pos.Latitude != 0 || pos.Longitude != 0) {
			fmt.Printf("\n[Position]\n")
			fmt.Printf("  Coordinates:     %.6f, %.6f\n", pos.Latitude, pos.Longitude)
			if pos.Address != "" {
				fmt.Printf("  Address:         %s\n", pos.Address)
			}
		}

		// Air Conditioning
		ac, err := client.GetAirConditioning(v.VIN)
		if err != nil {
			fmt.Printf("\n[Climate] Error: %v\n", err)
		} else {
			fmt.Printf("\n[Climate]\n")
			fmt.Printf("  AC State:        %s\n", ac.State)
			if ac.TargetTemperatureCelsius > 0 {
				fmt.Printf("  Target Temp:     %.1f°C\n", ac.TargetTemperatureCelsius)
			}
			if ac.ChargerConnected == "CONNECTED" {
				fmt.Printf("  Charger:         Connected\n")
			}
		}

		// Health / Warning Lights
		health, err := client.GetHealth(v.VIN)
		if err != nil {
			fmt.Printf("\n[Health] Error: %v\n", err)
		} else {
			fmt.Printf("\n[Health]\n")
			activeWarnings := 0
			for _, w := range health.WarningLights {
				if w.Name != "" && w.State != "" {
					fmt.Printf("  Warning:         %s (%s)\n", w.Name, w.State)
					activeWarnings++
				}
			}
			if activeWarnings == 0 {
				fmt.Printf("  Warning Lights:  None active\n")
			}
		}
	}
}

func showPrices(client *nordpool.Client) {
	fmt.Println("Fetching energy prices from NordPool...")

	prices, err := client.GetPrices()
	if err != nil {
		log.Fatalf("Failed to fetch prices: %v", err)
	}

	// Current and next hour prices
	current := client.GetCurrentPrice(prices)
	next := client.GetNextPrice(prices)
	lowest := client.GetLowestPrice(prices)
	highest := client.GetHighestPrice(prices)

	fmt.Printf("\n=== Energy Prices ===\n")

	if current != nil {
		fmt.Printf("\n[Current Hour] %s\n", current.Period.Format("15:04"))
		fmt.Printf("  Price:        %.2f EUR/MWh (%.4f EUR/kWh)\n", current.PriceEUR, current.PricePerKWh())
	}

	if next != nil {
		fmt.Printf("\n[Next Hour] %s\n", next.Period.Format("15:04"))
		fmt.Printf("  Price:        %.2f EUR/MWh (%.4f EUR/kWh)\n", next.PriceEUR, next.PricePerKWh())
	}

	if lowest != nil {
		fmt.Printf("\n[Lowest Price] %s\n", lowest.Period.Format("Mon 15:04"))
		fmt.Printf("  Price:        %.2f EUR/MWh (%.4f EUR/kWh)\n", lowest.PriceEUR, lowest.PricePerKWh())
	}

	if highest != nil {
		fmt.Printf("\n[Highest Price] %s\n", highest.Period.Format("Mon 15:04"))
		fmt.Printf("  Price:        %.2f EUR/MWh (%.4f EUR/kWh)\n", highest.PriceEUR, highest.PricePerKWh())
	}

	// Today's prices
	if len(prices.Today) > 0 {
		fmt.Printf("\n[Today - %s]\n", prices.Today[0].Period.Format("Mon 02 Jan"))
		fmt.Printf("  %-5s  %12s  %12s\n", "Hour", "EUR/MWh", "EUR/kWh")
		fmt.Printf("  %-5s  %12s  %12s\n", "-----", "------------", "------------")
		for _, p := range prices.Today {
			fmt.Printf("  %-5s  %12.2f  %12.4f\n",
				p.Period.Format("15:04"),
				p.PriceEUR,
				p.PricePerKWh())
		}
	}

	// Tomorrow's prices (if available)
	if len(prices.Tomorrow) > 0 {
		fmt.Printf("\n[Tomorrow - %s]\n", prices.Tomorrow[0].Period.Format("Mon 02 Jan"))
		fmt.Printf("  %-5s  %12s  %12s\n", "Hour", "EUR/MWh", "EUR/kWh")
		fmt.Printf("  %-5s  %12s  %12s\n", "-----", "------------", "------------")
		for _, p := range prices.Tomorrow {
			fmt.Printf("  %-5s  %12.2f  %12.4f\n",
				p.Period.Format("15:04"),
				p.PriceEUR,
				p.PricePerKWh())
		}
	} else {
		fmt.Printf("\n[Tomorrow]\n")
		fmt.Printf("  Prices not yet available (typically published around 13:00 CET)\n")
	}
}

func showBatteryStatus(client *alphaess.Client) {
	fmt.Println("Fetching AlphaESS battery status...")

	sn, err := client.GetSN()
	if err != nil {
		log.Fatalf("Failed to get system SN: %v", err)
	}
	fmt.Printf("Using system SN: %s\n", sn)

	power, err := client.GetLastPowerData()
	if err != nil {
		log.Fatalf("Failed to get power data: %v", err)
	}

	fmt.Printf("\n=== AlphaESS Battery Status ===\n")
	fmt.Printf("\n[Power]\n")
	fmt.Printf("  SOC:            %.1f %%\n", power.SOC)
	fmt.Printf("  Battery Power:  %.1f W", power.BatteryPower)
	if power.BatteryPower > 0 {
		fmt.Printf(" (charging)")
	} else if power.BatteryPower < 0 {
		fmt.Printf(" (discharging)")
	}
	fmt.Println()
	fmt.Printf("  PV Power:       %.1f W\n", power.PVPower)
	fmt.Printf("  Load Power:     %.1f W\n", power.LoadPower)
	fmt.Printf("  Grid Power:     %.1f W", power.GridPower)
	if power.GridPower > 0 {
		fmt.Printf(" (importing)")
	} else if power.GridPower < 0 {
		fmt.Printf(" (exporting)")
	}
	fmt.Println()

	// Get charge config
	chargeConfig, err := client.GetChargeConfig()
	if err != nil {
		fmt.Printf("\n[Charge Config] Error: %v\n", err)
	} else {
		fmt.Printf("\n[Charge Config]\n")
		if chargeConfig.GridCharge == 1 {
			fmt.Printf("  Grid Charge:    Enabled\n")
		} else {
			fmt.Printf("  Grid Charge:    Disabled\n")
		}
		if chargeConfig.TimeChaf1 != "00:00" || chargeConfig.TimeChae1 != "00:00" {
			fmt.Printf("  Period 1:       %s - %s\n", chargeConfig.TimeChaf1, chargeConfig.TimeChae1)
		}
		if chargeConfig.TimeChaf2 != "00:00" || chargeConfig.TimeChae2 != "00:00" {
			fmt.Printf("  Period 2:       %s - %s\n", chargeConfig.TimeChaf2, chargeConfig.TimeChae2)
		}
		fmt.Printf("  High Cap:       %.0f %%\n", chargeConfig.BatHighCap)
	}

	// Get discharge config
	dischargeConfig, err := client.GetDischargeConfig()
	if err != nil {
		fmt.Printf("\n[Discharge Config] Error: %v\n", err)
	} else {
		fmt.Printf("\n[Discharge Config]\n")
		if dischargeConfig.CtrDis == 1 {
			fmt.Printf("  Discharge:      Enabled\n")
		} else {
			fmt.Printf("  Discharge:      Disabled\n")
		}
		if dischargeConfig.TimeDisf1 != "00:00" || dischargeConfig.TimeDise1 != "00:00" {
			fmt.Printf("  Period 1:       %s - %s\n", dischargeConfig.TimeDisf1, dischargeConfig.TimeDise1)
		}
		if dischargeConfig.TimeDisf2 != "00:00" || dischargeConfig.TimeDise2 != "00:00" {
			fmt.Printf("  Period 2:       %s - %s\n", dischargeConfig.TimeDisf2, dischargeConfig.TimeDise2)
		}
		fmt.Printf("  Min SOC:        %.0f %%\n", dischargeConfig.BatUseCap)
	}
}

func showZappiStatus(client *myenergi.Client) {
	fmt.Println("Fetching Zappi status...")

	zappis, err := client.GetZappiStatus()
	if err != nil {
		log.Fatalf("Failed to get Zappi status: %v", err)
	}

	if len(zappis) == 0 {
		fmt.Println("No Zappi devices found")
		return
	}

	fmt.Printf("\nFound %d Zappi device(s):\n", len(zappis))

	for _, z := range zappis {
		fmt.Printf("\n=== Zappi %d ===\n", z.Serial)

		fmt.Printf("\n[Status]\n")
		fmt.Printf("  Mode:           %s\n", z.Mode)
		fmt.Printf("  Status:         %s\n", z.Status)
		fmt.Printf("  Plug:           %s\n", z.PlugStatus.String())

		fmt.Printf("\n[Power]\n")
		fmt.Printf("  Charger Power:  %.0f W\n", z.ChargerPower)
		fmt.Printf("  Grid Power:     %.0f W\n", z.GridPower)
		fmt.Printf("  Generated:      %.0f W\n", z.GeneratedPower)
		if z.Diverted > 0 {
			fmt.Printf("  Diverted:       %.0f W\n", z.Diverted)
		}

		fmt.Printf("\n[Session]\n")
		fmt.Printf("  Charge Added:   %.2f kWh\n", z.ChargeAdded)
		if z.BoostKWh > 0 {
			fmt.Printf("  Boost Target:   %.1f kWh\n", z.BoostKWh)
		}
		if z.SmartBoostHour > 0 || z.SmartBoostMin > 0 {
			fmt.Printf("  Smart Boost:    Complete by %02d:%02d\n", z.SmartBoostHour, z.SmartBoostMin)
		}

		fmt.Printf("\n[Settings]\n")
		fmt.Printf("  Min Green:      %d %%\n", z.MinGreenLevel)
		if z.Voltage > 0 {
			fmt.Printf("  Voltage:        %.1f V\n", z.Voltage/10)
		}
		if z.Frequency > 0 {
			fmt.Printf("  Frequency:      %.2f Hz\n", z.Frequency/100)
		}
	}
}

func controlZappi(client *myenergi.Client, mode myenergi.ZappiMode, message string) {
	// Get Zappi serial first
	zappis, err := client.GetZappiStatus()
	if err != nil {
		log.Fatalf("Failed to get Zappi status: %v", err)
	}

	if len(zappis) == 0 {
		fmt.Println("No Zappi devices found")
		return
	}

	serial := fmt.Sprintf("%d", zappis[0].Serial)
	fmt.Printf("%s for Zappi %s...\n", message, serial)

	if err := client.SetZappiMode(serial, mode); err != nil {
		log.Fatalf("Failed to set Zappi mode: %v", err)
	}

	fmt.Println("Done!")
}

func boostZappi(client *myenergi.Client, kwh int) {
	// Get Zappi serial first
	zappis, err := client.GetZappiStatus()
	if err != nil {
		log.Fatalf("Failed to get Zappi status: %v", err)
	}

	if len(zappis) == 0 {
		fmt.Println("No Zappi devices found")
		return
	}

	serial := fmt.Sprintf("%d", zappis[0].Serial)
	fmt.Printf("Starting boost charge of %d kWh for Zappi %s...\n", kwh, serial)

	if err := client.BoostZappi(serial, kwh); err != nil {
		log.Fatalf("Failed to start boost: %v", err)
	}

	fmt.Println("Boost started!")
}
