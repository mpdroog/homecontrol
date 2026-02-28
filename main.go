package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/mpdroog/homecontrol/myskoda"
)

type Config struct {
	MySkoda MySkodaConfig `toml:"myskoda"`
}

type MySkodaConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: homecontrol [options] [command]

Commands:
  status              Show battery/charging status (default)
  start [VIN]         Start charging (uses first vehicle if VIN not specified)
  stop [VIN]          Stop charging (uses first vehicle if VIN not specified)
  limit [VIN] PCT     Set charge limit to PCT percent

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

	// Parse command
	args := flag.Args()
	cmd := "status"
	if len(args) > 0 {
		cmd = args[0]
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
