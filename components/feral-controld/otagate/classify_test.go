package otagate

import "testing"

// These table tests are a 1:1 port of the feral-setupd update_coordinator.rs
// classify_updater_message tests. The Go test-name suffix in parentheses names
// the Rust test it mirrors.
func TestClassifyUpdaterMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    updateErrorKind
	}{
		// classify_updater_message_marks_network_failures_transient
		{"network/no-connection (marks_network_failures_transient)", "No network connection. Aborting update.", errTransient},
		{"network/content-length (marks_network_failures_transient)", "Failed to retrieve content length for image.", errTransient},
		{"network/download-ota (marks_network_failures_transient)", "Failed to download OTA image.", errTransient},
		{"network/download-sig (marks_network_failures_transient)", "Failed to download signature file.", errTransient},
		{"network/download-iso (marks_network_failures_transient)", "Failed to download recovery ISO.", errTransient},

		// classify_updater_message_marks_setupd_infrastructure_failures_transient
		{"infra/start-service (marks_setupd_infrastructure_failures_transient)", "Failed to start updater service: systemd error", errTransient},
		{"infra/open-log (marks_setupd_infrastructure_failures_transient)", "Failed to open /var/log/updaterd.log after retries", errTransient},
		{"infra/closed-channel (marks_setupd_infrastructure_failures_transient)", "updater closed channel without sending progress", errTransient},

		// classify_updater_message_marks_lock_failures_transient
		{"lock/either-held (marks_lock_failures_transient)", "Exception: either Lock already held by another instance or some error happened.", errTransient},

		// classify_updater_message_marks_signing_and_image_failures_permanent
		{"perm/signature-verify (marks_signing_and_image_failures_permanent)", "Error: Signature verification failed for /var/tmp/ota/image.iso.", errPermanent},
		{"perm/airootfs (marks_signing_and_image_failures_permanent)", "airootfs.sfs not found in image.", errPermanent},
		{"perm/snapshot (marks_signing_and_image_failures_permanent)", "Failed to create snapshot '/.snapshots/@ota_prev'. Aborting.", errPermanent},
		{"perm/unknown-error (marks_signing_and_image_failures_permanent)", "Unknown error occurred", errPermanent},

		// classify_updater_message_marks_unrecognized_as_permanent
		{"perm/unrecognized (marks_unrecognized_as_permanent)", "something unexpected", errPermanent},

		// classify_updater_message_classifies_exception_err_by_command
		{"errtrap/curl-image (classifies_exception_err_by_command)", `EXCEPTION ERR: LINE=118 CMD="curl -u "$auth_user:$auth_pass" --silent --show-error -fL "$ENDPOINT$IMAGE_URL" -o "$ZIP_FILE""`, errTransient},
		{"errtrap/curl-api (classifies_exception_err_by_command)", `EXCEPTION ERR: LINE=47 CMD="curl -su "$auth_user:$auth_pass" -f "$API_URL""`, errTransient},
		{"errtrap/wget (classifies_exception_err_by_command)", `EXCEPTION ERR: LINE=200 CMD="wget --timeout=10 https://example.com/file.tar.gz"`, errTransient},
		{"errtrap/mount (classifies_exception_err_by_command)", `EXCEPTION ERR: LINE=99 CMD="mount -o loop /var/tmp/ota/image.iso /mnt/ota-iso"`, errPermanent},
		{"errtrap/rsync (classifies_exception_err_by_command)", `EXCEPTION ERR: LINE=145 CMD="rsync -aAX --delete /mnt/ota-sfs/ /"`, errPermanent},
		{"errtrap/mkinitcpio (classifies_exception_err_by_command)", `EXCEPTION ERR: LINE=208 CMD="mkinitcpio -P"`, errPermanent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyUpdaterMessage(tc.message); got != tc.want {
				t.Fatalf("classifyUpdaterMessage(%q) = %v, want %v", tc.message, got, tc.want)
			}
		})
	}
}

// Ported from update_attempt_error_classifies_from_raw_updater_message_before_context_wrap:
// the raw updater message classifies transient, but once it is wrapped with extra
// context ("update process failed: ...") the string no longer matches and falls
// through to permanent. This is why the gate classifies the RAW runner error.
func TestClassifyUpdaterMessage_RawBeforeContextWrap(t *testing.T) {
	raw := "No network connection. Aborting update."
	if got := classifyUpdaterMessage(raw); got != errTransient {
		t.Fatalf("raw message should be transient, got %v", got)
	}
	wrapped := "update process failed: " + raw
	if got := classifyUpdaterMessage(wrapped); got != errPermanent {
		t.Fatalf("context-wrapped message should fall through to permanent, got %v", got)
	}
}
