package mediahealth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDrainer_SeqCursorCastResetAndGap(t *testing.T) {
	var d Drainer
	fresh, gap, _ := d.Drain(&Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 2, Source: "b", Kind: OutcomeOK, Stage: StageReady},
		{Seq: 1, Source: "a", Kind: OutcomeFail, Stage: StageLoad},
	}})
	assert.Equal(t, int64(0), gap)
	assert.Equal(t, []string{"a", "b"}, sourcesOf(fresh), "returned in seq order")

	fresh, _, _ = d.Drain(&Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "a"}, {Seq: 2, Source: "b"}, {Seq: 3, Source: "c"},
	}})
	assert.Equal(t, []string{"c"}, sourcesOf(fresh), "already-seen entries are not returned again")

	fresh, gap, _ = d.Drain(&Outcomes{CastSeq: 1, Entries: []Outcome{{Seq: 10, Source: "j"}}})
	assert.Equal(t, []string{"j"}, sourcesOf(fresh))
	assert.Equal(t, int64(6), gap, "the ring wrapped: 4..9 were never seen")

	fresh, gap, _ = d.Drain(&Outcomes{CastSeq: 2, Entries: []Outcome{{Seq: 1, Source: "x"}}})
	assert.Equal(t, []string{"x"}, sourcesOf(fresh), "a new cast restarts the cursor")
	assert.Equal(t, int64(0), gap)

	fresh, _, _ = d.Drain(nil)
	assert.Empty(t, fresh)
	fresh, _, _ = d.Drain(&Outcomes{CastSeq: 2, Entries: []Outcome{{Seq: 5, Source: ""}}})
	assert.Empty(t, fresh, "an entry without a source is dropped")
}

func sourcesOf(entries []Outcome) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Source)
	}
	return out
}

func TestReasonForStage(t *testing.T) {
	assert.Equal(t, ReasonRenderDetect, reasonForStage(StageDetect))
	assert.Equal(t, ReasonRenderExhausted, reasonForStage(StageInferred))
	assert.Equal(t, ReasonWebGLLost, reasonForStage(StageWebGL))
	assert.Empty(t, reasonForStage(StageReady))
	assert.Empty(t, reasonForStage(StageHint))
	assert.Empty(t, reasonForStage("made-up"))
}
