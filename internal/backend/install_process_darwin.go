package backend

import (
	"bytes"
	"errors"
	"os"

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

// installProcessParent returns the parent the kernel records for a process this
// user owns.
func installProcessParent(pid int) (int, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if int(info.Proc.P_pid) != pid || info.Eproc.Pcred.P_ruid != uint32(os.Getuid()) {
		return 0, errors.New("process does not belong to this user")
	}
	return int(info.Eproc.Ppid), nil
}
