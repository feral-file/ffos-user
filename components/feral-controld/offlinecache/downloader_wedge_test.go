package offlinecache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

// fakeExecCmd is a bare wrapper.ExecCmd stand-in used only to give
// downloader.cmd a distinguishable non-nil value in white-box tests —
// none of its methods are ever invoked.
type fakeExecCmd struct{ label string }

func (f *fakeExecCmd) String() string                  { return f.label }
func (f *fakeExecCmd) Run() error                      { return nil }
func (f *fakeExecCmd) Start() error                    { return nil }
func (f *fakeExecCmd) Wait() error                     { return nil }
func (f *fakeExecCmd) Output() ([]byte, error)         { return nil, nil }
func (f *fakeExecCmd) CombinedOutput() ([]byte, error) { return nil, nil }

// TestDownloader_ReapCompleted_DoesNotClobberNewerGeneration is a
// white-box regression test (package offlinecache, not offlinecache_test
// — see cdpsession_wedge_test.go/dial_wedge_test.go for the same pattern)
// for the identity guard reapCompleted's doc describes: through the
// public Acquire/Release/Close API this cannot actually happen anymore
// (Acquire now always waits for a prior generation's procDone before
// starting a replacement — see TestDownloader_Acquire_
// WaitsForPriorGenerationReapBeforeStartingReplacement), but the guard
// itself is what makes that safe rather than merely lucky, and this
// pins it directly: a reaper for an OLDER generation (gen1) that wakes
// up after a NEWER generation (gen2) is already tracked in d.cmd/
// d.procDone must not clear gen2's live state.
func TestDownloader_ReapCompleted_DoesNotClobberNewerGeneration(t *testing.T) {
	d := &downloader{sem: make(chan struct{}, 1), logger: zaptest.NewLogger(t)}

	gen1Done := make(chan struct{})
	gen2Cmd := &fakeExecCmd{label: "gen2"}
	gen2Done := make(chan struct{})

	// Simulate gen2 already being the current, live generation — as if
	// gen1's own earlier reap (a still-earlier hop in a longer chain, or
	// any other path) had already completed and start() had since run
	// again for gen2.
	d.mu.Lock()
	d.cmd = gen2Cmd
	d.procDone = gen2Done
	d.mu.Unlock()

	// gen1's reaper wakes up late (its own done channel — gen1Done — was
	// never gen2Done, so the identity check must reject it) and must not
	// touch gen2's state.
	d.reapCompleted(gen1Done)

	<-gen1Done // reapCompleted must still close the generation's own channel
	d.mu.Lock()
	assert.Same(t, gen2Cmd, d.cmd, "a late-waking older generation's reaper must not clear a newer generation's cmd")
	assert.Equal(t, gen2Done, d.procDone, "a late-waking older generation's reaper must not clear a newer generation's procDone")
	d.mu.Unlock()

	// gen2's own reaper, later, still correctly clears gen2's state.
	d.reapCompleted(gen2Done)
	<-gen2Done
	d.mu.Lock()
	assert.Nil(t, d.cmd)
	assert.Nil(t, d.procDone)
	d.mu.Unlock()
}
