// Package ff1config reads small slices of /home/feralfile/ff1-config.json so
// daemons stay aligned with feral-setupd without duplicating policy in constants.
package ff1config

import (
	"encoding/json"
	"os"
	"strings"
)

// ConfigPath matches feral-setupd UPDATER_LOCAL_CONFIG_PATH.
const ConfigPath = "/home/feralfile/ff1-config.json"

// DefaultWebappURL is the device-local player when ff1-config omits webapp_url or it is empty.
// Explicit remote (or other) URLs go in ff1-config webapp_url.
// Keep in sync with components/feral-setupd/src/constant.rs WEBAPP_URL.
const DefaultWebappURL = "http://127.0.0.1:8080/"

type localConfig struct {
	WebappURL *string `json:"webapp_url"`
}

// ResolveWebappURL returns webapp_url from ff1-config.json when set (override), otherwise DefaultWebappURL.
// Callers use this for CDP navigation that must match the player URL feral-setupd already chose.
func ResolveWebappURL() string {
	return ResolveWebappURLFromPath(ConfigPath)
}

// ResolveWebappURLFromPath is exported for unit tests; production code should use ResolveWebappURL.
func ResolveWebappURLFromPath(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // G304: production uses fixed ConfigPath; tests use temp files only.
	if err != nil {
		return DefaultWebappURL
	}
	var cfg localConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultWebappURL
	}
	if cfg.WebappURL == nil {
		return DefaultWebappURL
	}
	u := strings.TrimSpace(*cfg.WebappURL)
	if u == "" {
		return DefaultWebappURL
	}
	return u
}
