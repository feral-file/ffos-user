//nolint:gosec
package wrapper

import (
	"context"
	go_os "os"
	go_exec "os/exec"
	go_signal "os/signal"
)

//go:generate mockgen -source=os.go -destination=../mocks/os.go -package=mocks -mock_names=OS=MockOS
type OS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm go_os.FileMode) error
	ReadDir(path string) ([]go_os.DirEntry, error)
	IsNotExist(err error) bool
	MkdirAll(path string, perm go_os.FileMode) error
	Rename(oldpath, newpath string) error
	// Remove and RemoveAll back the offline-cache store's item/blob deletion and
	// GC sweep (see components/feral-controld/offlinecache). Added alongside that
	// feature; every other OS caller is unaffected since these are additive.
	Remove(path string) error
	RemoveAll(path string) error
	// Stat backs blob/disk size accounting in the offline-cache store.
	Stat(path string) (go_os.FileInfo, error)
	// Open backs the offline-cache static server's streamed serving of
	// large blobs (io.ReadSeeker, so http.ServeContent can honor Range
	// requests for video seeking) without loading the whole file into
	// memory the way ReadFile would.
	Open(path string) (*go_os.File, error)
	// CreateTemp backs the offline-cache store's streaming blob write
	// path (see offlinecache/store.go's WriteBlob): a blob's
	// content-addressed name is only known after its full content has
	// been hashed, so the write has to land in a uniquely-named temp
	// file first and get renamed into place once the hash is known.
	CreateTemp(dir, pattern string) (*go_os.File, error)
	Exit(code int)
}

type os struct{}

func NewOS() OS {
	return os{}
}

func (o os) ReadFile(path string) ([]byte, error) {
	return go_os.ReadFile(path)
}

func (o os) WriteFile(path string, data []byte, perm go_os.FileMode) error {
	return go_os.WriteFile(path, data, perm)
}

func (o os) ReadDir(path string) ([]go_os.DirEntry, error) {
	return go_os.ReadDir(path)
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

func (o os) Remove(path string) error {
	return go_os.Remove(path)
}

func (o os) RemoveAll(path string) error {
	return go_os.RemoveAll(path)
}

func (o os) Stat(path string) (go_os.FileInfo, error) {
	return go_os.Stat(path)
}

func (o os) Open(path string) (*go_os.File, error) {
	return go_os.Open(path)
}

func (o os) CreateTemp(dir, pattern string) (*go_os.File, error) {
	return go_os.CreateTemp(dir, pattern)
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
