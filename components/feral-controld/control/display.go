package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Display grants (§5.7): what a Play site holds after an owner approved it
// through mint pairing. They live in their own file, display_grants.json,
// never in grants.json: an older controld ignores the file, so a rollback
// can never lock a device on a role it does not know, and a load failure
// here disables display admission only (never the device, §3.1 of the plan).

// RoleDisplay is the display-store-only role. It is never wire-assignable:
// Role.valid, and so every role parser, keeps accepting owner|controller.
const RoleDisplay Role = "display"

// platformWeb is the display-store-only platform; validPlatforms is unchanged.
const platformWeb = "web"

const (
	displayGrantsFile = "display_grants.json"
	// maxActiveDisplayGrants and maxDisplayRecords are the display store's own
	// caps (Q1): separate from maxActiveGrants, which bounds the sealed
	// fan-out display peers never take part in.
	maxActiveDisplayGrants = 16
	maxDisplayRecords      = 64
	maxOriginBytes         = 2048
	// displayLabelFallback is the label when nothing usable can be read from
	// the origin.
	displayLabelFallback = "web"
)

var (
	// ErrDisplayDisabled means display_grants.json could not be trusted on
	// load: display admission and issuance are off until it is fixed or
	// removed. The device itself is not locked.
	ErrDisplayDisabled = errors.New("control: display grants disabled")
	// ErrDisplayCollision refuses a ctl that grants.json or the display store
	// already holds. Pairing keys are fresh, so a repeat is never legitimate.
	ErrDisplayCollision = errors.New("control: controller id already held")
	// ErrTooManyDisplayGrants is the active cap (too_many_sites to the site).
	ErrTooManyDisplayGrants = errors.New("control: too many display grants")
	// ErrNotOwner: the ctl is not an active owner (SealOwner, and the
	// approver of a display grant outside legacy).
	ErrNotOwner = errors.New("control: not an active owner")
	// ErrDisplayNotFound: RevokeDisplayGrant on an unknown ctl.
	ErrDisplayNotFound = errors.New("control: no such display grant")
)

// DisplayGrant is one display record. Revoked records are kept for their
// replay window, as grant records are (§6).
type DisplayGrant struct {
	Ctl            string
	Sig            PublicJWK
	Enc            PublicJWK
	Origin         string
	Label          string
	OriginAttested bool
	CreatedAt      int64
	ApprovedBy     string // the approving owner's ctl, or "" in legacy
	ExpiresAt      int64  // 0: kept until removed
	RevokedAt      int64
	Window         Window
}

// Active reports whether the grant currently confers authority.
func (g DisplayGrant) Active(now int64) bool {
	return g.RevokedAt == 0 && (g.ExpiresAt == 0 || now < g.ExpiresAt)
}

func (g DisplayGrant) inactiveSince(now int64) (int64, bool) {
	return inactiveSince(g.Active(now), g.RevokedAt, g.ExpiresAt)
}

// DisplayGrantRequest is what mint pairing hands IssueDisplayGrant.
type DisplayGrantRequest struct {
	Sig, Enc PublicJWK
	// Origin is the site's full origin; OriginAttested is true when the
	// broker attested it (a site-created channel).
	Origin         string
	OriginAttested bool
	ExpiresAt      int64 // 0: kept
}

// IssueDisplayGrant adds a display record for req (§5.7). approver is the
// owner's ctl from the approval decision, or "" when the device was in
// legacy at decision time; it is re-checked here, under a.mu, because a first
// owner can enroll between the decision and the issuance. Issuing revokes
// any other active grant for the same origin with the same attestation. On
// ErrDurability the record is committed but unconfirmed: the caller revokes
// it best-effort and rejects the site.
func (a *Authority) IssueDisplayGrant(approver string, req DisplayGrantRequest) (DisplayGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.LockErr(); err != nil {
		return DisplayGrant{}, err
	}
	if err := a.store.DisplayErr(); err != nil {
		return DisplayGrant{}, err
	}
	now := a.now().Unix()
	if approver == "" {
		if a.stateLocked(now) != StateLegacy {
			return DisplayGrant{}, ErrNotOwner
		}
	} else if g, ok := a.store.Grant(approver); !ok || !g.Active(now) || g.Role != RoleOwner {
		return DisplayGrant{}, ErrNotOwner
	}
	ctl := req.Sig.Thumbprint()
	if _, ok := a.store.Grant(ctl); ok {
		return DisplayGrant{}, ErrDisplayCollision
	}
	if !validOrigin(req.Origin) || req.ExpiresAt < 0 {
		return DisplayGrant{}, errors.New("control: invalid display grant request")
	}
	g := DisplayGrant{Ctl: ctl, Sig: req.Sig, Enc: req.Enc, Origin: req.Origin, Label: displayLabel(req.Origin),
		OriginAttested: req.OriginAttested, CreatedAt: now, ApprovedBy: approver, ExpiresAt: req.ExpiresAt}
	err := a.store.UpdateDisplay(func(gs []DisplayGrant) ([]DisplayGrant, error) {
		if indexOfDisplay(gs, ctl) >= 0 {
			return nil, ErrDisplayCollision
		}
		// One active grant per origin: a re-pair replaces the site's earlier
		// grant. An asserted origin never evicts an attested one.
		for i := range gs {
			if gs[i].Active(now) && gs[i].Origin == req.Origin && gs[i].OriginAttested == req.OriginAttested {
				gs[i].RevokedAt = now
			}
		}
		if activeDisplay(gs, now) >= maxActiveDisplayGrants {
			return nil, ErrTooManyDisplayGrants
		}
		if len(gs) >= maxDisplayRecords {
			j := evictableIn(gs, now, DisplayGrant.inactiveSince)
			if j < 0 {
				return nil, ErrTooManyDisplayGrants
			}
			gs = append(gs[:j], gs[j+1:]...)
		}
		return append(gs, g), nil
	})
	return g, err
}

// RevokeDisplayGrant revokes ctl's display record (mint pairing's abandon
// paths, §3.4 step 7). Already revoked is not an error.
func (a *Authority) RevokeDisplayGrant(ctl string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	return a.store.UpdateDisplay(revokeDisplay(ctl, now))
}

// DisplayGrants returns a copy of every display record.
func (a *Authority) DisplayGrants() []DisplayGrant { return a.store.DisplayGrants() }

func revokeDisplay(ctl string, now int64) func([]DisplayGrant) ([]DisplayGrant, error) {
	return func(gs []DisplayGrant) ([]DisplayGrant, error) {
		i := indexOfDisplay(gs, ctl)
		if i < 0 {
			return nil, ErrDisplayNotFound
		}
		if gs[i].RevokedAt == 0 {
			gs[i].RevokedAt = now
		}
		return gs, nil
	}
}

// setDisplayWindow is the display-store counterpart of setWindow.
func setDisplayWindow(ctl string, w Window) func([]DisplayGrant) ([]DisplayGrant, error) {
	return func(gs []DisplayGrant) ([]DisplayGrant, error) {
		i := indexOfDisplay(gs, ctl)
		if i < 0 {
			return nil, errors.New("control: display grant vanished")
		}
		gs[i].Window = w
		return gs, nil
	}
}

// refuseDisplay is refuse for a display record: the ctr is spent in the
// display store before the denial is answered.
func (a *Authority) refuseDisplay(adm *Admission, ctl string, window Window, code string) (*Admission, error) {
	if err := a.store.UpdateDisplay(setDisplayWindow(ctl, window)); err != nil && !errors.Is(err, ErrDurability) {
		return a.persistFailure(adm)
	}
	return a.fail(adm, code, nil)
}

// admitDisplay is §3.4 steps 5-6 for a display record, in every state but
// locked (OpenFrom answered that already): legacy included, because a
// legacy device issues display grants too (§5.7). A display ctl may send
// exactly two things, the Play cast (Permits) and its own self-revoke. An
// inactive record is access_revoked; for an active one every other op or
// command is permission_denied. Refusals spend the ctr.
func (a *Authority) admitDisplay(pre authenticated, g DisplayGrant, now int64) (*Admission, error) {
	adm, c := pre.adm, pre.claims
	adm.SignerRole = RoleDisplay
	if err := g.Window.Check(c.Ctr); err != nil {
		return a.fail(adm, CodeReplay, nil)
	}
	window := g.Window.Accept(c.Ctr)
	// Precedence: a future iat, then an inactive record (always
	// access_revoked, whatever it asked for), then what it asked for.
	switch {
	case pre.futureIat:
		return a.refuseDisplay(adm, g.Ctl, window, CodeInvalidRequest)
	case !g.Active(now):
		return a.refuseDisplay(adm, g.Ctl, window, CodeAccessRevoked)
	case c.Op != OpCmd:
		return a.refuseDisplay(adm, g.Ctl, window, CodePermissionDenied)
	}
	p := Principal{ControllerID: g.Ctl, Role: RoleDisplay}
	if isSelfRevoke(c, g.Ctl) {
		err := a.store.UpdateDisplay(func(gs []DisplayGrant) ([]DisplayGrant, error) {
			gs, err := setDisplayWindow(g.Ctl, window)(gs)
			if err != nil {
				return nil, err
			}
			return revokeDisplay(g.Ctl, now)(gs)
		})
		if err != nil && !errors.Is(err, ErrDurability) {
			return a.persistFailure(adm)
		}
		adm.Command, adm.RevokedCtls = c.Cmd, append(adm.RevokedCtls, g.Ctl)
		if err != nil {
			return a.persistFailure(adm)
		}
		return a.succeed(adm, struct{}{})
	}
	if !Permits(p, c.Cmd, c.Req) {
		return a.refuseDisplay(adm, g.Ctl, window, CodePermissionDenied)
	}
	adm.Command, adm.Request, adm.Principal = c.Cmd, c.Req, p
	if err := a.store.UpdateDisplay(setDisplayWindow(g.Ctl, window)); err != nil {
		return a.persistFailure(adm)
	}
	return adm, nil
}

// Permits reports whether p may run cmd with req (§7.1). A display grant is
// confined to the Play cast (§5.7, Q2): displayPlaylist in the exact Play
// shape, nothing else, not even a status read. Every other role is decided
// by the tier table. admitCmd and commandrouter's re-check both use it.
func Permits(p Principal, cmd string, req json.RawMessage) bool {
	if p.Role == RoleDisplay {
		return cmd == cmdDisplayPlaylist && isPlayCastShape(req)
	}
	return TierOf(cmd).Allows(p.Role)
}

// displayView is a display record in listControllers (§5.5).
type displayView struct {
	Ctl            string `json:"ctl"`
	Role           Role   `json:"role"`
	Label          string `json:"label"`
	Origin         string `json:"origin"`
	OriginAttested bool   `json:"originAttested"`
	Platform       string `json:"platform"`
	CreatedAt      int64  `json:"createdAt"`
	ExpiresAt      int64  `json:"expiresAt,omitempty"`
	RevokedAt      int64  `json:"revokedAt,omitempty"`
}

func indexOfDisplay(gs []DisplayGrant, ctl string) int {
	return slices.IndexFunc(gs, func(g DisplayGrant) bool { return g.Ctl == ctl })
}

func activeDisplay(gs []DisplayGrant, now int64) int {
	n := 0
	for _, g := range gs {
		if g.Active(now) {
			n++
		}
	}
	return n
}

// validOrigin is the stored-origin rule: non-empty, at most 2 KiB of valid
// UTF-8 with none of the label's control or formatting characters.
func validOrigin(s string) bool {
	return s != "" && len(s) <= maxOriginBytes && utf8.ValidString(s) && !unsafeLabelCharacters.MatchString(s)
}

// displayLabel derives the record's label from the origin: its host,
// punycoded, cut to the label length. It always returns a valid label.
func displayLabel(origin string) string {
	host := origin
	if u, err := url.Parse(origin); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	if ascii, err := idna.Lookup.ToASCII(host); err == nil && ascii != "" {
		host = ascii
	}
	if n := utf8.RuneCountInString(host); n > maxLabelRunes {
		host = string([]rune(host)[:maxLabelRunes])
	}
	if !validLabel(host) {
		return displayLabelFallback
	}
	return host
}

// --- store ---

// DisplayErr is non-nil when display_grants.json could not be trusted on
// load; the store then admits and issues no display grant (wrapped
// ErrDisplayDisabled). It never locks the device.
func (s *Store) DisplayErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.displayErr
}

// DisplayGrants returns a copy of every display record.
func (s *Store) DisplayGrants() []DisplayGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.display)
}

// DisplayGrant looks up one display record by ctl.
func (s *Store) DisplayGrant(ctl string) (DisplayGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := indexOfDisplay(s.display, ctl); i >= 0 {
		return s.display[i], true
	}
	return DisplayGrant{}, false
}

// UpdateDisplay is Update for display_grants.json, with the same commit and
// durability semantics.
func (s *Store) UpdateDisplay(fn func([]DisplayGrant) ([]DisplayGrant, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockErr != nil {
		return s.lockErr
	}
	if s.displayErr != nil {
		return s.displayErr
	}
	next, err := fn(slices.Clone(s.display))
	if err != nil {
		return err
	}
	if err := validateDisplayGrants(next); err != nil {
		return err
	}
	b, err := marshalDisplayGrants(s.id.gen, next)
	if err != nil {
		return err
	}
	committed, err := s.fs.WriteFileAtomic(filepath.Join(s.dir, displayGrantsFile), b, 0o600)
	if !committed {
		return err
	}
	s.display = next
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDurability, err)
	}
	return nil
}

// loadDisplayGrants reads display_grants.json. A missing file, or one
// written under another gen (records bound to a dead identity; the next
// write overwrites it), is an empty store; anything else wrong disables
// display grants (ErrDisplayDisabled) without locking, until the file is
// deleted or the device is reset.
func loadDisplayGrants(fsys FS, dir, gen string) ([]DisplayGrant, error) {
	raw, err := fsys.ReadFile(filepath.Join(dir, displayGrantsFile))
	if isNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDisplayDisabled, err)
	}
	gs, err := parseDisplayGrants(raw, gen)
	if errors.Is(err, errDisplayGenMismatch) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDisplayDisabled, err)
	}
	return gs, nil
}

var errDisplayGenMismatch = errors.New("display_grants.json: generation mismatch")

type displayGrantsJSON struct {
	V      int                `json:"v"`
	Gen    string             `json:"gen"`
	Grants []displayGrantJSON `json:"grants"`
}

type displayGrantJSON struct {
	Ctl            string    `json:"ctl"`
	Sig            PublicJWK `json:"sig"`
	Enc            PublicJWK `json:"enc"`
	Origin         string    `json:"origin"`
	Label          string    `json:"label"`
	Platform       string    `json:"platform"`
	OriginAttested bool      `json:"originAttested"`
	CreatedAt      int64     `json:"createdAt"`
	ApprovedBy     string    `json:"approvedBy"`
	ExpiresAt      int64     `json:"expiresAt,omitempty"`
	RevokedAt      int64     `json:"revokedAt,omitempty"`
	Hi             uint64    `json:"hi"`
	Bitmap         string    `json:"bitmap"`
}

var displayMembersRequired = []string{"ctl", "sig", "enc", "origin", "label", "platform", "originAttested", "createdAt", "approvedBy", "hi", "bitmap"}

func marshalDisplayGrants(gen string, gs []DisplayGrant) ([]byte, error) {
	out := displayGrantsJSON{V: 1, Gen: gen, Grants: make([]displayGrantJSON, 0, len(gs))}
	for _, g := range gs {
		out.Grants = append(out.Grants, displayGrantJSON{
			Ctl: g.Ctl, Sig: g.Sig, Enc: g.Enc, Origin: g.Origin, Label: g.Label, Platform: platformWeb,
			OriginAttested: g.OriginAttested, CreatedAt: g.CreatedAt, ApprovedBy: g.ApprovedBy,
			ExpiresAt: g.ExpiresAt, RevokedAt: g.RevokedAt, Hi: g.Window.Hi, Bitmap: g.Window.bitmapHex(),
		})
	}
	return marshalJSON(out)
}

// parseDisplayGrants decodes display_grants.json as strictly as parseGrants
// decodes grants.json; a gen that is not the identity's is discarded.
func parseDisplayGrants(raw []byte, gen string) ([]DisplayGrant, error) {
	m, err := strictObject(raw, []string{"v", "gen", "grants"}, nil)
	if err != nil {
		return nil, fmt.Errorf("display_grants.json: %w", err)
	}
	if v, err := uintMember(m, "v"); err != nil || v != 1 {
		return nil, errors.New("display_grants.json: unsupported version")
	}
	if err := expectString(m, "gen", gen); err != nil {
		return nil, fmt.Errorf("%w: %w", errDisplayGenMismatch, err)
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(m["grants"], &rawList); err != nil {
		return nil, fmt.Errorf("display_grants.json grants: %w", err)
	}
	gs := make([]DisplayGrant, 0, len(rawList))
	for i, rc := range rawList {
		cm, err := strictObject(rc, displayMembersRequired, grantMembersOptional)
		if err != nil {
			return nil, fmt.Errorf("display_grants.json grant %d: %w", i, err)
		}
		for _, k := range []string{"createdAt", "hi", "expiresAt", "revokedAt"} {
			if _, ok := cm[k]; ok {
				if _, err := uintMember(cm, k); err != nil {
					return nil, fmt.Errorf("display_grants.json grant %d: %w", i, err)
				}
			}
		}
		if _, err := boolMember(cm, "originAttested"); err != nil {
			return nil, fmt.Errorf("display_grants.json grant %d: %w", i, err)
		}
		var gj displayGrantJSON
		if err := json.Unmarshal(rc, &gj); err != nil {
			return nil, fmt.Errorf("display_grants.json grant %d: %w", i, err)
		}
		if gj.Platform != platformWeb {
			return nil, fmt.Errorf("display_grants.json grant %d: platform", i)
		}
		bm, err := parseBitmapHex(gj.Bitmap)
		if err != nil {
			return nil, fmt.Errorf("display_grants.json grant %d: %w", i, err)
		}
		gs = append(gs, DisplayGrant{Ctl: gj.Ctl, Sig: gj.Sig, Enc: gj.Enc, Origin: gj.Origin, Label: gj.Label,
			OriginAttested: gj.OriginAttested, CreatedAt: gj.CreatedAt, ApprovedBy: gj.ApprovedBy,
			ExpiresAt: gj.ExpiresAt, RevokedAt: gj.RevokedAt, Window: Window{Hi: gj.Hi, Bitmap: bm}})
	}
	if err := validateDisplayGrants(gs); err != nil {
		return nil, fmt.Errorf("display_grants.json: %w", err)
	}
	return gs, nil
}

// validateDisplayGrants is the display store invariant, checked on load and
// before every write.
func validateDisplayGrants(gs []DisplayGrant) error {
	if len(gs) > maxDisplayRecords {
		return fmt.Errorf("%d records exceeds %d", len(gs), maxDisplayRecords)
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
		if !validLabel(g.Label) || !validOrigin(g.Origin) {
			return fmt.Errorf("record %s: label/origin", g.Ctl)
		}
		if g.ApprovedBy != "" && !isThumbprint(g.ApprovedBy) {
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
