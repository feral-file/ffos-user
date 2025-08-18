module github.com/feral-file/ffos-user/components/feral-app-monitord

go 1.23.5

require (
	github.com/feral-file/ffos-user/components/feral-sys-monitord v0.0.0-20250815110255-b5b5ead9b48d
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/feral-file/godbus v0.0.6-0.20250716043107-25b56328d11e
	github.com/godbus/dbus/v5 v5.1.0
	github.com/gowebpki/jcs v1.0.1
	go.uber.org/zap v1.27.0
)

require (
	github.com/cenkalti/backoff/v4 v4.3.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
)
