package control

import "github.com/feral-file/ffos-user/components/feral-controld/commands"

// Tier is the minimum authority a command needs (§7.1).
type Tier string

const (
	// TierController: any active grant (owner or controller).
	TierController Tier = "controller"
	// TierOwner: an active owner grant. Also the tier of every name not in
	// the table, so a command added without a decision fails closed for
	// controllers rather than open.
	TierOwner Tier = "owner"
	// TierInternal: never accepted from an external principal, whatever its
	// role. These names exist on the device's internal paths (controld →
	// player paint commands, router-internal resolution, the envelope
	// carrier itself), and some of them would fall through to the player
	// over CDP if a remote caller named them (commandrouter "Any other
	// non-device command is forwarded").
	TierInternal Tier = "internal"
)

// cmdDisplayPlaylist is the one command a display grant may run (§5.7).
const cmdDisplayPlaylist = string(commands.CMD_DISPLAY_PLAYLIST)

// Command names owned by this package (§5.5). They are handled by the
// Authority itself and never reach commandrouter.
const (
	CmdApproveController = "approveController"
	CmdRevokeController  = "revokeController"
	CmdListControllers   = "listControllers"
	// CmdRegisterController is owner-initiated registration (§5.6).
	CmdRegisterController = "registerController"
	// CmdReleaseOwnership revokes every grant, owners included, and leaves
	// the device legacy and claimed (§5.5, Forget I exist).
	CmdReleaseOwnership = "releaseOwnership"
	// CmdEnvelope is the carrier command name on LAN and the legacy relayer
	// (§10). An envelope whose inner cmd is itself ff1Envelope is refused.
	CmdEnvelope = "ff1Envelope"
)

// commandTiers is THE tier table (§7.1). It is the only place a command's
// tier is decided. tiers_test.go cross-checks it against every command
// constant in commands/types.go, every literal command name commandrouter
// special-cases, and the player's CastCommand list (ff-player
// src/models/cast_info.model.ts + CDPRequestHandler.ts), so a new command
// cannot ship untiered.
var commandTiers = map[string]Tier{
	// --- owner: device ownership, setup, remote shell, input injection ---
	string(commands.CMD_SSH_ACCESS):                 TierOwner,
	string(commands.CMD_FACTORY_RESET):              TierOwner,
	string(commands.CMD_START_WIFI_SETUP):           TierOwner,
	string(commands.CMD_UPDATE_TO_LATEST):           TierOwner,
	string(commands.CMD_SET_DEVICE_NAME):            TierOwner,
	string(commands.CMD_SHOW_PAIRING_QR_CODE):       TierOwner,
	string(commands.CMD_START_MINT_PAIRING_SESSION): TierOwner,
	string(commands.CMD_JOIN_MINT_PAIRING_CHANNEL):  TierOwner,
	string(commands.CMD_CLOSE_MINT_PAIRING_SESSION): TierOwner,
	string(commands.CMD_MINT_PAIRING_APPROVAL):      TierOwner,
	string(commands.CMD_SET_CONTENT_POLICY):         TierOwner,
	// setSeriesPlayback is the same category of owner-level device setting
	// as setContentPolicy (feral-file/ffos-user#425): it changes what plays
	// on the device, not merely what a controller is shown.
	string(commands.CMD_SET_SERIES_PLAYBACK):        TierOwner,
	string(commands.CMD_ANALYTICS_TOGGLE):           TierOwner,
	string(commands.CMD_BETA_FEATURES_TOGGLE):       TierOwner,
	string(commands.CMD_UPLOAD_LOGS):                TierOwner,
	string(commands.CMD_KEYBOARD_EVENT):             TierOwner,
	string(commands.CMD_MOUSE_TAP_EVENT):            TierOwner,
	string(commands.CMD_MOUSE_DOUBLE_TAP_EVENT):     TierOwner,
	string(commands.CMD_MOUSE_LONG_PRESS_EVENT):     TierOwner,
	string(commands.CMD_MOUSE_DRAG_EVENT):           TierOwner,
	string(commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT): TierOwner,
	string(commands.CMD_ZOOM_GESTURE):               TierOwner,
	CmdApproveController:                            TierOwner,
	// revokeController naming the signer's own ctl is the one form a
	// controller may send (self-revoke, §5.5); admitCmd checks it.
	CmdRevokeController:   TierOwner,
	CmdListControllers:    TierOwner,
	CmdRegisterController: TierOwner,
	CmdReleaseOwnership:   TierOwner,
	// Player CastCommand names that reach the player over the CDP
	// fallthrough. cursorUpdate is pointer injection like the gestures;
	// disconnect tears down the player's cast session.
	"cursorUpdate": TierOwner,
	"disconnect":   TierOwner,

	// --- controller: playback, display, power, read-only status ---
	// connect starts every ff-app cast. Only an owner claims or rebinds: the
	// executor answers a non-owner connect to a claimed device ok without
	// changing anything, and refuses one to an unclaimed device (§7.1).
	string(commands.CMD_CONNECT):                   TierController,
	string(commands.CMD_DISPLAY_PLAYLIST):          TierController,
	string(commands.CMD_DISPLAY_DEFAULT_PLAYLIST):  TierController,
	string(commands.CMD_REFRESH_ARTWORK):           TierController,
	string(commands.CMD_GET_RECENTLY_PLAYED):       TierController,
	string(commands.CMD_PLAY_RECENTLY_PLAYED):      TierController,
	string(commands.CMD_DOWNLOAD_PLAYLIST_ITEM):    TierController,
	string(commands.CMD_DOWNLOAD_PLAYLIST):         TierController,
	string(commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE): TierController,
	string(commands.CMD_CLEAR_PLAYLIST_CACHE):      TierController,
	string(commands.CMD_GET_OFFLINE_CACHE_STATUS):  TierController,
	string(commands.CMD_DEVICE_STATUS):             TierController,
	string(commands.CMD_SYS_METRICS):               TierController, // == CMD_PROFILE
	string(commands.CMD_SCREEN_ROTATION):           TierController,
	string(commands.CMD_SET_VOLUME):                TierController,
	string(commands.CMD_TOGGLE_MUTE):               TierController,
	string(commands.CMD_DDC_PANEL_CONTROL):         TierController,
	string(commands.CMD_DDC_PANEL_STATUS):          TierController,
	string(commands.CMD_SET_SLEEP_SCHEDULE):        TierController,
	string(commands.CMD_SET_SLEEP_MODE):            TierController,
	string(commands.CMD_SLEEP_NOW):                 TierController,
	string(commands.CMD_WAKE_NOW):                  TierController,
	string(commands.CMD_REBOOT):                    TierController,
	string(commands.CMD_SHUTDOWN):                  TierController,
	string(commands.CMD_RUN_NETWORK_DIAGNOSTICS):   TierController,
	string(commands.CMD_GET_CONTENT_POLICY):        TierController,
	string(commands.CMD_GET_SERIES_PLAYBACK):       TierController,
	// Player CastCommand names forwarded over CDP.
	"checkStatus":           TierController,
	"moveToArtwork":         TierController,
	"updateIndex":           TierController,
	"setShuffle":            TierController,
	"setLoop":               TierController,
	"updateArtFraming":      TierController,
	"updateDisplaySettings": TierController,
	"updateDefaultDuration": TierController,
	"refreshPlaylist":       TierController,

	// --- internal: never from outside ---
	string(commands.CMD_RESOLVE_RECENTLY_PLAYED): TierInternal,
	CmdEnvelope:          TierInternal,
	"ping":               TierInternal, // controld → player liveness probe
	"mintPairingDisplay": TierInternal, // controld → player mint-pairing paint
	"setupDisplay":       TierInternal, // controld → player setup narration paint
}

// TierOf returns cmd's tier. Unknown names are owner-only (§7.1).
func TierOf(cmd string) Tier {
	if t, ok := commandTiers[cmd]; ok {
		return t
	}
	return TierOwner
}

// Allows reports whether a principal with role may run a command of tier t.
func (t Tier) Allows(role Role) bool {
	switch t {
	case TierController:
		return role == RoleController || role == RoleOwner
	case TierOwner:
		return role == RoleOwner
	default:
		return false
	}
}
