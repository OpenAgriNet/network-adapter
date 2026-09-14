package main

import (
	"fmt"
	"os"
	"strings"
)

// fail prints one message and exits non-zero.
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mandi_publish: "+format+"\n", args...)
	os.Exit(1)
}

// firstNonEmpty returns the first value that is set, so a flag beats an
// environment variable and both beat nothing.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// splitStates reads the comma-separated flag, dropping blanks so a trailing
// comma is not a state code.
func splitStates(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// loadEnvFile reads a simple KEY=VALUE file and populates missing environment variables.
func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			if os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
}
