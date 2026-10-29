package backend

import (
	"bytes"
	"errors"

	"golang.org/x/sys/unix"
)

// installProcessExecutable returns the absolute path the kernel recorded when
// pid executed, independent of the argv[0] the process presents.
func installProcessExecutable(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(raw) < 5 {
		return "", errors.New("process arguments are truncated")
	}
	path, _, found := bytes.Cut(raw[4:], []byte{0})
	if !found || len(path) == 0 || path[0] != '/' {
		return "", errors.New("process executable path is unavailable")
	}
	return string(path), nil
}
