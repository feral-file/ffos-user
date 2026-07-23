package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provisioning domain must work from cold start with no relayer and (for the
// non-narration packages) no CDP: a device that cannot reach the internet still has
// to raise its SoftAP and serve the captive portal. Importing relayer/ (or cdp/ for
// the pure provisioning-path packages) would silently re-couple provisioning to
// infrastructure that is down in exactly the scenario provisioning exists for.
// setupui is the deliberate exception for cdp: narration is fire-and-forget and
// runtime-tolerant of a dead CDP target, which its own tests prove.
var importDenylist = map[string][]string{
	"wifictl":      {"/relayer", "/cdp"},
	"softap":       {"/relayer", "/cdp"},
	"portal":       {"/relayer", "/cdp"},
	"provisioning": {"/relayer", "/cdp"},
	"setupui":      {"/relayer"},
	"otagate":      {"/relayer"},
}

func TestProvisioningDomainImportIsolation(t *testing.T) {
	fset := token.NewFileSet()
	for pkg, banned := range importDenylist {
		if _, err := os.Stat(pkg); os.IsNotExist(err) {
			// Packages land incrementally on this branch; a missing directory is
			// not a violation, it just has nothing to check yet.
			continue
		}
		files, err := filepath.Glob(filepath.Join(pkg, "*.go"))
		if err != nil {
			t.Fatalf("globbing %s: %v", pkg, err)
		}
		for _, file := range files {
			src, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", file, err)
			}
			for _, imp := range src.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if !strings.Contains(path, "feral-controld") {
					continue
				}
				for _, deny := range banned {
					if strings.HasSuffix(path, deny) || strings.Contains(path, deny+"/") {
						t.Errorf("%s imports %s — %s must not depend on %s (provisioning-domain isolation)",
							file, path, pkg, deny)
					}
				}
			}
		}
	}
}
