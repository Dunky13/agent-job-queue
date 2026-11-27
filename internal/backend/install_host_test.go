package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// memoryHost is the installer's host port for tests: what is running, who holds
// each lock path and which paths fail signature verification. Every query is
// recorded as {"host", operation, path}.
type memoryHost struct {
	processes []installProcess
	// holders answers LockHolders; a test replaces it to state who holds a path.
	holders func(ctx context.Context, path string) ([]int, error)
	// verify answers VerifySignature; a test wraps it to refuse a path or act then.
	verify func(path string) error
	record func(call []string) error
}

func newMemoryHost(record func([]string) error) *memoryHost {
	return &memoryHost{
		record: record,
		// lsof fails when nothing holds a path.
		holders: func(_ context.Context, path string) ([]int, error) {
			return nil, errors.New("memory host: nothing holds " + path)
		},
		verify: func(string) error { return nil },
	}
}

func (host *memoryHost) Processes(context.Context) ([]installProcess, error) {
	if err := host.record([]string{"host", "processes"}); err != nil {
		return nil, err
	}
	for _, process := range host.processes {
		if !process.Unparsed && (process.PID <= 0 || process.Command == "") {
			return nil, fmt.Errorf("memory host: ps cannot list %+v", process)
		}
	}
	return slices.Clone(host.processes), nil
}

func (host *memoryHost) LockHolders(ctx context.Context, path string) ([]int, error) {
	if err := host.record([]string{"host", "lock-holders", path}); err != nil {
		return nil, err
	}
	holders, err := host.holders(ctx, path)
	for _, pid := range holders {
		if pid <= 1 {
			return nil, fmt.Errorf("memory host: %d cannot hold a lock", pid)
		}
	}
	return holders, err
}

func (host *memoryHost) VerifySignature(_ context.Context, path string) error {
	if err := host.record([]string{"host", "verify-signature", path}); err != nil {
		return err
	}
	// codesign refuses a path that does not exist.
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	return host.verify(path)
}

// Setup stops on any listed line naming old queue code, even one it cannot
// parse. Preflight reviews only processes it can identify.
func TestInstallerLegacyProcessChecks(t *testing.T) {
	f := installBaseFixture(t)
	install := f.installer
	if err := os.MkdirAll(install.state, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(install.root, "agent_heavy.py")
	for name, test := range map[string]struct {
		process  installProcess
		stop     string
		reviewed bool
	}{
		"queue daemon":     {installProcess{PID: 7, Command: "pueued --config " + filepath.Join(install.config, "pueue.yml")}, "external queue daemon", true},
		"python":           {installProcess{PID: 7, Command: "python " + script}, "Python queue process", true},
		"unparsed":         {installProcess{Command: script, Unparsed: true}, "Python queue process", false},
		"unrelated daemon": {installProcess{PID: 7, Command: "pueued --config /elsewhere/pueue.yml"}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			f.host.processes = []installProcess{test.process}
			err := install.stopLegacy(context.Background(), nil)
			if test.stop == "" && err != nil || test.stop != "" && (err == nil || !strings.Contains(err.Error(), test.stop)) {
				t.Fatalf("stop legacy: %v, want %q", err, test.stop)
			}
			report := installPreflightReport{SchemaVersion: 1, Issues: []installPreflightIssue{}}
			install.preflightLegacyProcesses(context.Background(), &report, true)
			if preflightIDs(report)["legacy-processes"] != test.reviewed {
				t.Fatalf("preflight: %+v", report.Issues)
			}
		})
	}
}

// The macOS adapter's own seam is the installer's command runner.
func TestInstallMacOSHost(t *testing.T) {
	var calls [][]string
	var output string
	var failure error
	install := &installer{command: func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		return []byte(output), failure
	}}
	host, ctx := install.hostQueries(), context.Background()

	output = "    1 /sbin/launchd\n  123 /Users/A  B/.venv/bin/python  agent_heavy.py \nlone\n x /legacy.py\n"
	processes, err := host.Processes(ctx)
	want := []installProcess{
		{PID: 1, Command: "/sbin/launchd"},
		{PID: 123, Command: "/Users/A  B/.venv/bin/python  agent_heavy.py"},
		{Command: "lone", Unparsed: true},
		{Command: " x /legacy.py", Unparsed: true},
		{Command: "", Unparsed: true},
	}
	if err != nil || !slices.Equal(processes, want) {
		t.Fatalf("processes: %+v %v", processes, err)
	}

	output = "123\n456\n"
	if holders, err := host.LockHolders(ctx, "/state/lock"); err != nil || !slices.Equal(holders, []int{123, 456}) {
		t.Fatalf("holders: %v %v", holders, err)
	}
	for _, refused := range []string{"abc", "1", "0", "-5"} {
		output = "123\n" + refused + "\n456\n"
		holders, err := host.LockHolders(ctx, "/state/lock")
		if err == nil || err.Error() != fmt.Sprintf("holder %q is not a process", refused) || !slices.Equal(holders, []int{123}) {
			t.Fatalf("holder %s: %v %v", refused, holders, err)
		}
	}
	output = "\n"
	if holders, err := host.LockHolders(ctx, "/state/lock"); err != nil || len(holders) != 0 {
		t.Fatalf("no holders: %v %v", holders, err)
	}

	if err := host.VerifySignature(ctx, "/Applications/Agent Queue.app"); err != nil {
		t.Fatal(err)
	}

	output, failure = "123\n", errors.New("exit status 1")
	if processes, err := host.Processes(ctx); processes != nil || !errors.Is(err, failure) {
		t.Fatalf("failed process list: %+v %v", processes, err)
	}
	if holders, err := host.LockHolders(ctx, "/state/lock"); holders != nil || !errors.Is(err, failure) {
		t.Fatalf("failed holder query: %v %v", holders, err)
	}
	if err := host.VerifySignature(ctx, "/Applications/Agent Queue.app"); !errors.Is(err, failure) {
		t.Fatal("signature verification hid its failure", err)
	}

	ps := []string{"ps", "-axo", "pid=,command="}
	lsof := []string{"/usr/sbin/lsof", "-nP", "-t", "--", "/state/lock"}
	codesign := []string{"/usr/bin/codesign", "--verify", "--strict", "--verbose=2", "/Applications/Agent Queue.app"}
	argv := [][]string{ps, lsof, lsof, lsof, lsof, lsof, lsof, codesign, ps, lsof, codesign}
	if !slices.EqualFunc(calls, argv, slices.Equal[[]string]) {
		t.Fatalf("host argv: %v", calls)
	}
}
