package control

import (
	"errors"
	"fmt"
	"path/filepath"
)

// The owner's LAN switch (§7.2, §8 lan.json, feral-file#3551).
//
// Open (the default) lets anyone on the device's own network run the
// LAN-open set (the controller tier) in plaintext on an owned device. Closed
// puts the LAN back to "envelopes only": plaintext is refused with
// enrollment_required, exactly as before the switch existed.
//
// The switch lives in its own side file, never in grants.json: grants.json
// is strictly parsed and an unknown member LOCKS the device, and a rollback
// to a build that does not know the switch must not lock anything. The
// failure direction is deliberate:
//   - missing file: open (the default; a fresh identity has none);
//   - a gen that is not the identity's: open (the file belongs to a dead
//     identity, like display_grants.json; the next write overwrites it);
//   - any other read or parse failure: CLOSED. A corrupt switch must not
//     reopen the LAN the owner closed. The owner rewrites it with
//     setLanControl; nothing else depends on it.
const lanFile = "lan.json"

// CmdSetLanControl is the owner's switch command: {"open":bool} →
// {"open":bool}. Owner tier; handled by the Authority, never by commandrouter.
const CmdSetLanControl = "setLanControl"

// Values of Status.LAN (§11).
const (
	LANOpen   = "open"
	LANClosed = "closed"
)

var errLANGenMismatch = errors.New("lan.json: generation mismatch")

// loadLAN reads lan.json; see the file comment for the failure direction.
// err is non-nil only for the fail-closed case, so the caller can log it.
func loadLAN(fsys FS, dir, gen string) (open bool, err error) {
	raw, err := fsys.ReadFile(filepath.Join(dir, lanFile))
	if isNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("lan.json: %w", err)
	}
	open, err = parseLAN(raw, gen)
	if errors.Is(err, errLANGenMismatch) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return open, nil
}

func parseLAN(raw []byte, gen string) (bool, error) {
	m, err := strictObject(raw, []string{"v", "gen", "open"}, nil)
	if err != nil {
		return false, fmt.Errorf("lan.json: %w", err)
	}
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return false, errors.New("lan.json: unsupported version")
	}
	if err := expectString(m, "gen", gen); err != nil {
		return false, fmt.Errorf("%w: %w", errLANGenMismatch, err)
	}
	open, err := boolMember(m, "open")
	if err != nil {
		return false, fmt.Errorf("lan.json: %w", err)
	}
	return open, nil
}

func marshalLAN(gen string, open bool) ([]byte, error) {
	return marshalJSON(struct {
		V    int    `json:"v"`
		Gen  string `json:"gen"`
		Open bool   `json:"open"`
	}{1, gen, open})
}

// LANOpen reports the owner's switch. A locked store reports closed.
func (s *Store) LANOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockErr == nil && s.lanOpen
}

// LANErr is non-nil when lan.json could not be read on load (the switch then
// reads closed until the owner rewrites it).
func (s *Store) LANErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lanErr
}

// SetLANOpen persists the switch, with the commit and durability semantics
// of Update: memory follows a committed rename, and an unconfirmed fsync is
// ErrDurability.
func (s *Store) SetLANOpen(open bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockErr != nil {
		return s.lockErr
	}
	b, err := marshalLAN(s.id.gen, open)
	if err != nil {
		return err
	}
	committed, err := s.fs.WriteFileAtomic(filepath.Join(s.dir, lanFile), b, 0o600)
	if !committed {
		return err
	}
	s.lanOpen, s.lanErr = open, nil
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDurability, err)
	}
	return nil
}

// LANOpen reports whether the owner's switch admits LAN plaintext (§7.2).
// It means something only in StateEnrolled; Policy ignores it elsewhere.
func (a *Authority) LANOpen() bool { return a.store.LANOpen() }

// setLanControl runs the owner's switch command. The replay window is made
// durable first (step 6, like every other command), then the switch; a failed
// switch write is internal_error and the owner re-reads the status.
func (a *Authority) setLanControl(adm *Admission, c RequestClaims, window Window) (*Admission, error) {
	if err := a.store.Update(setWindow(adm.ctl, window)); err != nil {
		return a.persistFailure(adm)
	}
	m, err := strictObject(c.Req, []string{"open"}, nil)
	var open bool
	if err == nil {
		open, err = boolMember(m, "open")
	}
	if err != nil {
		return a.fail(adm, CodeCommandFailed, map[string]string{"error": "invalid_request"})
	}
	if err := a.store.SetLANOpen(open); err != nil {
		return a.persistFailure(adm)
	}
	return a.succeed(adm, map[string]bool{"open": open})
}
