package mediahealth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A render verdict (the work mounted here and failed) answers the cast pass
// from the store without a request, but as inconclusive: an owner casting
// that one work gets it played, not a sourceUnreachable. A play-layer
// content-type refusal keeps its fetch category and still reads dead.
func TestPreflight_RenderVerdictDoesNotRejectACast(t *testing.T) {
	cases := []struct {
		name     string
		play     func() []Outcome
		category Category
		want     Verdict
	}{
		{"render failure", infuraPlay, CategoryRenderFailed, VerdictInconclusive},
		{"content-type refusal", func() []Outcome {
			return []Outcome{{Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1}, iframeReady}
		}, CategorySourceMissing, VerdictDead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlayFeed(t, true)
			f.src = "https://origin.example/work.html"
			f.emit(tc.play()...)
			f.emit(tc.play()...)
			rec := f.record()
			require.Equal(t, StateBroken, rec.State)
			require.Equal(t, tc.category, rec.Category)

			fetcher := newTableFetcher()
			p := &Prober{Probe: &Probe{Fetcher: fetcher, Validator: NewContentValidator(DefaultProbeMaxBytes, nil)}, Store: f.s.Store(), Now: f.clock.Now}
			results := p.Preflight(context.Background(), []string{f.src})
			require.Len(t, results, 1)
			assert.True(t, results[0].FromStore, "a stored verdict costs no request")
			assert.Equal(t, tc.want, results[0].Verdict)
			assert.Zero(t, fetcher.count(f.src))
		})
	}
}
