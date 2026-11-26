// Command sathi builds SATHI's seed stock catalog: one resource per district of the
// state, each listing every active SATHI crop. Settings come from the environment.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
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
	if err := loadDotEnv(dotEnvPath("sathi-seed")); err != nil {
		return err
	}
	s, err := loadSettings()
	if err != nil {
		return err
	}
	districts, err := readDistricts(s.AreasDir, s.StateCode)
	if err != nil {
		return err
	}
	log.Printf("read %d districts of state %s (%s) from %s", len(districts), s.StateCode, districts[0].StateName, s.AreasDir)
	client := &http.Client{Timeout: 60 * time.Second}
	crops, err := fetchCrops(client, s.MasterBaseURL)
	if err != nil {
		return err
	}
	body, err := encode(publishBody(districts, crops, s))
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.Output, body, 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s: %d districts x %d crops, %.2f MB", s.Output, len(districts), len(crops), float64(len(body))/1e6)
	return nil
}

// settings is everything the builder reads from the environment.
type settings struct {
	AreasDir, StateCode, MasterBaseURL string
	ProviderID, CatalogID              string
	Output                             string
}

var requiredSettings = []string{
	"SATHI_MASTER_BASE_URL",
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
		AreasDir:      setting("SATHI_AREAS_DIR", "areas"),
		StateCode:     setting("SATHI_STATE_LGD_CODE", "27"),
		MasterBaseURL: strings.TrimRight(setting("SATHI_MASTER_BASE_URL", ""), "/"),
		ProviderID:    setting("SATHI_PROVIDER_ID", "sathi"),
		CatalogID:     setting("SATHI_CATALOG_ID", "cat-sathi"),
		Output:        setting("SATHI_OUTPUT", "sathi-seed/sathi-catalog-publish.json"),
	}, nil
}

// district is one district of the state: its LGD codes, names and boundary as GeoJSON.
type district struct {
	StateCode, StateName, Code, Name string
	Geometry                         json.RawMessage
}

// readDistricts takes the state's districts and their boundaries from the areas folder,
// joined on the LGD code, never on names.
func readDistricts(dir, stateCode string) ([]district, error) {
	rows, shapes, err := areas.Read(dir)
	if err != nil {
		return nil, err
	}
	stateName, districts, err := stateDistricts(rows, stateCode)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range districts {
		districts[i].StateName = stateName
		districts[i].Geometry = shapes[areas.Key(districts[i].Code, "District")]
		if districts[i].Geometry == nil {
			return nil, fmt.Errorf("district %s (%s) has no boundary in areas.geojsonl", districts[i].Code, districts[i].Name)
		}
		if err := checkDistrict(districts[i], seen); err != nil {
			return nil, err
		}
	}
	sort.Slice(districts, func(i, j int) bool { return lgdNumber(districts[i].Code) < lgdNumber(districts[j].Code) })
	return districts, nil
}

// stateDistricts finds the state's name and the districts whose parent it is.
func stateDistricts(rows []areas.Area, stateCode string) (string, []district, error) {
	stateName := ""
	var districts []district
	for _, r := range rows {
		if r.Level == "State" && r.Code == stateCode {
			stateName = r.Name
		}
		if r.Level == "District" && r.ParentLevel == "State" && r.ParentCode == stateCode {
			districts = append(districts, district{StateCode: stateCode, Code: r.Code, Name: r.Name})
		}
	}
	if stateName == "" {
		return "", nil, fmt.Errorf("state %s is not in areas.csv", stateCode)
	}
	if len(districts) == 0 {
		return "", nil, fmt.Errorf("state %s (%s) has no districts in areas.csv", stateCode, stateName)
	}
	return stateName, districts, nil
}

func lgdNumber(code string) int {
	n, _ := strconv.Atoi(code)
	return n
}

// checkDistrict refuses a row the catalog cannot use, naming the district.
func checkDistrict(d district, seen map[string]bool) error {
	key := d.StateCode + ":" + d.Code
	if _, err := strconv.Atoi(d.Code); err != nil || d.Name == "" {
		return fmt.Errorf("district %q: needs a numeric LGD code and a name", d.Code)
	}
	if seen[key] {
		return fmt.Errorf("district %s is listed twice", key)
	}
	seen[key] = true
	var shape struct{ Type string }
	if err := json.Unmarshal(d.Geometry, &shape); err != nil || (shape.Type != "Polygon" && shape.Type != "MultiPolygon") {
		return fmt.Errorf("district %s: geometry must be a GeoJSON Polygon or MultiPolygon", d.Code)
	}
	return nil
}

// crop is one SATHI crop, by SATHI's own code: the code the stock call takes.
type crop struct{ Code, Name string }

// masterAnswer is SATHI's master API reply; the data sits under EncryptedResponse, unencrypted.
type masterAnswer struct {
	EncryptedResponse struct {
		StatusCode int             `json:"status_code"`
		Message    string          `json:"message"`
		Data       json.RawMessage `json:"data"`
	}
}

func fetchCrops(client *http.Client, baseURL string) ([]crop, error) {
	var groups []struct {
		GroupCode string `json:"group_code"`
		GroupName string `json:"group_name"`
		IsActive  any    `json:"is_active"`
	}
	if err := getMaster(client, baseURL, "get-crop-group", nil, &groups); err != nil {
		return nil, err
	}
	byCode := map[string]crop{}
	for _, g := range groups {
		if !isActive(g.IsActive) {
			log.Printf("skipping crop group %s (%s): SATHI marks it inactive", g.GroupCode, g.GroupName)
			continue
		}
		if err := addGroupCrops(client, baseURL, g.GroupCode, byCode); err != nil {
			return nil, err
		}
	}
	crops := make([]crop, 0, len(byCode))
	for _, c := range byCode {
		crops = append(crops, c)
	}
	sort.Slice(crops, func(i, j int) bool { return crops[i].Code < crops[j].Code })
	return crops, nil
}

func addGroupCrops(client *http.Client, baseURL, group string, byCode map[string]crop) error {
	query := url.Values{"group_code": {group}}
	var rows []struct {
		CropCode string `json:"crop_code"`
		CropName string `json:"crop_name"`
	}
	if err := getMaster(client, baseURL, "get-crops-list", query, &rows); err != nil {
		return fmt.Errorf("crop group %s: %w", group, err)
	}
	log.Printf("crop group %s: %d crops", group, len(rows))
	for _, r := range rows {
		if _, repeated := byCode[r.CropCode]; repeated {
			log.Printf("crop %s (%s) is in more than one group; kept once", r.CropCode, r.CropName)
			continue
		}
		byCode[r.CropCode] = crop{r.CropCode, r.CropName}
	}
	return nil
}

func isActive(flag any) bool {
	switch v := flag.(type) {
	case float64:
		return v == 1
	case string:
		return v == "1"
	case bool:
		return v
	}
	return flag == nil
}

// getMaster calls one master endpoint and returns the data under EncryptedResponse.
func getMaster(client *http.Client, baseURL, path string, query url.Values, into any) error {
	resp, err := client.Get(baseURL + "/" + path + "?" + query.Encode())
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("SATHI %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SATHI %s answered HTTP %d", path, resp.StatusCode)
	}
	var answer masterAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return fmt.Errorf("SATHI %s: unreadable answer: %w", path, err)
	}
	if answer.EncryptedResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("SATHI %s: status %d, %s", path, answer.EncryptedResponse.StatusCode, answer.EncryptedResponse.Message)
	}
	return json.Unmarshal(answer.EncryptedResponse.Data, into)
}

// descriptorCode is what SATHI's contract and the chat API bind on, so it is not a setting.
const descriptorCode = "sathi-stock"

type object = map[string]any

// publishContext and the catalog's bppId, bppUri and visibleTo are Postman variables: the
// API collection's publish request fills them.
func publishContext() object {
	return object{"domain": "agriculture", "action": "catalog/publish", "version": "2.0.0",
		"transactionId": "{{$guid}}", "messageId": "{{$guid}}", "timestamp": "{{$isoTimestamp}}",
		"networkId": "{{networkId}}", "senderId": "{{senderId}}", "receiverId": "{{receiverId}}"}
}

var visibleTo = []string{"{{networkId}}", "{{otherNetworkId}}"}

func publishBody(districts []district, crops []crop, s settings) object {
	resources := make([]object, 0, len(districts))
	for _, d := range districts {
		resources = append(resources, resource(d, crops, s))
	}
	return object{
		"context": publishContext(),
		"message": object{
			"catalogs": []object{{
				"id": s.CatalogID, "bppId": "{{bppId}}", "bppUri": "{{bppUri}}",
				"descriptor": object{
					"code": "SATHI_SEED_STOCK", "name": "SATHI certified seed availability",
					"shortDesc": "On-demand certified seed stock by crop and district, from SATHI.",
					"longDesc": "Each resource is one district with its boundary and LGD codes, and lists the SATHI crops. " +
						"Select a resource with one crop to get the live lots and dealers holding stock in that district.",
				},
				"provider": object{"id": s.ProviderID, "descriptor": object{
					"code": "SATHI", "name": "SATHI (Seed Authentication, Traceability and Holistic Inventory)"}},
				"isActive":  true,
				"resources": resources,
			}},
			"publishDirectives": []object{{"catalogId": s.CatalogID, "catalogType": "REGULAR", "visibleTo": visibleTo}},
		},
	}
}

func resource(d district, crops []crop, s settings) object {
	subjects := make([]object, 0, len(crops))
	for _, c := range crops {
		subjects = append(subjects, object{"descriptor": object{"code": c.Code, "name": c.Name}})
	}
	return object{
		"id": fmt.Sprintf("res-%s:%s:%s", descriptorCode, d.StateCode, d.Code),
		"descriptor": object{
			"code": descriptorCode,
			"name": fmt.Sprintf("SATHI certified seed stock - %s, %s", d.Name, d.StateName),
		},
		"resourceAttributes": object{
			"@context": "{{sathi-resource-context}}", "@type": "openagrinet:AgricultureResource",
			"informationMode": "OnDemand", "subjectCategories": []string{"Crop"},
			"agricultureSubjects": subjects,
			"coverageAreas": []any{
				object{"codeScheme": "LGD", "areaLevel": "State", "areaCode": d.StateCode, "areaName": d.StateName},
				object{"codeScheme": "LGD", "areaLevel": "District", "areaCode": d.Code, "areaName": d.Name},
				d.Geometry,
			},
			"actions": []object{{"action": "select", "operationRef": "{{sathi-stock-operation-ref}}"}},
		},
	}
}

func encode(body object) ([]byte, error) {
	return json.Marshal(body)
}

// dotEnvPath is the builder's own .env: "sathi-seed/.env" when run from catalog-builders, as the
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
