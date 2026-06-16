.PHONY: verify
verify: verify-go verify-setupd

.PHONY: verify-go
verify-go: \
	verify-feral-controld \
	verify-feral-sys-monitord \
	verify-feral-watchdog

.PHONY: verify-feral-controld
verify-feral-controld: verify-feral-controld-lint verify-feral-controld-test

.PHONY: verify-feral-sys-monitord
verify-feral-sys-monitord: verify-feral-sys-monitord-lint verify-feral-sys-monitord-test

.PHONY: verify-feral-watchdog
verify-feral-watchdog: verify-feral-watchdog-lint verify-feral-watchdog-test

.PHONY: verify-feral-controld-lint
verify-feral-controld-lint:
	@$(MAKE) verify-go-component-lint GO_COMPONENT=feral-controld

.PHONY: verify-feral-sys-monitord-lint
verify-feral-sys-monitord-lint:
	@$(MAKE) verify-go-component-lint GO_COMPONENT=feral-sys-monitord

.PHONY: verify-feral-watchdog-lint
verify-feral-watchdog-lint:
	@$(MAKE) verify-go-component-lint GO_COMPONENT=feral-watchdog

.PHONY: verify-feral-controld-test
verify-feral-controld-test:
	@./scripts/test-mint-pairing-ui.sh
	@$(MAKE) verify-go-component-test GO_COMPONENT=feral-controld

.PHONY: verify-feral-sys-monitord-test
verify-feral-sys-monitord-test:
	@$(MAKE) verify-go-component-test GO_COMPONENT=feral-sys-monitord

.PHONY: verify-feral-watchdog-test
verify-feral-watchdog-test:
	@$(MAKE) verify-go-component-test GO_COMPONENT=feral-watchdog

.PHONY: verify-go-component-lint
verify-go-component-lint:
	@test -n "$(GO_COMPONENT)" || (echo "GO_COMPONENT is required" >&2; exit 2)
	@cd components/$(GO_COMPONENT) && go mod download
	@cd components/$(GO_COMPONENT) && go vet ./...
	@cd components/$(GO_COMPONENT) && golangci-lint run ./...
	@cd components/$(GO_COMPONENT) && \
		unformatted="$$(gofmt -s -l .)"; \
		if [ -n "$$unformatted" ]; then \
			echo "Code is not formatted. Please run 'gofmt -s -w .'"; \
			echo "$$unformatted"; \
			exit 1; \
		fi

.PHONY: verify-go-component-test
verify-go-component-test:
	@test -n "$(GO_COMPONENT)" || (echo "GO_COMPONENT is required" >&2; exit 2)
	@cd components/$(GO_COMPONENT) && go mod download
	@cd components/$(GO_COMPONENT) && go vet ./...
	@cd components/$(GO_COMPONENT) && go test -v $$(go list ./... | grep -vE "/mocks|/wrapper")

.PHONY: verify-setupd
verify-setupd: verify-setupd-lint verify-setupd-test

.PHONY: verify-setupd-lint
verify-setupd-lint:
	@cd components/feral-setupd && cargo fmt -- --check
	@cd components/feral-setupd && cargo clippy --all-targets --all-features -- -D warnings
	@cd components/feral-setupd && cargo check --all-targets --all-features --verbose

.PHONY: verify-setupd-test
verify-setupd-test:
	@cd components/feral-setupd && cargo check --verbose
	@./scripts/test-serve-feral-player.sh
	@cd components/feral-setupd && cargo test --all-targets --all-features --verbose
