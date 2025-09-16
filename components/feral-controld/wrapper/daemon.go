package wrapper

import go_daemon "github.com/coreos/go-systemd/v22/daemon"

//go:generate mockgen -source=daemon.go -destination=../mocks/daemon.go -package=mocks -mock_names=Daemon=MockDaemon
type Daemon interface {
	SdNotify(unsetEnvironment bool, state string) (bool, error)
}

type daemon struct{}

func NewDaemon() Daemon {
	return &daemon{}
}

func (d *daemon) SdNotify(unsetEnvironment bool, state string) (bool, error) {
	return go_daemon.SdNotify(unsetEnvironment, state)
}
