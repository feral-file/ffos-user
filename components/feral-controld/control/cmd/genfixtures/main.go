// Command genfixtures writes the control protocol v1 conformance vectors
// (docs/ff1-control-v1.md §12). Run from components/feral-controld:
//
//	go run ./control/cmd/genfixtures
//
// Output is deterministic; control's golden test fails if the committed
// files differ from what the code produces.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/feral-file/ffos-user/components/feral-controld/control/internal/fixturegen"
)

func main() {
	out := flag.String("out", "control/testdata/fixtures", "output directory")
	flag.Parse()
	files, err := fixturegen.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genfixtures:", err)
		os.Exit(1)
	}
	// Replace the whole set so a renamed vector does not linger.
	for _, sub := range []string{"positive", "negative"} {
		if err := os.RemoveAll(filepath.Join(*out, sub)); err != nil {
			fmt.Fprintln(os.Stderr, "genfixtures:", err)
			os.Exit(1)
		}
	}
	for name, b := range files {
		p := filepath.Join(*out, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			fmt.Fprintln(os.Stderr, "genfixtures:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "genfixtures:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %d files to %s\n", len(files), *out)
}
