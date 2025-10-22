module github.com/feral-file/ffos-user/components/feral-watchdog

go 1.23.5

toolchain go1.23.9

require github.com/gorilla/websocket v1.5.3

require (
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/feral-file/godbus v0.0.6-0.20250530032926-fc5a2d7c32a7
	github.com/getsentry/sentry-go v0.33.0
	github.com/godbus/dbus/v5 v5.1.0
	github.com/golang/mock v1.6.0
	github.com/stretchr/testify v1.10.0
	go.uber.org/zap v1.27.0
)

require (
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sys v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
