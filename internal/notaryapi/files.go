package notaryapi

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Resolve existing parent directories physically (macOS /var aliases /private/var),
// then walk the resolved components without following symlinks. Final input files
// are opened separately with O_NOFOLLOW and must be regular files.
func openDirectory(path string) (*os.File, error) {
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(physical)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), abs), nil
}

func openRegular(path string) (*os.File, error) {
	dir, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return openRegularAt(dir, filepath.Base(path))
}

func openRegularAt(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, failure("invalid_file")
	}
	return f, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, failure("response_failed")
	}
	return data, nil
}

// Atomic replacement with both the file and directory fsynced. A failed sync
// fails closed even if the rename has already happened.
func atomicWrite(dir *os.File, name string, data []byte) error {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	tmp := ".notary-" + hex.EncodeToString(entropy[:])
	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = f.Close(); _ = unix.Unlinkat(int(dir.Fd()), tmp, 0) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = unix.Renameat(int(dir.Fd()), tmp, int(dir.Fd()), name); err != nil {
		return err
	}
	return dir.Sync()
}

// Reject duplicate keys and trailing JSON as well as unknown/missing fields.
func recordFields(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, failure("invalid_record")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return nil, failure("invalid_record")
		}
		name, ok := t.(string)
		if !ok {
			return nil, failure("invalid_record")
		}
		if _, exists := fields[name]; exists {
			return nil, failure("invalid_record")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, failure("invalid_record")
		}
		fields[name] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, failure("invalid_record")
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, failure("invalid_record")
	}
	return fields, nil
}
