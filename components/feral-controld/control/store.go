package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
)

// Role is a grant's authority (§5.5, §7.1).
type Role string

const (
	RoleOwner      Role = "owner"
	RoleController Role = "controller"
)

func (r Role) valid() bool { return r == RoleOwner || r == RoleController }

// approvedByInvitation marks an owner grant minted by the first-owner
// invitation (§5.1) rather than by another owner's approveController.
const approvedByInvitation = "invitation"

var validPlatforms = []string{"ios", "android", "cli", "other"}

const (
	maxLabelRunes = 64
	// maxActiveGrants bounds the per-notification fan-out (one AES-GCM per
	// active grant, §9) and the approval UI.
	maxActiveGrants = 32
	// maxGrantRecords bounds grants.json. Revoked records are kept for their
	// replay window (§6), so this is larger than the active cap.
	maxGrantRecords = 128
)

var (
	// ErrLocked means the control directory exists but cannot be trusted
	// (§8): every envelope is refused until a human resets the device. It is
	// never resolved by silently starting over, which would turn corruption
	// into an unowned device.
	ErrLocked = errors.New("control: state locked")
	// ErrDurability means a write was renamed into place but the directory
	// fsync failed: the new content is what a restart reads, but a power cut
	// might still lose it. Callers must not report success on top of it.
	ErrDurability = errors.New("control: write not confirmed durable")
	errNoDir      = errors.New("control: state directory missing")
	// ErrRetired means Retire deleted the control directory (factory reset).
	// It wraps ErrLocked: a retired authority refuses everything until the
	// process restarts and, finding no directory, starts over as legacy.
	ErrRetired = fmt.Errorf("%w: control identity retired", ErrLocked)
)

// Grant is one controller record (§8). Revoked records are kept: their
// replay window must survive so a re-approval cannot replay old requests.
type Grant struct {
	Ctl        string
	Sig        PublicJWK
	Enc        PublicJWK
	Role       Role
	Label      string
	Platform   string
	CreatedAt  int64
	ApprovedBy string
	ExpiresAt  int64 // 0: no expiry
	RevokedAt  int64 // 0: not revoked
	NK         [32]byte
	Window     Window
}

// Active reports whether the grant currently confers authority.
func (g Grant) Active(now int64) bool {
	return g.RevokedAt == 0 && (g.ExpiresAt == 0 || now < g.ExpiresAt)
}

// Store is the durable grant store. All mutation goes through Update, which
// persists before it publishes, so memory never runs ahead of disk.
type Store struct {
	mu      sync.Mutex
	fs      FS
	dir     string
	id      *Identity
	grants  []Grant
	lockErr error
}

// InitDir provisions a fresh control directory holding id and grants. The
// files are written into dir+".new" and the directory is renamed into place,
// so a crash can never leave a half-created dir (identity without grants)
// that the next start would have to treat as corrupt. It refuses to replace
// an existing dir.
func InitDir(fsys FS, dir string, id *Identity, grants []Grant) error {
	if _, err := fsys.Stat(dir); err == nil {
		return fmt.Errorf("control: %s already exists", dir)
	} else if !isNotExist(err) {
		return err
	}
	staging := dir + ".new"
	if err := fsys.RemoveAll(staging); err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	if err := fsys.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	if err := fsys.Mkdir(staging, 0o700); err != nil {
		return err
	}
	idb, err := id.marshal()
	if err != nil {
		return err
	}
	if _, err := fsys.WriteFileAtomic(filepath.Join(staging, identityFile), idb, 0o600); err != nil {
		return err
	}
	gb, err := marshalGrants(id.gen, grants)
	if err != nil {
		return err
	}
	if _, err := fsys.WriteFileAtomic(filepath.Join(staging, grantsFile), gb, 0o600); err != nil {
		return err
	}
	if err := fsys.Rename(staging, dir); err != nil {
		return err
	}
	return fsys.SyncDir(parent)
}

// LoadStore loads an existing directory. A missing directory returns an
// error wrapping errNoDir; anything else wrong returns ErrLocked.
func LoadStore(fsys FS, dir, deviceID string) (*Store, error) {
	if _, err := fsys.Stat(RetiredMarker(dir)); err == nil {
		return nil, ErrRetired
	} else if !isNotExist(err) {
		// Unsure is locked, but not retired: the plaintext factoryReset
		// allowance is only for a real tombstone (review F5).
		return nil, fmt.Errorf("%w: reading the retirement tombstone: %w", ErrLocked, err)
	}
	fi, err := fsys.Stat(dir)
	if isNotExist(err) {
		return nil, errNoDir
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrLocked, dir)
	}
	idb, err := fsys.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	id, err := parseIdentity(idb, deviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	gb, err := fsys.ReadFile(filepath.Join(dir, grantsFile))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	grants, err := parseGrants(gb, id.gen)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	return &Store{fs: fsys, dir: dir, id: id, grants: grants}, nil
}

// OpenStore loads dir, creating a fresh identity when (and only when) the
// directory does not exist (§8). It never returns nil: a store that cannot
// be trusted is returned locked, with LockErr explaining why.
func OpenStore(fsys FS, dir, deviceID string, r io.Reader, now time.Time) *Store {
	s, err := LoadStore(fsys, dir, deviceID)
	if errors.Is(err, errNoDir) {
		id, gerr := newRandomIdentity(deviceID, r, now)
		if gerr == nil {
			gerr = InitDir(fsys, dir, id, nil)
		}
		if gerr != nil {
			// Creation failed (disk full, read-only fs). Locked, not legacy:
			// refusing envelopes is recoverable by a restart; pretending
			// to have an identity we could not persist is not.
			return &Store{fs: fsys, dir: dir, lockErr: fmt.Errorf("%w: creating identity: %w", ErrLocked, gerr)}
		}
		s, err = LoadStore(fsys, dir, deviceID)
	}
	if err != nil {
		return &Store{fs: fsys, dir: dir, lockErr: err}
	}
	return s
}

// LockErr is non-nil when the store is locked.
func (s *Store) LockErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockErr
}

// RetiredMarker is the tombstone Retire writes next to the control directory
// (dir + ".retired") before deleting it. While it exists the device is
// locked-retired whatever controld.json says: a reset that rolls back onto
// this subvolume after the directory was deleted must not come back as "no
// control state", which with control.enabled=false would mean open. A reset
// that completes boots a subvolume without it.
func RetiredMarker(dir string) string { return dir + ".retired" }

// StateExists reports whether dir holds control state: the directory, its
// staging dir, or the retirement tombstone. Anything Stat cannot rule out
// (an error other than not-exist) counts as present: the answer decides
// whether the protocol is forced on, and "unsure" must fail closed.
func StateExists(fsys FS, dir string) bool {
	for _, p := range []string{dir, dir + ".new", RetiredMarker(dir)} {
		if _, err := fsys.Stat(p); err == nil || !isNotExist(err) {
			return true
		}
	}
	return false
}

// Retire deletes the control directory (factory reset, §8) and locks the
// store so nothing is admitted or persisted afterwards. Order: lock in
// memory, write the durable tombstone, then delete. If the tombstone cannot
// be written nothing is deleted and the error is returned; the caller must
// not proceed with the reset. A partly failed delete also returns the error
// with the store already locked.
func (s *Store) Retire() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := filepath.Dir(s.dir)
	if err := s.fs.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	committed, err := s.fs.WriteFileAtomic(RetiredMarker(s.dir), []byte(`{"v":1}`), 0o600)
	if !committed {
		// No tombstone: nothing changes, in memory or on disk, and the
		// caller refuses the reset (review F5).
		return err
	}
	// The tombstone is what a restart reads from here on, so memory follows
	// it even if its directory fsync failed (reported below).
	s.lockErr = ErrRetired
	s.grants = nil
	if err != nil {
		return err
	}
	if err := s.fs.RemoveAll(s.dir); err != nil {
		return err
	}
	if err := s.fs.RemoveAll(s.dir + ".new"); err != nil {
		return err
	}
	return s.fs.SyncDir(filepath.Dir(s.dir))
}

// Identity returns the device identity (nil when locked).
func (s *Store) Identity() *Identity { return s.id }

// Grants returns a copy of every record, revoked ones included.
func (s *Store) Grants() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.grants)
}

// Grant looks up one record by controllerId.
func (s *Store) Grant(ctl string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := indexOf(s.grants, ctl); i >= 0 {
		return s.grants[i], true
	}
	return Grant{}, false
}

func indexOf(gs []Grant, ctl string) int {
	return slices.IndexFunc(gs, func(g Grant) bool { return g.Ctl == ctl })
}

// Update applies fn to a copy of the records and persists the result before
// publishing it. If fn returns an error nothing is written. If the write is
// not committed, memory keeps the old records; if it is committed but not
// confirmed durable, memory takes the new records (they are what a restart
// reads) and the error wraps ErrDurability.
func (s *Store) Update(fn func([]Grant) ([]Grant, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockErr != nil {
		return s.lockErr
	}
	next, err := fn(slices.Clone(s.grants))
	if err != nil {
		return err
	}
	if err := validateGrants(next); err != nil {
		return err
	}
	b, err := marshalGrants(s.id.gen, next)
	if err != nil {
		return err
	}
	committed, err := s.fs.WriteFileAtomic(filepath.Join(s.dir, grantsFile), b, 0o600)
	if !committed {
		return err
	}
	s.grants = next
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDurability, err)
	}
	return nil
}

type grantsJSON struct {
	V           int         `json:"v"`
	Gen         string      `json:"gen"`
	Controllers []grantJSON `json:"controllers"`
}

type grantJSON struct {
	Ctl        string    `json:"ctl"`
	Sig        PublicJWK `json:"sig"`
	Enc        PublicJWK `json:"enc"`
	Role       Role      `json:"role"`
	Label      string    `json:"label"`
	Platform   string    `json:"platform"`
	CreatedAt  int64     `json:"createdAt"`
	ApprovedBy string    `json:"approvedBy"`
	ExpiresAt  int64     `json:"expiresAt,omitempty"`
	RevokedAt  int64     `json:"revokedAt,omitempty"`
	NK         string    `json:"nk"`
	Hi         uint64    `json:"hi"`
	Bitmap     string    `json:"bitmap"`
}

var grantMembersRequired = []string{"ctl", "sig", "enc", "role", "label", "platform", "createdAt", "approvedBy", "nk", "hi", "bitmap"}
var grantMembersOptional = []string{"expiresAt", "revokedAt"}

func marshalGrants(gen string, gs []Grant) ([]byte, error) {
	out := grantsJSON{V: 1, Gen: gen, Controllers: make([]grantJSON, 0, len(gs))}
	for _, g := range gs {
		out.Controllers = append(out.Controllers, grantJSON{
			Ctl: g.Ctl, Sig: g.Sig, Enc: g.Enc, Role: g.Role, Label: g.Label, Platform: g.Platform,
			CreatedAt: g.CreatedAt, ApprovedBy: g.ApprovedBy, ExpiresAt: g.ExpiresAt, RevokedAt: g.RevokedAt,
			NK: b64u(g.NK[:]), Hi: g.Window.Hi, Bitmap: g.Window.bitmapHex(),
		})
	}
	return marshalJSON(out)
}

// parseGrants decodes grants.json strictly. Any deviation — a duplicate
// member, an unknown member, a record whose ctl is not thumb(sig), a gen that
// is not the identity's — is corruption, and the caller locks.
func parseGrants(raw []byte, gen string) ([]Grant, error) {
	m, err := strictObject(raw, []string{"v", "gen", "controllers"}, nil)
	if err != nil {
		return nil, fmt.Errorf("grants.json: %w", err)
	}
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return nil, errors.New("grants.json: unsupported version")
	}
	if err := expectString(m, "gen", gen); err != nil {
		return nil, fmt.Errorf("grants.json: generation mismatch: %w", err)
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(m["controllers"], &rawList); err != nil {
		return nil, fmt.Errorf("grants.json controllers: %w", err)
	}
	gs := make([]Grant, 0, len(rawList))
	for i, rc := range rawList {
		cm, err := strictObject(rc, grantMembersRequired, grantMembersOptional)
		if err != nil {
			return nil, fmt.Errorf("grants.json controller %d: %w", i, err)
		}
		for _, k := range []string{"createdAt", "hi", "expiresAt", "revokedAt"} {
			if _, ok := cm[k]; ok {
				if _, err := uintMember(cm, k); err != nil {
					return nil, fmt.Errorf("grants.json controller %d: %w", i, err)
				}
			}
		}
		var gj grantJSON
		if err := json.Unmarshal(rc, &gj); err != nil {
			return nil, fmt.Errorf("grants.json controller %d: %w", i, err)
		}
		g := Grant{Ctl: gj.Ctl, Sig: gj.Sig, Enc: gj.Enc, Role: gj.Role, Label: gj.Label, Platform: gj.Platform,
			CreatedAt: gj.CreatedAt, ApprovedBy: gj.ApprovedBy, ExpiresAt: gj.ExpiresAt, RevokedAt: gj.RevokedAt}
		nk, err := b64uDecode(gj.NK)
		if err != nil || len(nk) != len(g.NK) {
			return nil, fmt.Errorf("grants.json controller %d: bad nk", i)
		}
		copy(g.NK[:], nk)
		bm, err := parseBitmapHex(gj.Bitmap)
		if err != nil {
			return nil, fmt.Errorf("grants.json controller %d: %w", i, err)
		}
		g.Window = Window{Hi: gj.Hi, Bitmap: bm}
		gs = append(gs, g)
	}
	if err := validateGrants(gs); err != nil {
		return nil, fmt.Errorf("grants.json: %w", err)
	}
	return gs, nil
}

// validateGrants is the store invariant, checked on load and before every
// write so a logic bug cannot persist a file the next start would lock on.
func validateGrants(gs []Grant) error {
	if len(gs) > maxGrantRecords {
		return fmt.Errorf("%d records exceeds %d", len(gs), maxGrantRecords)
	}
	seen := map[string]bool{}
	for _, g := range gs {
		if g.Ctl != g.Sig.Thumbprint() {
			return fmt.Errorf("record %s: ctl is not thumb(sig)", g.Ctl)
		}
		if seen[g.Ctl] {
			return fmt.Errorf("record %s: duplicate", g.Ctl)
		}
		seen[g.Ctl] = true
		if !g.Role.valid() {
			return fmt.Errorf("record %s: role %q", g.Ctl, g.Role)
		}
		if !validLabel(g.Label) || !slices.Contains(validPlatforms, g.Platform) {
			return fmt.Errorf("record %s: label/platform", g.Ctl)
		}
		if g.ApprovedBy != approvedByInvitation && !isThumbprint(g.ApprovedBy) {
			return fmt.Errorf("record %s: approvedBy", g.Ctl)
		}
		if !g.Window.valid() {
			return fmt.Errorf("record %s: inconsistent replay window", g.Ctl)
		}
		if g.CreatedAt < 0 || g.ExpiresAt < 0 || g.RevokedAt < 0 {
			return fmt.Errorf("record %s: negative time", g.Ctl)
		}
	}
	return nil
}

func validLabel(s string) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= 1 && n <= maxLabelRunes
}
