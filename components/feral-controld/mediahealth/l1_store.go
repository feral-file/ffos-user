package mediahealth

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

// l1Cand is one L1 candidate: never-rendered sources carry a zero next
// and so sort first.
type l1Cand struct {
	source string
	next   time.Time
}

// L1Candidates returns up to limit sources due for an L1 render, never
// rendered first and then by their next due time: software-class records
// (isSoftwareRecord) that are not broken by L0 or play (a 404 needs no
// render) and whose l1 sub-record is absent or due. A full scan of
// the items bucket: it runs at most once per render, at night, and a scan
// of a hundred thousand records costs well under a second — far cheaper
// than a second index to keep in step with every write.
func (s *Store) L1Candidates(now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	var best []l1Cand
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketItems).ForEach(func(_, v []byte) error {
			rec, derr := s.decode(v)
			if derr != nil || rec == nil || !isSoftwareRecord(rec) {
				return nil
			}
			// A broken record is rendered only when a frame could heal it:
			// a render-layer verdict (a play or a frame said it did not
			// paint). An L0 or fetch-layer broken is never rendered: L0's
			// own recheck decides when a 404 gets another look, and a frame
			// of an error page proves nothing a byte verdict did not.
			if rec.State == StateBroken && !rec.renderLayerBroken() {
				return nil
			}
			var next time.Time
			if rec.L1 != nil {
				if now.Before(rec.L1.Next) {
					return nil
				}
				next = rec.L1.Next
			}
			best = append(best, l1Cand{source: rec.Source, next: next})
			if len(best) > limit*4 {
				sortCands(best)
				best = best[:limit]
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sortCands(best)
	if len(best) > limit {
		best = best[:limit]
	}
	out := make([]string, len(best))
	for i, c := range best {
		out[i] = c.source
	}
	return out, nil
}

func sortCands(c []l1Cand) {
	sort.SliceStable(c, func(i, j int) bool { return c[i].next.Before(c[j].next) })
}

// ApplyRender records one L1 render on source. It never touches a broken
// verdict set by L0 or by the fetch layer of a play, nor the L0 recheck
// fields of an ok or suspect record.
//
//   - ok: a rendered frame (renderedFrame, layers.go): ok with layer l1 over
//     unknown, ok, suspect or a render-layer broken, never over an L0 or
//     fetch-layer broken; changes nothing while the open fetch run's last
//     failure is a 400/404/410. Next render after RecheckL1OK.
//   - blank: the first blank schedules an immediate confirmation render
//     (next = now, the runner uses the longer settle); the second
//     consecutive blank concludes a render negative.
//   - known_bad: concludes a render negative immediately.
//   - a render negative (step 2) is suspect, or broken — with the ordinary
//     broken window, so RecheckAfter is set — when mediaHealth.render-
//     Conclusive is on and an earlier-night L1 negative or an open play
//     failure run corroborates it; next render after RecheckL1Suspect.
//   - stalled: no evidence; lastError only, next after RecheckL1Stalled.
func (s *Store) ApplyRender(source string, r RenderResult) (*Record, error) {
	now := s.now()
	contentAddressed := s.contentAddressed(source)
	// An L1 frame is never learned as an error page: it is counted only
	// when its own GET of the document was served 2xx, which outranks the
	// content-type check's refusal (learnableErrorPage).
	rec, err := s.update(source, func(rec *Record, _ bool) bool {
		l1 := rec.L1
		if l1 == nil {
			l1 = &L1Verdict{}
		}
		l1.Verdict = r.Verdict
		l1.At = now
		l1.MainStatus = r.MainStatus
		l1.LastError = r.Err
		l1.Label = ""
		if r.Verdict != RenderStalled {
			l1.PHash = fmt.Sprintf("%016x", r.PHash)
			l1.Variance = r.Variance
		}
		brokenElsewhere := rec.State == StateBroken && rec.Layer != LayerL1
		// negative concludes a blank or known-bad render (step 2): suspect,
		// or broken when render verdicts are conclusive AND something
		// independent agrees — an L1 negative on an earlier night, or an
		// open play failure run. A same-night confirmation is not
		// independent. When that run is a fetch one (the content-type
		// check refused the document), both layers failed and the fetch
		// reason is the verdict.
		negative := func(reason Reason) {
			corroborated := (!l1.NegativeAt.IsZero() && now.Sub(l1.NegativeAt) >= l1NightGap) ||
				rec.Plays.ConsecutiveFail > 0
			l1.NegativeAt = now
			if brokenElsewhere {
				return
			}
			conclude := s.renderConclusive && corroborated
			if conclude && rec.fetchRunOpen() {
				status, _ := strconv.Atoi(rec.Plays.LastFail.Code)
				rec.concludeRender(ReasonRenderDetect, LayerPlay, now, true)
				rec.Category = playCategory(ReasonRenderDetect, status, contentAddressed)
				rec.HTTPStatus = status
				return
			}
			rec.concludeRender(reason, LayerL1, now, conclude)
		}
		switch r.Verdict {
		case RenderStalled:
			// ConsecutiveBlank is left as it was: a stalled confirmation
			// keeps the blank pending, so the next render is still the
			// long confirmation (the runner keys on the count, not on
			// Verdict).
			l1.Next = now.Add(RecheckL1Stalled)
		case RenderOK:
			l1.ConsecutiveBlank = 0
			l1.Next = now.Add(RecheckL1OK)
			// A frame of the work rendered: the play layer's evidence for
			// an HTML work, which an iframe's `ready` is not
			// (renderedFrame, layers.go).
			l1.NegativeAt = time.Time{}
			rec.renderedFrame(now, LayerL1)
		case RenderBlank:
			l1.ConsecutiveBlank++
			if l1.ConsecutiveBlank < 2 {
				l1.Next = now // confirmation render, now
				break
			}
			l1.Next = now.Add(RecheckL1Suspect)
			negative(ReasonRenderBlank)
		case RenderKnownBad:
			l1.ConsecutiveBlank = 0
			l1.Label = r.Label
			l1.Next = now.Add(RecheckL1Suspect)
			negative(ReasonRenderKnownBad)
		default:
			return false
		}
		rec.L1 = l1
		return true
	})
	return rec, err
}

// DeferL1 moves an existing record's next L1 render to next without a
// verdict: the render was canceled by the resource gate (RecheckL1Deferred).
// The record's state and any earlier L1 verdict stand.
func (s *Store) DeferL1(source string, next time.Time, reason string) error {
	_, err := s.update(source, func(rec *Record, existed bool) bool {
		if !existed {
			return false
		}
		if rec.L1 == nil {
			rec.L1 = &L1Verdict{}
		}
		rec.L1.Next = next
		rec.L1.LastError = reason
		return true
	})
	return err
}
