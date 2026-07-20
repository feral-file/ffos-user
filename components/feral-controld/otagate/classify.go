package otagate

import "strings"

// updateErrorKind classifies an updater failure for the retry ladder.
// Ported from feral-setupd update_coordinator.rs UpdateErrorType.
type updateErrorKind int

const (
	// errTransient failures are worth retrying at the gate level (network,
	// download, service-start, lock contention).
	errTransient updateErrorKind = iota
	// errPermanent failures are not retryable (bad signature, corrupt image,
	// filesystem/disk failures, unknown).
	errPermanent
)

// classifyUpdaterMessage decides whether an updater error is transient or permanent.
//
// Ported verbatim (semantics, not structure) from feral-setupd
// update_coordinator.rs::classify_updater_message.
//
// FRAGILE CONTRACT: this relies on exact-string matching against error messages
// emitted by the ffos shell scripts (feral-updater.sh, feral-system-update.sh,
// feral-recovery-update.sh) and by the updater-spawn code below. Any change to a
// script error message must be mirrored here.
func classifyUpdaterMessage(message string) updateErrorKind {
	msg := strings.TrimSpace(message)

	// ---- PERMANENT: do not retry at the gate level ----

	// Cryptographic failures (server published a bad/corrupt image).
	if strings.HasPrefix(msg, "Error: Signature verification failed for") ||
		strings.HasPrefix(msg, "Error: Signature file ") {
		return errPermanent
	}

	// Image integrity failures (corrupt or invalid image structure).
	if msg == "airootfs.sfs not found in image." || msg == "No post-extraction script found in ISO" {
		return errPermanent
	}

	// Filesystem operation failures (disk full, permissions, etc.).
	if msg == "Failed to create snapshot '/.snapshots/@ota_prev'. Aborting." ||
		strings.HasPrefix(msg, "Failed to create snapshot") {
		return errPermanent
	}

	// Unknown/unclassified errors from the updater-spawn layer.
	if msg == "Unknown error occurred" {
		return errPermanent
	}

	// ---- TRANSIENT: worth retrying at the gate level ----

	// Network connectivity and download failures (no internet or server unreachable).
	if msg == "No network connection. Aborting update." ||
		msg == "Failed to retrieve content length for image." ||
		msg == "Failed to download OTA image." ||
		msg == "Failed to download signature file." ||
		msg == "Failed to download recovery ISO." {
		return errTransient
	}

	// Updater-spawn infrastructure failures (service start, log file access, or
	// the unit dying without an id-tagged terminal line — SIGKILL/OOM/stop; see
	// systemdRunner.tail's liveness watch). All retryable.
	if strings.Contains(msg, "Failed to start updater service") ||
		strings.Contains(msg, "Failed to open /var/log/updaterd.log") ||
		msg == "updater closed channel without sending progress" ||
		strings.Contains(msg, "updater service exited without reporting completion") {
		return errTransient
	}

	// Lock failures (another update process running or a stale lock). Transient
	// because the lock may be released by the time we retry.
	if strings.Contains(msg, "Lock already held by another instance") ||
		strings.Contains(msg, "either Lock already held") {
		return errTransient
	}

	// ---- BASH ERR TRAP: classify by the trapped command ----
	//
	// Format: EXCEPTION ERR: LINE=118 CMD="curl -u ... -o /var/tmp/ota/image.zip"
	// Network commands (curl, wget) are transient; anything else (mount, rsync,
	// btrfs, mkinitcpio, ...) is permanent.
	if strings.HasPrefix(msg, "EXCEPTION ERR:") {
		if strings.Contains(msg, "curl") || strings.Contains(msg, "wget") {
			return errTransient
		}
		return errPermanent
	}

	// ---- DEFAULT: unknown errors are permanent (avoid infinite retry loops) ----
	return errPermanent
}
