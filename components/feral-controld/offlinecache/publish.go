package offlinecache

import "sync"

// publishGuardStore makes "write a record's blobs, then save the record"
// atomic with respect to GC, for producers that run outside the Service's
// worker (OnchfsMaterializer, PR #461 review F3).
//
// GC is mark-and-sweep: it lists the blobs saved records reference, then
// deletes every other blob. The Service runs its captures and its GC on one
// worker, so they never overlap. A producer on another goroutine can: a GC
// that marks before the producer saves its record and sweeps after it
// deletes the producer's new blobs and leaves a complete, fresh record
// pointing at nothing — and no check after the save can see a sweep that
// has not happened yet. So GC takes this guard exclusively for its whole
// run, and a publication holds it shared from its first WriteBlob through
// its SaveItem.
//
// It is the outermost Store BootstrapCore builds, so the Service's GC goes
// through it. newDiskBudgetFromStore unwraps it to find the budget store.
type publishGuardStore struct {
	Store
	mu sync.RWMutex
}

// GC runs the wrapped store's GC while no publication is in progress.
func (s *publishGuardStore) GC() (removedBlobs int, freedBytes int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.GC()
}

// Publish runs fn, which writes blobs and saves the record that references
// them, while no GC can run. fn must not call GC.
func (s *publishGuardStore) Publish(fn func() error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn()
}

// publisher is the optional Store capability OnchfsMaterializer uses.
type publisher interface {
	Publish(fn func() error) error
}
