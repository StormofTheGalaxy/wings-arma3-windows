//go:build windows

package ufs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type WalkDiratFunc func(dirfd int, name, relative string, d DirEntry, err error) error

type windowsDirEntry struct {
	fs    *UnixFS
	entry os.DirEntry
	path  string
}

func (de windowsDirEntry) Name() string { return de.entry.Name() }

func (de windowsDirEntry) IsDir() bool { return de.Type().IsDir() }

func (de windowsDirEntry) Type() FileMode {
	if de.entry.Type()&ModeSymlink != 0 {
		if info, err := os.Stat(de.fs.fullPath(de.path)); err == nil && info.IsDir() {
			return ModeDir
		}
	}
	return de.entry.Type()
}

func (de windowsDirEntry) Info() (FileInfo, error) {
	info, err := de.entry.Info()
	if err != nil {
		return nil, err
	}
	if info.Mode()&ModeSymlink != 0 {
		if targetInfo, statErr := os.Stat(de.fs.fullPath(de.path)); statErr == nil && targetInfo.IsDir() {
			return targetInfo, nil
		}
	}
	return info, nil
}

func (de windowsDirEntry) Open() (File, error) { return de.fs.OpenFile(de.path, O_RDONLY, 0) }

type UnixFS struct {
	basePath string
}

func NewUnixFS(basePath string, _ bool) (*UnixFS, error) {
	return &UnixFS{basePath: filepath.Clean(basePath)}, nil
}

func (fsys *UnixFS) BasePath() string { return fsys.basePath }

func (fsys *UnixFS) Close() error { return nil }

func (fsys *UnixFS) fullPath(name string) string {
	name = filepath.Clean(filepath.FromSlash(name))
	if name == "." || name == string(filepath.Separator) {
		return fsys.basePath
	}
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(fsys.basePath, name)
}

func joinRelative(base, name string) string {
	base = filepath.Clean(filepath.FromSlash(base))
	if base == "." || base == string(filepath.Separator) {
		return name
	}
	return filepath.Join(base, name)
}

func (fsys *UnixFS) unsafePath(name string) (string, error) {
	name = filepath.Clean(filepath.FromSlash(name))
	name = strings.TrimPrefix(name, string(filepath.Separator))
	if name == "" {
		return ".", nil
	}
	return name, nil
}

func (fsys *UnixFS) Chmod(name string, mode FileMode) error {
	return os.Chmod(fsys.fullPath(name), mode)
}

func (fsys *UnixFS) Chown(string, int, int) error { return nil }

func (fsys *UnixFS) Lchown(string, int, int) error { return nil }

func (fsys *UnixFS) Lchownat(int, string, int, int) error { return nil }

func (fsys *UnixFS) Chtimes(name string, atime, mtime time.Time) error {
	return os.Chtimes(fsys.fullPath(name), atime, mtime)
}

func (fsys *UnixFS) Create(name string) (File, error) {
	return fsys.OpenFile(name, O_CREATE|O_WRONLY|O_TRUNC, 0o644)
}

func (fsys *UnixFS) Mkdir(name string, perm FileMode) error {
	return os.Mkdir(fsys.fullPath(name), perm)
}

func (fsys *UnixFS) MkdirAll(name string, perm FileMode) ([]string, error) {
	p := fsys.fullPath(name)
	if err := os.MkdirAll(p, perm); err != nil {
		return nil, err
	}
	return []string{name}, nil
}

func (fsys *UnixFS) Open(name string) (File, error) {
	return os.Open(fsys.fullPath(name))
}

func (fsys *UnixFS) OpenFile(name string, flag int, perm FileMode) (File, error) {
	p := fsys.fullPath(name)
	if flag&O_CREATE != 0 {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(p, flag, perm)
}

func (fsys *UnixFS) OpenFileat(_ int, name string, flag int, perm FileMode) (File, error) {
	return fsys.OpenFile(name, flag, perm)
}

func (fsys *UnixFS) Touch(name string, flag int, perm FileMode) (File, error) {
	return fsys.OpenFile(name, flag|O_CREATE, perm)
}

func (fsys *UnixFS) ReadDir(name string) ([]DirEntry, error) {
	entries, err := os.ReadDir(fsys.fullPath(name))
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, windowsDirEntry{fs: fsys, entry: entry, path: joinRelative(name, entry.Name())})
	}
	return out, nil
}

func (fsys *UnixFS) Remove(name string) error {
	return os.Remove(fsys.fullPath(name))
}

func (fsys *UnixFS) RemoveAll(name string) error {
	return os.RemoveAll(fsys.fullPath(name))
}

func (fsys *UnixFS) Rename(oldname, newname string) error {
	return os.Rename(fsys.fullPath(oldname), fsys.fullPath(newname))
}

func (fsys *UnixFS) Stat(name string) (FileInfo, error) {
	return os.Stat(fsys.fullPath(name))
}

func (fsys *UnixFS) Lstat(name string) (FileInfo, error) {
	return os.Lstat(fsys.fullPath(name))
}

func (fsys *UnixFS) Symlink(oldname, newname string) error {
	return os.Symlink(fsys.fullPath(oldname), fsys.fullPath(newname))
}

func (fsys *UnixFS) WalkDir(root string, fn WalkDirFunc) error {
	base := fsys.fullPath(root)
	return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(fsys.basePath, path)
		if rerr != nil {
			rel = path
		}
		if d == nil {
			return fn(filepath.ToSlash(rel), nil, err)
		}
		return fn(filepath.ToSlash(rel), windowsDirEntry{fs: fsys, entry: d, path: rel}, err)
	})
}

func (fsys *UnixFS) WalkDirat(_ int, name string, fn WalkDiratFunc) error {
	base := fsys.fullPath(name)
	return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil || rel == "." {
			rel = filepath.Base(path)
		}
		if d == nil {
			return fn(0, filepath.ToSlash(path), filepath.ToSlash(rel), nil, err)
		}
		return fn(0, filepath.ToSlash(path), filepath.ToSlash(rel), windowsDirEntry{fs: fsys, entry: d, path: path}, err)
	})
}

func (fsys *UnixFS) SafePath(name string) (int, string, func(), error) {
	return 0, fsys.fullPath(name), func() {}, nil
}

func (fsys *UnixFS) RemoveStat(name string) (FileInfo, error) {
	st, err := fsys.Lstat(name)
	if err != nil {
		return nil, err
	}
	return st, fsys.Remove(name)
}

func (fsys *UnixFS) Lstatat(_ int, name string) (FileInfo, error) { return fsys.Lstat(name) }

func (fsys *UnixFS) unlinkat(_ int, name string, flags int) error {
	if flags&AT_REMOVEDIR != 0 {
		return os.Remove(fsys.fullPath(name))
	}
	return os.Remove(fsys.fullPath(name))
}

func ReadDirMap[T any](fsys *UnixFS, path string, fn func(DirEntry) (T, error)) ([]T, error) {
	entries, err := fsys.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(entries))
	for _, entry := range entries {
		v, err := fn(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func removeAll(fsys *Quota, path string) error {
	path, err := fsys.unsafePath(path)
	if err != nil {
		return err
	}
	if path == "." {
		return errors.New("refusing to remove filesystem root")
	}
	return os.RemoveAll(fsys.fullPath(path))
}
