module github.com/Feral-File/ffos-user/components/feral-watchdog

go 1.23.5

toolchain go1.23.9

require github.com/gorilla/websocket v1.5.3

require (
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/feral-file/godbus v0.0.6-0.20250530032926-fc5a2d7c32a7
	github.com/godbus/dbus/v5 v5.1.0
	go.uber.org/zap v1.27.0
)

require (
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
)
