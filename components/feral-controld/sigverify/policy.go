package sigverify

// Player-toast policy (feral-file/ffos-user#307). What a verdict does on the
// wall is a function of the owner's Mode and the cast's Status only — the two
// live here so devicectl, commandrouter and the refresher read one table
// rather than each re-deriving it. The notices are the closed set ff-player's
// playerToast contract lists; playertoast sends them, copy is owned by the
// player.

// Notice is a player-toast identifier. Its values are the states in
// ff-player's `contracts.playerToast` manifest entry; controld must send only
// these (playertoast validates the notice against the live manifest before
// sending).
type Notice string

const (
	// NoticeInvalid: signatures were present but at least one failed.
	NoticeInvalid Notice = "signature_invalid"
	// NoticeUnsigned: the document carried no signatures.
	NoticeUnsigned Notice = "signature_unsigned"
	// NoticeRejected: strict mode refused the cast (it is not on screen).
	NoticeRejected Notice = "signature_rejected"
)

// ToastFor returns the player-toast notice for a (mode, status) pair and
// whether one should be shown. It is the single policy table:
//
//	silent → never toasts.
//	notify → the cast still plays; a non-valid verdict is surfaced —
//	         invalid → NoticeInvalid, unsigned → NoticeUnsigned. A valid or
//	         verdict-less (status "") cast toasts nothing: notify only names a
//	         verdict it actually has.
//	strict → the cast is refused, so ANY non-valid status (invalid, unsigned,
//	         or the verdict-less cached copy, status "") toasts NoticeRejected;
//	         a valid cast plays and toasts nothing.
//
// valid never toasts in any mode.
func ToastFor(mode Mode, status Status) (Notice, bool) {
	switch mode {
	case ModeStrict:
		if status == StatusValid {
			return "", false
		}
		return NoticeRejected, true
	case ModeNotify:
		switch status {
		case StatusInvalid:
			return NoticeInvalid, true
		case StatusUnsigned:
			return NoticeUnsigned, true
		default:
			return "", false
		}
	default: // ModeSilent and any unknown mode: never toast.
		return "", false
	}
}
