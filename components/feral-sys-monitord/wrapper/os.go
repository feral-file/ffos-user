//nolint:gosec
package wrapper

import (
	"context"
	"io"
	go_os "os"
	go_exec "os/exec"
	go_signal "os/signal"
)

//go:generate mockgen -source=os.go -destination=../mocks/os.go -package=mocks -mock_names=OS=MockOS
type OS interface {
	Open(name string) (*go_os.File, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm go_os.FileMode) error
	IsNotExist(err error) bool
	MkdirAll(path string, perm go_os.FileMode) error
	Rename(oldpath, newpath string) error
	Exit(code int)
}

type os struct{}

func NewOS() OS {
	return os{}
}

func (o os) Open(name string) (*go_os.File, error) {
	return go_os.Open(name)
}

func (o os) ReadFile(path string) ([]byte, error) {
	return go_os.ReadFile(path)
}

func (o os) WriteFile(path string, data []byte, perm go_os.FileMode) error {
	return go_os.WriteFile(path, data, perm)
}

func (o os) IsNotExist(err error) bool {
	return go_os.IsNotExist(err)
}

func (o os) MkdirAll(path string, perm go_os.FileMode) error {
	return go_os.MkdirAll(path, perm)
}

func (o os) Rename(oldpath, newpath string) error {
	return go_os.Rename(oldpath, newpath)
}

func (o os) Exit(code int) {
	go_os.Exit(code)
}

//go:generate mockgen -source=os.go -destination=../mocks/os.go -package=mocks -mock_names=Exec=MockExec
type Exec interface {
	CommandContext(ctx context.Context, name string, arg ...string) ExecCmd
}

type exec struct{}

func NewExec() Exec {
	return &exec{}
}

func (e *exec) CommandContext(ctx context.Context, name string, arg ...string) ExecCmd {
	return execCmd{cmd: go_exec.CommandContext(ctx, name, arg...)}
}

//go:generate mockgen -source=os.go -destination=../mocks/os.go -package=mocks -mock_names=ExecCmd=MockExecCmd
type ExecCmd interface {
	String() string
	Run() error
	Start() error
	Wait() error
	Output() ([]byte, error)
	CombinedOutput() ([]byte, error)
	SetStderr(w io.Writer)
	StdoutPipe() (io.ReadCloser, error)
}

type execCmd struct {
	cmd *go_exec.Cmd
}

func (e execCmd) String() string {
	return e.cmd.String()
}

func (e execCmd) Run() error {
	return e.cmd.Run()
}

func (e execCmd) Start() error {
	return e.cmd.Start()
}

func (e execCmd) Wait() error {
	return e.cmd.Wait()
}

func (e execCmd) SetStderr(w io.Writer) {
	e.cmd.Stderr = w
}

func (e execCmd) StdoutPipe() (io.ReadCloser, error) {
	return e.cmd.StdoutPipe()
}

func (e execCmd) Output() ([]byte, error) {
	return e.cmd.Output()
}

func (e execCmd) CombinedOutput() ([]byte, error) {
	return e.cmd.CombinedOutput()
}

//go:generate mockgen -source=os.go -destination=../mocks/os.go -package=mocks -mock_names=Signal=MockSignal
type Signal interface {
	Notify(c chan<- go_os.Signal, sig ...go_os.Signal)
}

type signal struct{}

func NewSignal() Signal {
	return &signal{}
}

func (s *signal) Notify(c chan<- go_os.Signal, sig ...go_os.Signal) {
	go_signal.Notify(c, sig...)
}
