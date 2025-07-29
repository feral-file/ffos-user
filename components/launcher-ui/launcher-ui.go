package main

import (
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

func main() {
	// Base file URL
	baseURL := "file:///opt/feral/ui/launcher/index.html"
	u, err := url.Parse(baseURL)
	if err != nil {
		log.Fatalf("Invalid base URL: %v", err)
	}

	// Parse key=value arguments
	q := u.Query()
	for _, arg := range os.Args[1:] {
		if parts := splitArg(arg); parts != nil {
			q.Set(parts[0], parts[1])
		}
	}

	// Final URL with query parameters
	u.RawQuery = q.Encode()
	fullURL := u.String()
    log.Printf("Launching URL: %s", fullURL)
	// Launch Chromium
	cmd := exec.Command("cage", "--", "/usr/bin/chromium", "--ozone-platform=wayland",
		"--app="+fullURL,
		"--disable-features=TranslateUI",
		"--noerrdialogs",
		"--start-fullscreen")

	if err := cmd.Start(); err != nil {
		log.Fatalf("Error starting Chromium: %v", err)
	}

	cmd.Wait()
}

// Helper to split "key=value" into [key, value]
func splitArg(arg string) []string {
	if i := strings.Index(arg, "="); i != -1 {
		return []string{arg[:i], arg[i+1:]}
	}
	return nil
}
