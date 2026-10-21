package main

import (
	"os"
	"testing"
)

// LOCKSTEP with ffos (.github/workflows/build-components.yaml,
// manual-build-components.yaml, build-image-from-tags.yml and the image
// workflows' controld.json gate): ffos emits the package capability marker
// provides=('feral-controld-control-v1') and keeps an image's .control block
// only when this checkout contains components/feral-controld/control/.
// Renaming or moving that package silently drops the marker, and every
// device holding control state then holds feral-controld and refuses every
// image. Change both repositories together.
func TestControlCapabilityDirectoryLockstep(t *testing.T) {
	fi, err := os.Stat("control")
	if err != nil || !fi.IsDir() {
		t.Fatalf("components/feral-controld/control must exist (ffos keys the feral-controld-control-v1 marker on it): %v", err)
	}
}
