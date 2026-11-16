package mediahealth

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// mediahealth is a store plus a probe over an injected Fetcher. The
// transports and the router call INTO it; it must never depend on them, and
// it must never dial on its own (offlinecache owns the guarded client). The
// one allowed daemon dependency is dp1-go's item type.
func TestImportBoundary(t *testing.T) {
	banned := []string{"/relayer", "/hub", "/mediator", "/commandrouter", "/offlinecache", "/uarewrite", "/status", "/cdp", "/devicectl"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, imp := range src.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "net/http" {
				// Only for the http.Header type on FetchResponse; a client
				// construction would be a boundary violation reviewers look
				// for by hand.
				continue
			}
			if !strings.Contains(path, "feral-controld") {
				continue
			}
			for _, deny := range banned {
				if strings.HasSuffix(path, deny) || strings.Contains(path, deny+"/") {
					t.Errorf("%s imports %s — mediahealth must not depend on %s", file, path, deny)
				}
			}
		}
	}
}
