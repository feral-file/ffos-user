.PHONY: sync
sync:
	@echo "📡 Syncing to X4..."
	@./scripts/sync-to-x4.sh . /home/feralfile/src/components/$(notdir $(PWD))

.PHONY: sync-connectd
sync-connectd:
	@echo "📡 Syncing connectd..."
	@./scripts/sync-to-x4.sh ../feral-connectd /home/feralfile/src/components/feral-connectd

.PHONY: sync-sys-monitord
sync-sys-monitord:
	@echo "📡 Syncing sys-monitord..."
	@./scripts/sync-to-x4.sh ../feral-sys-monitord /home/feralfile/src/components/feral-sys-monitord

.PHONY: sync-launcher-ui
sync-launcher-ui:
	@echo "📡 Syncing launcher-ui..."
	@./scripts/sync-to-x4.sh ../launcher-ui /opt/feral/ui/launcher/

.PHONY: fresh-debug
fresh-debug:
	@echo "🔍 Running fresh debug build..."
	@./scripts/fresh-debug.sh

.PHONY: resume-debug
resume-debug:
	@echo "🔍 Running resume debug build..."
	@./scripts/resume-debug.sh

.PHONY: fresh-release
fresh-release:
	@echo "🔍 Running fresh release build..."
	@./scripts/fresh-release.sh

.PHONY: resume-release
resume-release:
	@echo "🔍 Running resume release build..."
	@./scripts/resume-release.sh

.PHONY: restart-bluetooth
restart-bluetooth:
	@echo "🔄 Restarting bluetooth service..."
	@./scripts/restart-bluetooth.sh

# ───────────────────────────────────────────────────────────────────────────────
# Paths
ROOT             := /home/feralfile
CONNECTD_DIR     := $(ROOT)/feral-connectd
SETUPD_DIR       := $(ROOT)/feral-setupd
MONITORD_DIR     := $(ROOT)/feral-sys-monitord
LOGDIR           := $(CURDIR)/logs

# ───────────────────────────────────────────────────────────────────────────────
create-log-dir:
	@mkdir -p $(LOGDIR)

start-silent-monitord: create-log-dir
	@echo "Building & starting feral-sys-monitord…"
	@cd $(MONITORD_DIR) && go build
	@nohup $(MONITORD_DIR)/feral-sys-monitord \
	    >>$(LOGDIR)/feral-sys-monitord.log 2>&1 & \
	    echo "  ↳ PID $$!"

start-silent-connectd: create-log-dir
	@echo "Building & starting feral-connectd…"
	@cd $(CONNECTD_DIR) && go build
	@nohup $(CONNECTD_DIR)/feral-connectd \
	    >>$(LOGDIR)/feral-connectd.log 2>&1 & \
	    echo "  ↳ PID $$!"

start-silent-setupd: create-log-dir
	@echo "Building & starting feral-setupd…"
	@cd $(SETUPD_DIR) && cargo build --quiet
	@nohup $(SETUPD_DIR)/target/debug/feral-setupd \
	    >>$(LOGDIR)/feral-setupd.log 2>&1 & \
	    echo "  ↳ PID $$!"

start-silent-all: create-log-dir
	make start-silent-monitord
	make start-silent-connectd
	@sleep 2
	make start-silent-setupd

stop-silent-all:
	@echo "Stopping all feral-* processes…"
	@pkill -f feral- || true
	@echo "Done."

.PHONY: start-silent-monitord start-silent-connectd start-silent-setupd
.PHONY: start-silent-all stop-silent-all


# ───────────────────────────────────────────────────────────────────────────────
# Docker

.PHONY: docker-build
docker-build:
	@docker build -t arch-dev .

.PHONY: docker-run
docker-run:
	@docker run --rm -it \
		-v "$(CURDIR)":/workspace \
		arch-dev

# ───────────────────────────────────────────────────────────────────────────────