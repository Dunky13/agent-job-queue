package backend

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// installHost is the installer's port to what it asks the host about besides
// services. When LockHolders fails it still returns the holders listed before
// the failure, so the caller reports the first problem in listing order.
type installHost interface {
	Processes(ctx context.Context) ([]installProcess, error)
	LockHolders(ctx context.Context, path string) ([]int, error)
	VerifySignature(ctx context.Context, path string) error
}

// installProcess is one entry of the process list. Unparsed marks a listed line
// that does not read as a process ID and a command; Command then holds the whole
// line, so a safety check can still match it.
type installProcess struct {
	PID      int
	Command  string
	Unparsed bool
}

func (install *installer) hostQueries() installHost {
	if install.host == nil {
		return installMacOSHost{install}
	}
	return install.host
}

type installMacOSHost struct{ install *installer }

func (adapter installMacOSHost) Processes(ctx context.Context) ([]installProcess, error) {
	output, err := adapter.install.run(ctx, "ps", "-axo", "pid=,command=")
	if err != nil {
		return nil, err
	}
	var processes []installProcess
	for _, line := range strings.Split(string(output), "\n") {
		process := installProcess{Command: line, Unparsed: true}
		if fields := strings.Fields(line); len(fields) >= 2 {
			if pid, err := strconv.Atoi(fields[0]); err == nil {
				// Cut rather than rejoin the fields: paths may hold repeated spaces.
				command := strings.TrimSpace(strings.TrimSpace(line)[len(fields[0]):])
				process = installProcess{PID: pid, Command: command}
			}
		}
		processes = append(processes, process)
	}
	return processes, nil
}

func (adapter installMacOSHost) LockHolders(ctx context.Context, path string) ([]int, error) {
	// lsof also exits 1 when nothing holds path; that stays a failure.
	output, err := adapter.install.run(ctx, "/usr/sbin/lsof", "-nP", "-t", "--", path)
	if err != nil {
		return nil, err
	}
	var holders []int
	for _, holder := range strings.Fields(string(output)) {
		pid, err := strconv.Atoi(holder)
		if err != nil || pid <= 1 {
			return holders, fmt.Errorf("holder %q is not a process", holder)
		}
		holders = append(holders, pid)
	}
	return holders, nil
}

func (adapter installMacOSHost) VerifySignature(ctx context.Context, path string) error {
	_, err := adapter.install.run(ctx, "/usr/bin/codesign", "--verify", "--strict", "--verbose=2", path)
	return err
}
