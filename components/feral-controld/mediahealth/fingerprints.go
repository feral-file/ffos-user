package mediahealth

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Learned fingerprints (feral-file/ffos-user#392): the known-bad list fills
// itself from the one conflict that proves a frame is not art. When the
// wall's frame of a work renders but the same play's content-type check
// said the document is not there (a 400/404/410 — the HEAD wins,
// renderedFrame; "the same play" is checked by learnableErrorPage), the
// frame is the gateway's error page: a rendered artwork can never produce a
// 404. Its pHash is a candidate. Candidates within learnClusterDistance of
// each other are one cluster; a cluster seen on learnPromoteSources
// different works is a learned known-bad fingerprint, so the same error page
// served with a 200 (an Arweave gateway's, say) is recognized on any work.
// Real art cannot become a candidate, so it cannot be learned.

const (
	// learnClusterDistance groups candidate hashes of one error page: the
	// same page over different works differs by a few bits (a CID in the
	// text, the title label masked on the wall).
	learnClusterDistance = 6
	// learnPromoteSources different works must show the same page before it
	// is learned: one work could be a coincidence, three is a pattern.
	learnPromoteSources = 3
	// learnMaxDistance is a learned fingerprint's match tolerance.
	learnMaxDistance = 6
	// maxLearnClusters bounds the bucket; the least recently seen cluster
	// that is not promoted is evicted first.
	maxLearnClusters = 256
	// maxClusterSources bounds the works remembered per cluster.
	maxClusterSources = 16
	// learnedLabel is a learned fingerprint's label on a verdict.
	learnedLabel = "learned"
)

var bucketFingerprints = []byte("fingerprints")

// learnCluster is one bucketFingerprints value.
type learnCluster struct {
	Hash     string    `json:"hash"`
	Sources  []string  `json:"sources"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Promoted bool      `json:"promoted,omitempty"`
}

// LearnedFingerprints are the promoted clusters, for the classifiers to use
// beside the configured mediaHealth.l1.knownBad. A copy; safe to keep.
func (s *Store) LearnedFingerprints() []Fingerprint {
	s.learnedMu.RLock()
	defer s.learnedMu.RUnlock()
	return slices.Clone(s.learned)
}

// loadLearned mirrors the promoted clusters into memory at Open.
func (s *Store) loadLearned() error {
	var learned []Fingerprint
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFingerprints).ForEach(func(_, v []byte) error {
			var c learnCluster
			if json.Unmarshal(v, &c) != nil || !c.Promoted {
				return nil
			}
			if fp, err := ParseFingerprint(c.Hash, learnMaxDistance, learnedLabel); err == nil {
				learned = append(learned, fp)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	s.learnedMu.Lock()
	s.learned = learned
	s.learnedMu.Unlock()
	return nil
}

// learnErrorPage records a frame that rendered where the content-type check
// said the document is not there. Best effort: a failure costs a candidate,
// never an observation.
func (s *Store) learnErrorPage(source string, hash uint64) {
	key := Key(source)
	now := s.now()
	var promoted *learnCluster
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFingerprints)
		var (
			matchKey []byte
			match    learnCluster
			oldestK  []byte
			oldest   time.Time
			count    int
		)
		if err := b.ForEach(func(k, v []byte) error {
			count++
			var c learnCluster
			if json.Unmarshal(v, &c) != nil {
				return nil
			}
			if h, perr := ParseFingerprint(c.Hash, 0, ""); perr == nil && matchKey == nil && HashDistance(h.Hash, hash) <= learnClusterDistance {
				matchKey, match = append([]byte(nil), k...), c
			}
			if !c.Promoted && (oldestK == nil || c.Last.Before(oldest)) {
				oldestK, oldest = append([]byte(nil), k...), c.Last
			}
			return nil
		}); err != nil {
			return err
		}
		if matchKey == nil {
			if count >= maxLearnClusters && oldestK != nil {
				if err := b.Delete(oldestK); err != nil {
					return err
				}
			}
			match = learnCluster{Hash: fmt.Sprintf("%016x", hash), First: now}
			matchKey = []byte(match.Hash)
		}
		match.Last = now
		if !slices.Contains(match.Sources, key) && len(match.Sources) < maxClusterSources {
			match.Sources = append(match.Sources, key)
		}
		if !match.Promoted && len(match.Sources) >= learnPromoteSources {
			match.Promoted = true
			promoted = &match
		}
		raw, err := json.Marshal(match)
		if err != nil {
			return err
		}
		return b.Put(matchKey, raw)
	})
	if err != nil {
		s.logger.Warn("media health: recording an error-page candidate failed", zap.Error(err))
		return
	}
	if promoted == nil {
		return
	}
	fp, perr := ParseFingerprint(promoted.Hash, learnMaxDistance, learnedLabel)
	if perr != nil {
		return
	}
	s.learnedMu.Lock()
	s.learned = append(s.learned, fp)
	s.learnedMu.Unlock()
	s.logger.Info("media health: learned a known-bad fingerprint (an error page seen rendering on several works whose content-type check said 404)",
		zap.String("phash", promoted.Hash), zap.Int("works", len(promoted.Sources)))
}

// learnConflictWindow bounds a refusal and the wall frame of the same play:
// the player's retries (3, 3 s apart) and the embedded load watchdog
// (45 s), then the wall settle (15 s), fit well inside it. A frame later
// than this is another play, and the document may since have come back.
const learnConflictWindow = 5 * time.Minute

// learnableErrorPage reports a wall frame (taken at `at`) that rendered
// over the content-type check's definite absence in the SAME play: the
// refusal was not overruled by the kiosk's own GET (fetchRunOpen ignores a
// superseded one), was taken on the gateway the record routes through now,
// and is at most learnConflictWindow old. Only then is the frame the
// gateway's error page — renderedFrame refuses it (the HEAD wins) and its
// hash becomes a candidate.
//
// The nightly L1 render never qualifies: it counts a frame only when its
// own GET of the document returned 2xx, and a served GET outranks the HEAD
// — that frame is the document, not an error page (PR #450 review, F1: a
// healthy frame through a fallback gateway was learned as known-bad).
func (r *Record) learnableErrorPage(at time.Time) bool {
	if !r.fetchRunOpen() || !definiteAbsence(r.Plays.LastFail.Code) {
		return false
	}
	f := r.Plays.LastFail
	return f.ServedBy == r.ServedBy && !at.Before(f.At) && at.Sub(f.At) <= learnConflictWindow
}
