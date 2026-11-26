// Package areas reads the LGD states and districts in this folder: areas.csv for codes and
// names, areas.geojsonl for boundaries. See README.md for the sources.
package areas

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Area is one LGD area from areas.csv.
type Area struct {
	Code, Level, Name       string
	ParentCode, ParentLevel string
}

var columns = []string{"code_scheme", "area_code", "area_level", "area_name", "parent_code", "parent_level"}

// Key names one area in the boundaries map. An LGD code repeats across levels, so the
// level is part of it.
func Key(code, level string) string { return code + ":" + level }

// Read returns the LGD rows of areas.csv and the LGD boundaries of areas.geojsonl, keyed by Key.
func Read(dir string) ([]Area, map[string]json.RawMessage, error) {
	rows, err := readRows(filepath.Join(dir, "areas.csv"))
	if err != nil {
		return nil, nil, err
	}
	shapes, err := readBoundaries(filepath.Join(dir, "areas.geojsonl"))
	if err != nil {
		return nil, nil, err
	}
	return rows, shapes, nil
}

func readRows(path string) ([]Area, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil || len(records) < 2 {
		return nil, fmt.Errorf("%s: no area rows (%v)", path, err)
	}
	at, err := columnIndexes(records[0])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var rows []Area
	for _, r := range records[1:] {
		if r[at["code_scheme"]] != "LGD" {
			continue
		}
		rows = append(rows, Area{Code: r[at["area_code"]], Level: r[at["area_level"]], Name: r[at["area_name"]],
			ParentCode: r[at["parent_code"]], ParentLevel: r[at["parent_level"]]})
	}
	return rows, nil
}

// columnIndexes names every missing column at once.
func columnIndexes(header []string) (map[string]int, error) {
	at := map[string]int{}
	for i, name := range header {
		at[name] = i
	}
	var missing []string
	for _, name := range columns {
		if _, ok := at[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing columns: %s", strings.Join(missing, ", "))
	}
	return at, nil
}

func readBoundaries(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	shapes := map[string]json.RawMessage{}
	for n, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var f struct {
			Properties struct {
				Scheme string `json:"code_scheme"`
				Code   string `json:"area_code"`
				Level  string `json:"area_level"`
			}
			Geometry json.RawMessage
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n+1, err)
		}
		if f.Properties.Scheme == "LGD" {
			shapes[Key(f.Properties.Code, f.Properties.Level)] = f.Geometry
		}
	}
	return shapes, nil
}
