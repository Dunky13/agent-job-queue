package lifeline

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const modeEnv = "AGENT_QUEUE_LIFELINE_TEST_MODE"

// Each scenario runs in a fresh copy of the test binary because the lifeline
// owns descriptor 3 of a whole process.
func TestMain(m *testing.M) {
	switch os.Getenv(modeEnv) {
	case "":
		os.Exit(m.Run())
	case "watch":
		helperWatch()
	case "inherit":
		helperInherit()
	case "report":
		helperReport()
	default:
		os.Exit(125)
	}
}

// helperWatch behaves like a daemon: SIGTERM is its orderly shutdown path.
func helperWatch() {
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, syscall.SIGTERM)
	started := Adopt(true)
	fmt.Printf("ready started=%v env=%q\n", started, os.Getenv(Env))
	select {
	case <-terminated:
		fmt.Println("sigterm")
		os.Exit(0)
	case <-time.After(500 * time.Millisecond):
		fmt.Println("alive")
		os.Exit(0)
	}
}

// helperInherit spawns an ordinary child and reports what that child holds.
func helperInherit() {
	started := Adopt(os.Getenv(modeEnv+"_WATCH") == "1")
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), modeEnv+"=report")
	out, err := child.CombinedOutput()
	if err != nil {
		fmt.Printf("child failed: %v %s\n", err, out)
		os.Exit(1)
	}
	fmt.Printf("started=%v %s", started, out)
	os.Exit(0)
}

func helperReport() {
	var info syscall.Stat_t
	state := "closed"
	if syscall.Fstat(3, &info) == nil {
		state = "open"
	}
	_, present := os.LookupEnv(Env)
	fmt.Printf("fd3=%s env=%v\n", state, present)
	os.Exit(0)
}

type helper struct {
	command *exec.Cmd
	lines   *bufio.Scanner
	done    chan error
}

func startHelper(t *testing.T, mode string, fd3 *os.File, environment ...string) *helper {
	t.Helper()
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), modeEnv+"="+mode)
	command.Env = append(command.Env, environment...)
	if fd3 != nil {
		command.ExtraFiles = []*os.File{fd3}
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{command: command, lines: bufio.NewScanner(stdout), done: make(chan error, 1)}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return h
}

func (h *helper) line(t *testing.T) string {
	t.Helper()
	if !h.lines.Scan() {
		t.Fatalf("helper ended without output: %v", h.lines.Err())
	}
	return h.lines.Text()
}

func (h *helper) exit(t *testing.T) error {
	t.Helper()
	go func() { h.done <- h.command.Wait() }()
	select {
	case err := <-h.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not exit")
		return nil
	}
}

func TestEOFTerminatesThroughTheSIGTERMPath(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	h := startHelper(t, "watch", read, Env+"=3")
	read.Close()
	if line := h.line(t); line != `ready started=true env=""` {
		t.Fatalf("unexpected readiness: %q", line)
	}
	// The app holding the write end keeps the helper alive.
	time.Sleep(150 * time.Millisecond)
	begin := time.Now()
	write.Close()
	if line := h.line(t); line != "sigterm" {
		t.Fatalf("closing the lifeline did not take the SIGTERM path: %q", line)
	}
	if err := h.exit(t); err != nil {
		t.Fatalf("shutdown was not orderly: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}
}

func TestWriteEndHeldKeepsTheProcessAlive(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	h := startHelper(t, "watch", read, Env+"=3")
	read.Close()
	h.line(t)
	// Data written to the lifeline is not a signal either.
	if _, err := write.Write([]byte("ignored")); err != nil {
		t.Fatal(err)
	}
	if line := h.line(t); line != "alive" {
		t.Fatalf("a held lifeline terminated the process: %q", line)
	}
}

func TestOnlyTheExactDescriptorNameIsHonored(t *testing.T) {
	for _, value := range []string{"4", "03", " 3", "3 ", "", "fd3"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			h := startHelper(t, "watch", read, Env+"="+value)
			read.Close()
			write.Close()
			if line := h.line(t); line != `ready started=false env=""` {
				t.Fatalf("unexpected readiness: %q", line)
			}
			if line := h.line(t); line != "alive" {
				t.Fatalf("a lifeline with value %q terminated the process: %q", value, line)
			}
		})
	}
}

func TestNonPipeDescriptorIsIgnored(t *testing.T) {
	regular, err := os.Create(filepath.Join(t.TempDir(), "not-a-pipe"))
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	// A write end is not a lifeline: reading it fails at once and must not kill.
	_, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	for name, file := range map[string]*os.File{"regular file": regular, "pipe write end": write} {
		t.Run(name, func(t *testing.T) {
			h := startHelper(t, "watch", file, Env+"=3")
			if line := h.line(t); line != `ready started=false env=""` {
				t.Fatalf("a bogus descriptor was adopted: %q", line)
			}
			if line := h.line(t); line != "alive" {
				t.Fatalf("a bogus descriptor terminated the process: %q", line)
			}
		})
	}
}

func TestNoLifelineEnvironmentLeavesTheProcessAlone(t *testing.T) {
	h := startHelper(t, "watch", nil)
	if line := h.line(t); line != `ready started=false env=""` {
		t.Fatalf("unexpected readiness: %q", line)
	}
	if line := h.line(t); line != "alive" {
		t.Fatalf("a process without a lifeline terminated: %q", line)
	}
}

func TestSpawnedChildrenDoNotInheritTheLifeline(t *testing.T) {
	for _, watch := range []string{"1", "0"} {
		t.Run("watch="+watch, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer write.Close()
			h := startHelper(t, "inherit", read, Env+"=3", modeEnv+"_WATCH="+watch)
			read.Close()
			want := "started=" + map[string]string{"1": "true", "0": "false"}[watch] + " fd3=closed env=false"
			if line := h.line(t); line != want {
				t.Fatalf("child saw %q, want %q", line, want)
			}
			if err := h.exit(t); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContractNames(t *testing.T) {
	if Env != "AGENT_QUEUE_LIFELINE" || descriptor != 3 {
		t.Fatal("the Swift supervisor depends on these exact names")
	}
}
