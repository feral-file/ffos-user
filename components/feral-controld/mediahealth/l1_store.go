package mediahealth

import (
	"fmt"
	"sort"
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
			// A broken record is never rendered, inside or past its L0
			// window: L0's own recheck decides when a 404 gets another
			// look, and a frame of an error page proves nothing a byte
			// verdict did not.
			if rec.State == StateBroken {
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

// ApplyRender records one L1 render on source. It never touches the L0
// recheck fields (RecheckAfter, RetryAfter) nor a broken verdict set by L0
// or play: an L1 verdict only ever moves a record between ok and suspect.
//
//   - ok: l1 = ok, next render after RecheckL1OK; a suspect record heals to
//     ok (layer l1).
//   - blank: the first blank schedules an immediate confirmation render
//     (next = now, the runner uses the longer settle); the second
//     consecutive blank concludes suspect/render_blank for RecheckL1Suspect.
//   - known_bad: suspect/render_known_bad immediately, for RecheckL1Suspect.
//   - stalled: no evidence; lastError only, next after RecheckL1Stalled.
func (s *Store) ApplyRender(source string, r RenderResult) (*Record, error) {
	now := s.now()
	return s.update(source, func(rec *Record, _ bool) bool {
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
		switch r.Verdict {
		case RenderStalled:
			l1.Next = now.Add(RecheckL1Stalled)
		case RenderOK:
			l1.ConsecutiveBlank = 0
			l1.Next = now.Add(RecheckL1OK)
			if rec.State == StateSuspect {
				rec.State = StateOK
				rec.Layer = LayerL1
				rec.Category, rec.Reason = "", ""
				rec.CheckedAt = now
			}
		case RenderBlank:
			l1.ConsecutiveBlank++
			if l1.ConsecutiveBlank < 2 {
				l1.Next = now // confirmation render, now
				break
			}
			l1.Next = now.Add(RecheckL1Suspect)
			if !brokenElsewhere {
				rec.State = StateSuspect
				rec.Layer = LayerL1
				rec.Reason = ReasonRenderBlank
				rec.Category = CategoryOf(ReasonRenderBlank, 0)
				rec.CheckedAt = now
			}
		case RenderKnownBad:
			l1.ConsecutiveBlank = 0
			l1.Label = r.Label
			l1.Next = now.Add(RecheckL1Suspect)
			if !brokenElsewhere {
				rec.State = StateSuspect
				rec.Layer = LayerL1
				rec.Reason = ReasonRenderKnownBad
				rec.Category = CategoryOf(ReasonRenderKnownBad, 0)
				rec.CheckedAt = now
			}
		default:
			return false
		}
		rec.L1 = l1
		return true
	})
}
