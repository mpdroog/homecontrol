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

func main() {
	configPath := flag.String("config", "config.toml", "path to config file")
	debug := flag.Bool("debug", false, "enable debug output")
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

	fmt.Printf("\nFound %d vehicle(s):\n", len(vehicles))
	for _, v := range vehicles {
		fmt.Printf("  - %s (VIN: %s, Plate: %s)\n", v.Name, v.VIN, v.LicensePlate)
	}

	// Get battery/charging status for each vehicle
	for _, v := range vehicles {
		fmt.Printf("\n--- %s ---\n", v.Name)

		charging, err := client.GetCharging(v.VIN)
		if err != nil {
			fmt.Printf("  Error getting charging status: %v\n", err)
			continue
		}

		if charging.Status != nil {
			fmt.Printf("  State of Charge: %d%%\n", charging.Status.Battery.StateOfChargePercent)
			fmt.Printf("  Remaining Range: %d km\n", charging.Status.Battery.RemainingRangeMeters/1000)
			fmt.Printf("  Charging State:  %s\n", charging.Status.State)

			if charging.Status.State == "CHARGING" {
				fmt.Printf("  Charge Power:    %.1f kW\n", charging.Status.ChargePowerKW)
				fmt.Printf("  Charging Rate:   %.0f km/h\n", charging.Status.ChargingRateKmPerHour)
				fmt.Printf("  Time to Full:    %d minutes\n", charging.Status.RemainingMinutesToFullyCharged)
			}
		} else {
			fmt.Println("  No charging status available")
		}
	}
}
