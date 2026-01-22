# ───────────────────────────────────────────────────────────────────────────────
# Debugging commands
.PHONY: clear-state
clear-state:
	rm -rf ~/.state/setupd

.PHONY: debug
debug:
	@./scripts/debug.sh

.PHONY: restart-bluetooth
restart-bluetooth:
	sudo systemctl restart bluetooth

# ───────────────────────────────────────────────────────────────────────────────
# Docker build and run (on development machine, not on ff1 device)
# This is mainly for running cargo check and tests

.PHONY: docker-build
docker-build:
	@docker build -t arch-dev .

.PHONY: docker-run
docker-run:
	@docker run --rm -it \
		-v "$(CURDIR)":/workspace \
		arch-dev

.PHONY: docker-check
docker-check:
	@docker run --rm \
		-v "$(CURDIR)":/workspace \
		arch-dev sh -c '\
			set -e && \
			echo "==> cargo fmt -- --check" && \
			cargo fmt -- --check && \
			echo "==> cargo clippy --all-targets --all-features -- -D warnings" && \
			cargo clippy --all-targets --all-features -- -D warnings && \
			echo "==> cargo check --all-targets --all-features --verbose" && \
			cargo check --all-targets --all-features --verbose && \
			echo "==> All checks passed!"'
# ───────────────────────────────────────────────────────────────────────────────	