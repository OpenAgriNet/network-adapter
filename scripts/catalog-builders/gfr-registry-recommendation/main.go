// Command gfr-registry-recommendation builds GFR's catalog: one fertilizer advice resource per
// state with GFR crops, and one India-wide soil test resource. Settings come from the environment.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/scripts/catalog-builders/areas"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := loadDotEnv(dotEnvPath("gfr-registry-recommendation")); err != nil {
		return err
	}
	s, err := loadSettings()
	if err != nil {
		return err
	}
	rows, shapes, err := areas.Read(s.AreasDir)
	if err != nil {
		return err
	}
	states, err := fetchStates(&http.Client{Timeout: 120 * time.Second}, s.SHCURL)
	if err != nil {
		return err
	}
	body, err := json.Marshal(publishBody(resources(states, rows, shapes, s), s))
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.Output, body, 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s: %.2f MB", s.Output, float64(len(body))/1e6)
	return nil
}

// settings is everything the builder reads from the environment.
type settings struct {
	SHCURL, AreasDir      string
	ProviderID, CatalogID string
	Output                string
}

var requiredSettings = []string{
	"SOIL_HEALTH_BASE_URL",
}

func setting(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// loadSettings names every missing variable at once, so one run shows them all.
func loadSettings() (settings, error) {
	var missing []string
	for _, name := range requiredSettings {
		if setting(name, "") == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return settings{}, fmt.Errorf("missing settings: %s", strings.Join(missing, ", "))
	}
	return settings{
		SHCURL: setting("SOIL_HEALTH_BASE_URL", ""), AreasDir: setting("GFR_AREAS_DIR", "areas"),
		ProviderID: setting("GFR_PROVIDER_ID", "gfr"), CatalogID: setting("GFR_CATALOG_ID", "cat-gfr"),
		Output: setting("GFR_OUTPUT", "gfr-registry-recommendation/gfr-catalog-publish.json"),
	}, nil
}

// crop is one SHC crop registry entry; its id is what GFR's recommendation call takes.
type crop struct{ Code, Name string }

// shcState is one state in SHC's registry: SHC's own id for it and its GFR crops.
type shcState struct {
	SHCID string
	Crops []crop
}

const registryQuery = "query GetCropRegistries($state: String) { getCropRegistries(state: $state) " +
	"{ name variety irrigationType season splitdose state GFRavailable id combinedName __typename } }"

// fetchStates asks SHC for every state's crop registry and keeps the crops GFR can advise on,
// keyed by the state's LGD code. No login: the registry is public.
func fetchStates(client *http.Client, shcURL string) (map[string]shcState, error) {
	rows, err := registryRows(client, shcURL)
	if err != nil {
		return nil, err
	}
	states := map[string]shcState{}
	for _, r := range rows {
		state := states[r.State.Code]
		state.SHCID = r.State.ID
		if strings.EqualFold(strings.TrimSpace(r.GFRAvailable), "yes") {
			state.Crops = append(state.Crops, crop{r.ID, cropName(r.CombinedName, r.Name)})
		}
		states[r.State.Code] = state
	}
	log.Printf("SHC registry: %d crops across %d states", len(rows), len(states))
	return states, nil
}

type registryRow struct {
	ID, Name, CombinedName, GFRAvailable string
	State                                struct{ ID, Code, Name string }
}

func registryRows(client *http.Client, shcURL string) ([]registryRow, error) {
	query, _ := json.Marshal(map[string]any{"query": registryQuery, "variables": map[string]any{}})
	resp, err := client.Post(shcURL, "application/json", bytes.NewReader(query))
	if err != nil {
		return nil, fmt.Errorf("SHC crop registry: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SHC crop registry answered HTTP %d", resp.StatusCode)
	}
	var answer struct {
		Data struct {
			GetCropRegistries []struct {
				ID, Name, CombinedName, GFRavailable string
				State                                struct {
					ID   string `json:"_id"`
					Code string
					Name string
				}
			}
		}
		Errors []struct{ Message string }
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("SHC crop registry: unreadable answer: %w", err)
	}
	if len(answer.Errors) > 0 {
		return nil, fmt.Errorf("SHC crop registry: %s", answer.Errors[0].Message)
	}
	if len(answer.Data.GetCropRegistries) == 0 {
		return nil, fmt.Errorf("SHC crop registry returned no crops")
	}
	rows := make([]registryRow, 0, len(answer.Data.GetCropRegistries))
	for _, r := range answer.Data.GetCropRegistries {
		row := registryRow{ID: r.ID, Name: r.Name, CombinedName: r.CombinedName, GFRAvailable: r.GFRavailable}
		row.State.ID, row.State.Code, row.State.Name = r.State.ID, r.State.Code, r.State.Name
		rows = append(rows, row)
	}
	return rows, nil
}

func cropName(combined, name string) string {
	if strings.TrimSpace(combined) != "" {
		return strings.TrimSpace(combined)
	}
	return strings.TrimSpace(name)
}

type object = map[string]any

// resources makes one advice resource per SHC state that has GFR crops and a boundary in
// the areas folder, logging every state it skips, then the India-wide soil test.
func resources(states map[string]shcState, rows []areas.Area, shapes map[string]json.RawMessage, s settings) []object {
	names := map[string]string{}
	for _, r := range rows {
		if r.Level == "State" {
			names[r.Code] = r.Name
		}
	}
	codes := make([]string, 0, len(states))
	for code := range states {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return lgdNumber(codes[i]) < lgdNumber(codes[j]) })
	var out []object
	var used []json.RawMessage
	for _, code := range codes {
		boundary := shapes[areas.Key(code, "State")]
		switch {
		case len(states[code].Crops) == 0:
			log.Printf("skipped state %s: no crop with a GFR recommendation", code)
		case names[code] == "" || boundary == nil:
			log.Printf("skipped state %s: not in the areas folder", code)
		default:
			out = append(out, adviceResource(code, names[code], states[code], boundary, s))
			used = append(used, boundary)
			log.Printf("%s (LGD %s): %d GFR crops", names[code], code, len(states[code].Crops))
		}
	}
	return append(out, soilResource(boundingBox(used), s))
}

func adviceResource(code, name string, state shcState, boundary json.RawMessage, s settings) object {
	subjects := make([]object, 0, len(state.Crops))
	for _, c := range state.Crops {
		subjects = append(subjects, object{"descriptor": object{"code": c.Code, "name": c.Name}})
	}
	return object{
		"id": "res-gfr-advice:" + code,
		"descriptor": object{"code": "gfr-advice", "name": "GFR fertilizer recommendation - " + name,
			"shortDesc": "Fertilizer doses from soil test values, for this state's GFR crops. Boundary: Survey of India"},
		"resourceAttributes": object{
			"@context": "{{gfr-advisory-context}}", "@type": "openagrinet:AgricultureAdvisory",
			"informationMode": "OnDemand", "subjectCategories": []string{"Crop"}, "languages": []string{"en", "hi"},
			"topics": []string{"Fertilizer recommendation"}, "agricultureSubjects": subjects,
			"coverageAreas": []any{
				object{"codeScheme": "LGD", "areaLevel": "State", "areaCode": code, "areaName": name},
				object{"codeScheme": "SHC", "areaLevel": "State", "areaCode": state.SHCID},
				boundary,
			},
			"actions": []object{{"action": "select", "operationRef": "{{gfr-advice-operation-ref}}"}},
		},
	}
}

// soilResource is found from anywhere in India, so its coverage is the rectangle around every state.
func soilResource(india object, s settings) object {
	return object{
		"id":         "res-gfr-soil-report",
		"descriptor": object{"code": "gfr-soil-report", "name": "GFR soil test", "shortDesc": "The farmer's soil test values for a cycle"},
		"resourceAttributes": object{
			"@context": "{{gfr-resource-context}}", "@type": "openagrinet:AgricultureResource",
			"informationMode": "OnDemand", "subjectCategories": []string{"Practice"}, "languages": []string{"en", "hi"},
			"coverageAreas": []any{object{"codeScheme": "ISO-3166-1", "areaLevel": "Country", "areaCode": "IN", "areaName": "India"}, india},
			"actions":       []object{{"action": "select", "operationRef": "{{gfr-soil-operation-ref}}"}},
		},
	}
}

func boundingBox(boundaries []json.RawMessage) object {
	west, south, east, north := 180.0, 90.0, -180.0, -90.0
	for _, b := range boundaries {
		var g struct{ Coordinates any }
		json.Unmarshal(b, &g)
		walkPoints(g.Coordinates, func(lon, lat float64) {
			west, east = min(west, lon), max(east, lon)
			south, north = min(south, lat), max(north, lat)
		})
	}
	return object{"type": "Polygon", "coordinates": [][][]float64{{{west, south}, {east, south}, {east, north}, {west, north}, {west, south}}}}
}

func walkPoints(node any, visit func(lon, lat float64)) {
	list, ok := node.([]any)
	if !ok || len(list) == 0 {
		return
	}
	if lon, isNumber := list[0].(float64); isNumber && len(list) >= 2 {
		visit(lon, list[1].(float64))
		return
	}
	for _, child := range list {
		walkPoints(child, visit)
	}
}

// publishContext and the catalog's bppId, bppUri and visibleTo are Postman variables: the
// API collection's publish request fills them.
func publishContext() object {
	return object{"domain": "agriculture", "action": "catalog/publish", "version": "2.0.0",
		"transactionId": "{{$guid}}", "messageId": "{{$guid}}", "timestamp": "{{$isoTimestamp}}",
		"networkId": "{{networkId}}", "senderId": "{{senderId}}", "receiverId": "{{receiverId}}"}
}

var visibleTo = []string{"{{networkId}}", "{{otherNetworkId}}"}

func publishBody(resources []object, s settings) object {
	descriptor := object{"code": "GFR", "name": "GFR fertilizer recommendation"}
	return object{
		"context": publishContext(),
		"message": object{
			"catalogs": []object{{
				"id": s.CatalogID, "bppId": "{{bppId}}", "bppUri": "{{bppUri}}", "descriptor": descriptor,
				"provider": object{"id": s.ProviderID, "descriptor": descriptor},
				"isActive": true, "resources": resources,
			}},
			"publishDirectives": []object{{"catalogId": s.CatalogID, "catalogType": "REGULAR", "visibleTo": visibleTo}},
		},
	}
}

func lgdNumber(code string) int {
	n, _ := strconv.Atoi(code)
	return n
}

// dotEnvPath is the builder's own .env: "gfr-registry-recommendation/.env" when run from catalog-builders, as the
// README says, or ".env" when run from inside the builder's folder.
func dotEnvPath(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		return filepath.Join(dir, ".env")
	}
	return ".env"
}

// loadDotEnv sets each NAME=value line the environment does not already set, so a real
// environment variable wins. A missing file is not an error: every setting can come from the environment.
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" || strings.HasPrefix(name, "#") || strings.ContainsAny(name, " \t") {
			continue
		}
		if _, set := os.LookupEnv(name); !set {
			os.Setenv(name, strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
	log.Printf("read settings from %s", path)
	return nil
}
