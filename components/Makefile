# Default REMOTE_HOST if not provided
REMOTE_HOST ?= FF1-WPBJ9VTQ.local
REMOTE_DIR ?= /home/feralfile/src/components
SYNC_SCRIPT := ./scripts/sync-to-device.sh

.PHONY: sync-feral-setupd
sync-feral-setupd:
	@$(SYNC_SCRIPT) ./feral-setupd $(REMOTE_DIR)/feral-setupd $(REMOTE_HOST)

.PHONY: sync-feral-controld
sync-feral-controld:
	@$(SYNC_SCRIPT) ./feral-controld $(REMOTE_DIR)/feral-controld $(REMOTE_HOST)

.PHONY: sync-feral-sys-monitord
sync-feral-sys-monitord:
	@$(SYNC_SCRIPT) ./feral-sys-monitord $(REMOTE_DIR)/feral-sys-monitord $(REMOTE_HOST)

.PHONY: sync-launcher-ui
sync-launcher-ui:
	@$(SYNC_SCRIPT) ./launcher-ui /opt/feral/ui/launcher/ $(REMOTE_HOST)

.PHONY: sync-feral-watchdog
sync-feral-watchdog:
	@$(SYNC_SCRIPT) ./feral-watchdog $(REMOTE_DIR)/feral-watchdog $(REMOTE_HOST)


.PHONY: sync-all
sync-all:
	@$(SYNC_SCRIPT) . $(REMOTE_DIR)/ $(REMOTE_HOST)