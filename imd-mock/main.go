// IMD mock: stands in for the real IMD weather API so the ONIX JSONata transform
// (engineering-tracker#66/#67) can be built without real IMD access. Every value in
// the response is randomized per hit, within bounds that stay physically sane.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"time"
)

type station struct {
	Code string
	Name string
	Lat  float64
	Lon  float64
}

var stations = []station{
	{"43382", "NANCOWRY", 7.98333, 93.55},
	{"42809", "LUCKNOW", 26.44, 80.33},
	{"42667", "NEW DELHI", 28.61, 77.21},
	{"43295", "CHENNAI", 13.08, 80.27},
}

var clearForecasts = []string{"Clear sky", "Mainly clear sky", "Partly cloudy", "Generally cloudy"}
var lightRainForecasts = []string{"Cloudy with light rain", "Generally cloudy sky with light rain"}
var heavyRainForecasts = []string{"Heavy rain likely", "Heavy to very heavy rain likely"}

func pick[T any](options []T) T {
	return options[rand.IntN(len(options))]
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

type dayWeather struct {
	MinTemp  float64
	MaxTemp  float64
	Rainfall float64
	RHMin    int
	RHMax    int
	Forecast string
}

// randomDayWeather keeps values in ranges that hold across most of India:
// min temp 18-28C, max temp 4-14C above that (capped 42C), rainfall 0-40mm,
// humidity 40-100%, and picks a forecast phrase that matches the rolled rainfall.
func randomDayWeather() dayWeather {
	tmin := 18 + rand.Float64()*10
	tmax := tmin + 4 + rand.Float64()*10
	if tmax > 42 {
		tmax = 42
	}
	rainfall := round1(rand.Float64() * 40)
	rhmin := 40 + rand.IntN(36)
	rhmax := rhmin + 5 + rand.IntN(21)
	if rhmax > 100 {
		rhmax = 100
	}

	var forecast string
	switch {
	case rainfall > 20:
		forecast = pick(heavyRainForecasts)
	case rainfall > 5:
		forecast = pick(lightRainForecasts)
	default:
		forecast = pick(clearForecasts)
	}

	return dayWeather{
		MinTemp: round1(tmin), MaxTemp: round1(tmax), Rainfall: rainfall,
		RHMin: rhmin, RHMax: rhmax, Forecast: forecast,
	}
}

func randomStationResponse() map[string]any {
	st := pick(stations)

	resp := map[string]any{
		"Date":         time.Now().Format("2006-01-02"),
		"Station_Code": st.Code,
		"Station_Name": st.Name,
		"Latitude":     st.Lat,
		"Longitude":    st.Lon,
	}

	today := randomDayWeather()
	resp["Today_Max_temp"] = today.MaxTemp
	resp["Today_Min_temp"] = today.MinTemp
	resp["Past_24_hrs_Rainfall"] = today.Rainfall
	resp["Relative_Humidity_at_0830"] = today.RHMin
	resp["Relative_Humidity_at_1730"] = today.RHMax
	resp["Todays_Forecast_Max_Temp"] = today.MaxTemp
	resp["Todays_Forecast_Min_temp"] = today.MinTemp
	resp["Todays_Forecast"] = today.Forecast

	for day := 2; day <= 7; day++ {
		d := randomDayWeather()
		resp[fmt.Sprintf("Day_%d_Max_Temp", day)] = d.MaxTemp
		resp[fmt.Sprintf("Day_%d_Min_temp", day)] = d.MinTemp
		resp[fmt.Sprintf("Day_%d_Forecast", day)] = d.Forecast
	}

	return resp
}

func main() {
	http.HandleFunc("/IMD/daily-weather-forecast", handleWeather)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}
	log.Printf("IMD mock listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleWeather(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := []map[string]any{randomStationResponse()}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(resp)
}
