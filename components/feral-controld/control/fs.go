package control

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FS is the filesystem surface the control store needs. It is narrower than
// wrapper.OS on purpose and adds the two durability primitives wrapper.OS
// lacks (fsync of a file and of a directory), which are the whole point of
// the replay window (§6): a window that is accepted but not durable lets a
// captured request replay after a power cut.
type FS interface {
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
	MkdirAll(path string, perm fs.FileMode) error
	Mkdir(path string, perm fs.FileMode) error
	// WriteFileAtomic replaces name with data: write name+".tmp", fsync it,
	// rename over name, fsync the parent directory. committed reports whether
	// the rename happened; once it has, name holds data even if the directory
	// fsync then fails (same contract as contentpolicy.persistAtomic).
	WriteFileAtomic(name string, data []byte, perm fs.FileMode) (committed bool, err error)
	Rename(oldpath, newpath string) error
	RemoveAll(path string) error
	SyncDir(path string) error
}

// OSFS is the production FS. It counts fsync calls so the per-request
// durability cost is observable (benchmarks, metrics).
type OSFS struct {
	fsyncs atomic.Int64
}

// Fsyncs returns how many fsync calls (files and directories) were issued.
func (o *OSFS) Fsyncs() int64 { return o.fsyncs.Load() }

func (o *OSFS) ReadFile(name string) ([]byte, error)      { return os.ReadFile(name) } //nolint:gosec // G304: fixed files under the control dir.
func (o *OSFS) Stat(name string) (fs.FileInfo, error)     { return os.Stat(name) }
func (o *OSFS) MkdirAll(p string, perm fs.FileMode) error { return os.MkdirAll(p, perm) }
func (o *OSFS) Mkdir(p string, perm fs.FileMode) error    { return os.Mkdir(p, perm) }
func (o *OSFS) Rename(a, b string) error                  { return os.Rename(a, b) }
func (o *OSFS) RemoveAll(p string) error                  { return os.RemoveAll(p) }

func (o *OSFS) SyncDir(p string) error {
	d, err := os.Open(p) //nolint:gosec // G304: the control dir or its parent.
	if err != nil {
		return err
	}
	o.fsyncs.Add(1)
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

func (o *OSFS) WriteFileAtomic(name string, data []byte, perm fs.FileMode) (bool, error) {
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // G304: fixed file under the control dir.
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return false, err
	}
	o.fsyncs.Add(1)
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, name); err != nil {
		return false, err
	}
	return true, o.SyncDir(filepath.Dir(name))
}

// MemFS is an in-memory FS for tests and fixture generation. Paths are
// cleaned slash paths. Fault injection: set FailWrite/FailSync to make the
// next matching call fail.
type MemFS struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
	// FailWrite, when it returns an error for a name, makes WriteFileAtomic
	// fail before the rename (committed=false).
	FailWrite func(name string) error
	// FailSync, when it returns an error, makes WriteFileAtomic fail after
	// the rename (committed=true), modeling a failed directory fsync.
	FailSync func(name string) error
	Writes   int
}

// NewMemFS returns an empty in-memory filesystem whose root exists.
func NewMemFS() *MemFS {
	return &MemFS{files: map[string][]byte{}, dirs: map[string]bool{"/": true, ".": true}}
}

func clean(p string) string { return path.Clean(filepath.ToSlash(p)) }

type memInfo struct {
	name string
	dir  bool
	size int64
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o700
	}
	return 0o600
}
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

func (m *MemFS) ReadFile(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[clean(name)]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), b...), nil
}

func (m *MemFS) Stat(name string) (fs.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := clean(name)
	if m.dirs[n] {
		return memInfo{name: path.Base(n), dir: true}, nil
	}
	if b, ok := m.files[n]; ok {
		return memInfo{name: path.Base(n), size: int64(len(b))}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (m *MemFS) MkdirAll(p string, _ fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n := clean(p); ; n = path.Dir(n) {
		m.dirs[n] = true
		if n == "/" || n == "." {
			return nil
		}
	}
}

func (m *MemFS) Mkdir(p string, _ fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := clean(p)
	if m.dirs[n] {
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrExist}
	}
	if !m.dirs[path.Dir(n)] {
		return &fs.PathError{Op: "mkdir", Path: p, Err: fs.ErrNotExist}
	}
	m.dirs[n] = true
	return nil
}

func (m *MemFS) WriteFileAtomic(name string, data []byte, _ fs.FileMode) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := clean(name)
	if !m.dirs[path.Dir(n)] {
		return false, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if m.FailWrite != nil {
		if err := m.FailWrite(n); err != nil {
			return false, err
		}
	}
	m.files[n] = append([]byte(nil), data...)
	m.Writes++
	if m.FailSync != nil {
		if err := m.FailSync(n); err != nil {
			return true, err
		}
	}
	return true, nil
}

// SetFile writes a file directly (test setup, corruption injection).
func (m *MemFS) SetFile(name string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := clean(name)
	m.files[n] = append([]byte(nil), data...)
	for d := path.Dir(n); !m.dirs[d]; d = path.Dir(d) {
		m.dirs[d] = true
	}
}

func (m *MemFS) Rename(a, b string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, to := clean(a), clean(b)
	if data, ok := m.files[from]; ok {
		m.files[to] = data
		delete(m.files, from)
		return nil
	}
	if !m.dirs[from] {
		return &fs.PathError{Op: "rename", Path: a, Err: fs.ErrNotExist}
	}
	if m.dirs[to] {
		return &fs.PathError{Op: "rename", Path: b, Err: fs.ErrExist}
	}
	// Move the directory and everything under it.
	var keys []string
	for k := range m.dirs {
		if k == from || strings.HasPrefix(k, from+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		delete(m.dirs, k)
		m.dirs[to+strings.TrimPrefix(k, from)] = true
	}
	for k, v := range m.files {
		if strings.HasPrefix(k, from+"/") {
			delete(m.files, k)
			m.files[to+strings.TrimPrefix(k, from)] = v
		}
	}
	return nil
}

func (m *MemFS) RemoveAll(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := clean(p)
	for k := range m.dirs {
		if k == n || strings.HasPrefix(k, n+"/") {
			delete(m.dirs, k)
		}
	}
	for k := range m.files {
		if k == n || strings.HasPrefix(k, n+"/") {
			delete(m.files, k)
		}
	}
	return nil
}

func (m *MemFS) SyncDir(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirs[clean(p)] {
		return &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	return nil
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
