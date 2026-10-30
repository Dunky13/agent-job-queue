// Package lifeline ties a helper process to the menu bar app that supervises it.
//
// The app starts each helper in its own session, so a crash or force quit would
// otherwise leave it running. The app passes the read end of a pipe as file
// descriptor 3 and names it in AGENT_QUEUE_LIFELINE=3, and keeps the write end.
// When the app dies the kernel closes the write end, the helper reads EOF and
// terminates itself through its ordinary SIGTERM shutdown path.
package lifeline

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Env names the descriptor the app inherited to the helper. Only the exact
// value "3" is honored.
const Env = "AGENT_QUEUE_LIFELINE"

const descriptor = 3

// The watcher's file is kept for the life of the process: a garbage-collected
// os.File would close the descriptor and fake an EOF.
var watched *os.File

// Adopt claims the lifeline before the helper can spawn any process. It must
// be called first from every helper entry point.
//
// The descriptor is always marked close-on-exec: Go's fork does not close
// inherited descriptors, so without it every worker and child process would
// hold the read end open and EOF would never arrive. The variable is removed
// so that no environment built from this process can forward it.
//
// Only the long-running commands pass watch. The descriptor is used only if it
// is the read side of a pipe; anything else is ignored, so a bogus value can
// never terminate a process. Adopt reports whether a watcher was started.
func Adopt(watch bool) bool {
	return adopt(watch, terminateSelf)
}

func terminateSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

func adopt(watch bool, onClose func()) bool {
	value, present := os.LookupEnv(Env)
	if !present {
		return false
	}
	if value != "3" {
		_ = os.Unsetenv(Env)
		return false
	}
	syscall.CloseOnExec(descriptor)
	_ = os.Unsetenv(Env)
	if !watch || !pipeReadEnd(descriptor) {
		return false
	}
	watched = os.NewFile(uintptr(descriptor), "agent-queue-lifeline")
	go func(file *os.File) {
		buffer := make([]byte, 64)
		for {
			if _, err := file.Read(buffer); err != nil {
				break
			}
		}
		onClose()
	}(watched)
	return true
}

func pipeReadEnd(fd int) bool {
	var info syscall.Stat_t
	if syscall.Fstat(fd, &info) != nil || info.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return false
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	return err == nil && flags&unix.O_ACCMODE != unix.O_WRONLY
}
