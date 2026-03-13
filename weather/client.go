package weather

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client for Open-Meteo API
type Client struct {
	Latitude  float64
	Longitude float64
	httpClient *http.Client
}

// Current weather data
type Current struct {
	Temperature    float64 `json:"temperature_2m"`
	Humidity       int     `json:"relative_humidity_2m"`
	ApparentTemp   float64 `json:"apparent_temperature"`
	WeatherCode    int     `json:"weather_code"`
	WindSpeed      float64 `json:"wind_speed_10m"`
	WindDirection  int     `json:"wind_direction_10m"`
	CloudCover     int     `json:"cloud_cover"`
	IsDay          int     `json:"is_day"`
}

// Weather contains current conditions and daily forecast
type Weather struct {
	Current     Current
	Description string
	Icon        string
	LastUpdate  time.Time
}

// openMeteoResponse is the API response structure
type openMeteoResponse struct {
	Current struct {
		Temperature       float64 `json:"temperature_2m"`
		RelativeHumidity  int     `json:"relative_humidity_2m"`
		ApparentTemp      float64 `json:"apparent_temperature"`
		WeatherCode       int     `json:"weather_code"`
		WindSpeed         float64 `json:"wind_speed_10m"`
		WindDirection     int     `json:"wind_direction_10m"`
		CloudCover        int     `json:"cloud_cover"`
		IsDay             int     `json:"is_day"`
	} `json:"current"`
}

// NewClient creates a new Open-Meteo client
func NewClient(lat, lon float64) *Client {
	return &Client{
		Latitude:  lat,
		Longitude: lon,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetCurrent fetches current weather conditions
func (c *Client) GetCurrent() (*Weather, error) {
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&current=temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,wind_speed_10m,wind_direction_10m,cloud_cover,is_day",
		c.Latitude, c.Longitude,
	)

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch weather: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weather API returned status %d", resp.StatusCode)
	}

	var data openMeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode weather response: %w", err)
	}

	weather := &Weather{
		Current: Current{
			Temperature:   data.Current.Temperature,
			Humidity:      data.Current.RelativeHumidity,
			ApparentTemp:  data.Current.ApparentTemp,
			WeatherCode:   data.Current.WeatherCode,
			WindSpeed:     data.Current.WindSpeed,
			WindDirection: data.Current.WindDirection,
			CloudCover:    data.Current.CloudCover,
			IsDay:         data.Current.IsDay,
		},
		Description: weatherCodeToDescription(data.Current.WeatherCode),
		Icon:        weatherCodeToIcon(data.Current.WeatherCode, data.Current.IsDay == 1),
		LastUpdate:  time.Now(),
	}

	return weather, nil
}

// weatherCodeToDescription converts WMO weather codes to descriptions
func weatherCodeToDescription(code int) string {
	switch code {
	case 0:
		return "Clear sky"
	case 1:
		return "Mainly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45, 48:
		return "Foggy"
	case 51, 53, 55:
		return "Drizzle"
	case 56, 57:
		return "Freezing drizzle"
	case 61, 63, 65:
		return "Rain"
	case 66, 67:
		return "Freezing rain"
	case 71, 73, 75:
		return "Snow"
	case 77:
		return "Snow grains"
	case 80, 81, 82:
		return "Rain showers"
	case 85, 86:
		return "Snow showers"
	case 95:
		return "Thunderstorm"
	case 96, 99:
		return "Thunderstorm with hail"
	default:
		return "Unknown"
	}
}

// weatherCodeToIcon converts WMO weather codes to Lucide icon names
func weatherCodeToIcon(code int, isDay bool) string {
	switch code {
	case 0:
		if isDay {
			return "sun"
		}
		return "moon"
	case 1, 2:
		if isDay {
			return "cloud-sun"
		}
		return "cloud-moon"
	case 3:
		return "cloud"
	case 45, 48:
		return "cloud-fog"
	case 51, 53, 55, 56, 57, 61, 63, 65, 66, 67, 80, 81, 82:
		return "cloud-rain"
	case 71, 73, 75, 77, 85, 86:
		return "cloud-snow"
	case 95, 96, 99:
		return "cloud-lightning"
	default:
		return "cloud"
	}
}

// WindDirectionToString converts degrees to cardinal direction
func WindDirectionToString(degrees int) string {
	directions := []string{"N", "NE", "E", "SE", "S", "SW", "W", "NW"}
	index := ((degrees + 22) % 360) / 45
	return directions[index]
}
