// Package devinauth reads the credentials file written by `devin auth login`.
//
// Devin CLI stores its session credentials in
// ~/.local/share/devin/credentials.toml as a flat list of quoted
// key = "value" pairs. Only a small subset of TOML is needed here, so the
// file is parsed line-by-line instead of pulling in a full TOML decoder.
package devinauth

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Credentials mirrors the fields of credentials.toml written by
// `devin auth login`.
type Credentials struct {
	// APIKey is the session token (`windsurf_api_key`, format
	// `devin-session-token$...`) used against the Connect API.
	APIKey string
	// APIServerURL is the Connect API base URL (`api_server_url`).
	APIServerURL string
	// WebappHost is the Devin web app host (`devin_webapp_host`).
	WebappHost string
	// DevinAPIURL is the public REST API URL (`devin_api_url`).
	DevinAPIURL string
}

// DefaultPath returns the credentials file path: the
// DEVIN_CREDENTIALS_FILE environment variable when set, otherwise
// ~/.local/share/devin/credentials.toml.
func DefaultPath() string {
	if path := strings.TrimSpace(os.Getenv("DEVIN_CREDENTIALS_FILE")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "devin", "credentials.toml")
}

// Load reads and parses a credentials.toml file.
func Load(path string) (Credentials, error) {
	file, err := os.Open(path)
	if err != nil {
		return Credentials{}, err
	}
	defer file.Close()

	var credentials Credentials
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseLine(scanner.Text())
		if !ok {
			continue
		}
		switch key {
		case "windsurf_api_key":
			credentials.APIKey = value
		case "api_server_url":
			credentials.APIServerURL = value
		case "devin_webapp_host":
			credentials.WebappHost = value
		case "devin_api_url":
			credentials.DevinAPIURL = value
		}
	}
	if err := scanner.Err(); err != nil {
		return Credentials{}, fmt.Errorf("read credentials %q: %w", path, err)
	}
	return credentials, nil
}

// parseLine parses a single `key = "value"` line. It returns ok=false for
// blank lines, comments, section headers, and malformed lines.
func parseLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
		return "", "", false
	}
	name, raw, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	raw = strings.TrimSpace(raw)
	if name == "" || raw == "" {
		return "", "", false
	}
	parsed, err := strconv.Unquote(raw)
	if err != nil {
		// Tolerate unquoted scalar values (numbers, booleans) — none of the
		// fields we consume are unquoted, but skipping silently is safer than
		// failing the whole file on an unexpected value shape.
		return "", "", false
	}
	return name, parsed, true
}

// ErrNoCredentials is returned when no usable credential source is found.
var ErrNoCredentials = errors.New("no Devin credentials found")
