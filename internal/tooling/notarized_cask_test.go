package tooling

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

// This test is deliberately opt-in: an ad hoc PR build cannot establish the
// quarantined release's Developer ID trust or notarized execution behavior.
func TestNotarizedHomebrew(t *testing.T) {
	payload := os.Getenv("AGENT_QUEUE_TEST_NOTARIZED_PAYLOAD")
	if payload == "" {
		t.Skip("final notarized release payload not supplied")
	}
	team := os.Getenv("APPLE_TEAM_ID")
	if runtime.GOOS != "darwin" || !filepath.IsAbs(payload) || !regexp.MustCompile(`^[A-Z0-9]{10}$`).MatchString(team) {
		t.Fatal("notarized runtime requires macOS, an absolute payload and APPLE_TEAM_ID")
	}
	payload, err := filepath.EvalSymlinks(payload)
	if err != nil {
		t.Fatal(err)
	}
	fixture, home, environment, owner := notarizedCaskFixture(t, team)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	run := func(name string, args ...string) []byte {
		t.Helper()
		command, err := owner.start(ctx, environment, nil, name, args...)
		if err != nil {
			t.Fatal(err)
		}
		output, err := command.wait()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", filepath.Base(name), err, output)
		}
		return output
	}
	// Reject untrusted artifacts before starting even the private daemon. The
	// verification helper checks both universal slices, both bundles and tickets.
	installer := filepath.Dir(filepath.Dir(filepath.Dir(payload)))
	if filepath.Join(installer, "Contents/Resources/payload") != payload {
		t.Fatal("payload must come from the extracted final installer bundle")
	}
	run("/bin/bash", "../../scripts/macos-signing.sh", "verify-installer", installer, team)
	executable := filepath.Join(payload, "source/bin/agent-heavy")
	// Apple recommends codesign's notarized requirement for non-app code;
	// spctl's execute assessment is intended for top-level application bundles.
	requirement := fmt.Sprintf("=anchor apple generic and certificate leaf[subject.OU] = \"%s\" and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized", team)
	run("/usr/bin/codesign", "--verify", "--strict", "--all-architectures", "--check-notarization", "--test-requirement", requirement, executable)
	run("/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", payload, filepath.Join(fixture, "payload"))
	metadata, err := os.ReadFile(filepath.Join(payload, "source/build-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := backend.ValidateBuildInfo(metadata)
	if err != nil {
		t.Fatal(err)
	}
	cask := notarizedFixtureCask(t, fixture, identity.Version)
	log, err := os.Create(filepath.Join(fixture, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := owner.start(ctx, environment, log, executable, "daemon")
	if err != nil {
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		probeCtx, stop := context.WithTimeout(ctx, time.Second)
		probe, err := owner.start(probeCtx, environment, nil, executable, "status", "--json")
		if err == nil {
			_, err = probe.wait()
		}
		stop()
		if err == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		output, _ := os.ReadFile(filepath.Join(fixture, "daemon.log"))
		t.Fatalf("private signed queue daemon did not become ready: %s", output)
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal(err)
	}
	// Ruby owns Homebrew's separate installer groups for at most 60 seconds,
	// including verification. Leave time for its TERM/KILL/reap cleanup before
	// the outer Go deadline can cancel Ruby itself.
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 75*time.Second {
		t.Fatal("insufficient remaining time for the bounded Homebrew command owner")
	}
	output := run(brew, "ruby", "--", "../../packaging/homebrew/test-cask.rb", cask, fixture, "--quarantined-runtime")
	var result struct {
		Executed bool           `json:"fixture_installer_executed"`
		Attrs    map[string]int `json:"fixture_attributes"`
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil || !result.Executed {
		t.Fatalf("real cask installer did not complete: %v\n%s", err, output)
	}
	if result.Attrs["entries"] == 0 || len(result.Attrs) != 5 {
		t.Fatalf("missing staged attribute evidence: %v", result.Attrs)
	}
	for name, count := range result.Attrs {
		if count != result.Attrs["entries"] {
			t.Fatalf("staged %s was not retained: %v", name, result.Attrs)
		}
	}
	installed, err := os.ReadFile(filepath.Join(home, ".local/share/agent-heavy/build-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	installedIdentity, err := backend.ValidateBuildInfo(installed)
	if err != nil || !identity.Equal(installedIdentity) {
		t.Fatalf("real installed identity differs: %v", err)
	}
	for _, name := range []string{"Applications/Agent Queue.app/Contents/MacOS/AgentQueue", ".local/share/agent-heavy/bin/agent-heavy", ".local/bin/agent-remote", "Library/LaunchAgents/io.went.agent-resources.plist"} {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Fatalf("missing real installed file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local/share/agent-heavy/install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatal("real installation did not finish its transaction")
	}
	run(filepath.Join(home, ".local/share/agent-heavy/bin/agent-heavy"), "status", "--json")
	select {
	case <-daemon.done:
		t.Fatalf("installation stopped the private daemon: %v", daemon.waitErr)
	default:
	}
	calls, err := os.ReadFile(filepath.Join(fixture, "calls"))
	if err != nil || strings.Contains(string(calls), "UNEXPECTED") || !strings.Contains(string(calls), "bootstrap") {
		t.Fatalf("private launchctl boundary was not exercised safely: %v\n%s", err, calls)
	}
	t.Logf("Real quarantined cask install completed; %d staged entries retain both attributes and the private daemon remains healthy", result.Attrs["entries"])
}

func TestNotarizedHomebrewRejectsUnsignedPayload(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew runtime verification requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	fixture, home, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
	binary := filepath.Join(fixture, "payload/source/bin/agent-heavy")
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	// Executing this unsigned entry would leave observable evidence.
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntouch \"$HOME/unsigned-executed\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cask := notarizedFixtureCask(t, fixture, "0.1.0")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command, err := owner.start(ctx, environment, nil, brew, "ruby", "--", "../../packaging/homebrew/test-cask.rb", cask, fixture, "--quarantined-runtime")
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.wait()
	if err == nil || !strings.Contains(string(output), "failed Developer ID/notarization verification") {
		t.Fatalf("unsigned payload was not rejected by the trust check: %v\n%s", err, output)
	}
	for _, name := range []string{filepath.Join(home, "unsigned-executed"), filepath.Join(fixture, "Caskroom"), filepath.Join(fixture, "calls")} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("unsigned rejection executed or staged content: %s (%v)", filepath.Base(name), err)
		}
	}
}

// Exercise Homebrew's real separate process group, rather than assuming its
// installer inherits brew's group. Both timeout and successful parent exit must
// remove a TERM-ignoring grandchild before the private tools can disappear.
func TestNotarizedHomebrewCommandDescendants(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Homebrew command ownership requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	loader, err := filepath.Abs("../../packaging/homebrew/test-cask.rb")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"timeout", "normal-exit", "outer-cancel"} {
		t.Run(mode, func(t *testing.T) {
			fixture, _, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
			child := filepath.Join(fixture, "child.sh")
			parent := filepath.Join(fixture, "parent.sh")
			driver := filepath.Join(fixture, "driver.rb")
			files := map[string]string{
				child: `#!/bin/sh
trap '' TERM
printf '%s\n' "$$" > "$TEST_CHILD_PID"
/bin/ps -o pgid= -p "$$" >> "$TEST_CHILD_PID"
while :; do /bin/sleep 1; done
`,
				parent: `#!/bin/sh
printf '%s\n' "$$" > "$TEST_PARENT_PID"
/bin/sh "$TEST_CHILD_SCRIPT" &
while [ ! -s "$TEST_CHILD_PID" ] || [ "$(/usr/bin/wc -l < "$TEST_CHILD_PID")" -lt 2 ]; do /bin/sleep 0.01; done
if [ "$TEST_PARENT_MODE" = normal-exit ]; then exit 0; fi
wait
`,
				driver: `require ARGV.fetch(0)
ENV["TEST_CHILD_PID"] = ARGV.fetch(1) + "/child.pid"
ENV["TEST_PARENT_PID"] = ARGV.fetch(1) + "/parent.pid"
ENV["TEST_CHILD_SCRIPT"] = ARGV.fetch(1) + "/child.sh"
ENV["TEST_PARENT_MODE"] = ARGV.fetch(3)
FixtureSystemCommand.configure(Pathname(ARGV.fetch(1)), seconds: 10)
begin
  FixtureSystemCommand.run!("/bin/sh", args: [ARGV.fetch(2)], timeout: 2)
  raise "timeout did not occur" if ARGV.fetch(3) == "timeout"
rescue Timeout::Error
  raise unless ARGV.fetch(3) == "timeout"
rescue Interrupt
  raise unless ARGV.fetch(3) == "outer-cancel"
end
puts "owned installer group cleaned"
`,
			}
			for name, body := range files {
				if err := os.WriteFile(name, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			pidFile, parentPIDFile := filepath.Join(fixture, "child.pid"), filepath.Join(fixture, "parent.pid")
			environment = append(environment, "TEST_CHILD_PID="+pidFile, "TEST_PARENT_PID="+parentPIDFile, "TEST_CHILD_SCRIPT="+child, "TEST_PARENT_MODE="+mode)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command, err := owner.start(ctx, environment, nil, brew, "ruby", "--", driver, loader, fixture, parent, mode)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "outer-cancel" {
				ready := false
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
					body, _ := os.ReadFile(pidFile)
					registry, _ := os.ReadFile(filepath.Join(fixture, "command-groups.json"))
					var record struct {
						PID int `json:"pid"`
					}
					if len(strings.Fields(string(body))) == 2 && json.Unmarshal(registry, &record) == nil && record.PID > 1 {
						ready = true
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !ready {
					t.Fatal("separate owned installer group did not become ready")
				}
				cancel()
			}
			output, err := command.wait()
			if (err != nil && mode != "outer-cancel") || !strings.Contains(string(output), "owned installer group cleaned") {
				t.Fatalf("command ownership regression failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(string(body))
			if len(fields) != 2 {
				t.Fatalf("grandchild did not publish PID and group: %q", body)
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil || pid <= 1 {
				t.Fatalf("invalid owned grandchild PID: %q", body)
			}
			group, err := strconv.Atoi(fields[1])
			parentBody, parentErr := os.ReadFile(parentPIDFile)
			if err != nil || parentErr != nil || strings.TrimSpace(string(parentBody)) != fields[1] || group == command.cmd.Process.Pid {
				t.Fatalf("fixture did not exercise Homebrew's separate installer group: child=%q parent=%q", body, parentBody)
			}
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
				err = syscall.Kill(pid, 0)
				if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
				t.Fatalf("owned grandchild %d survived command cleanup", pid)
			}
			// This assertion runs before fixture cleanup: no descendant can observe
			// tools being removed and fall through to the host's launchctl.
			if _, err := os.Stat(filepath.Join(fixture, "tools/launchctl")); err != nil {
				t.Fatalf("private tools were removed before descendant verification: %v", err)
			}
			body, err = os.ReadFile(filepath.Join(fixture, "command-groups.json"))
			var acknowledgement struct {
				Safe bool `json:"safe"`
			}
			if err != nil || json.Unmarshal(body, &acknowledgement) != nil || !acknowledgement.Safe {
				t.Fatalf("Ruby did not acknowledge complete group cleanup: %v %s", err, body)
			}
			if mode == "outer-cancel" {
				if !owner.retain {
					t.Fatal("outer cancellation must conservatively retain tools")
				}
				// This regression has separately proven that its exact child and
				// group exited before cleanup; its inert tools can now be removed.
				owner.retain = false
			}
		})
	}
}

func notarizedFixtureCask(t *testing.T, fixture, version string) string {
	t.Helper()
	build, commit := 123, strings.Repeat("a", 40)
	cask, err := RenderCask(backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: version, Commit: &commit, Build: &build}, Tag: "build-123-aaaaaaa", Asset: "Agent-Job-Queue-Installer-" + version + "-universal.zip", SHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture, "agent-job-queue.rb")
	if err := os.WriteFile(path, []byte(cask), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func notarizedCaskFixture(t *testing.T, team string) (string, string, []string, *notarizedCommandOwner) {
	t.Helper()
	// Keep Darwin's Unix socket path below 104 bytes, including the spaced HOME.
	fixture := filepath.Join("/private/tmp", "agent-queue-install-test-"+rand.Text()[:6])
	if err := os.Mkdir(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	owner := &notarizedCommandOwner{t: t}
	t.Cleanup(func() {
		if body, err := os.ReadFile(filepath.Join(fixture, "command-groups.json")); err == nil {
			var record struct {
				Safe bool `json:"safe"`
			}
			if json.Unmarshal(body, &record) != nil || !record.Safe {
				owner.retain = true
			}
		} else if !os.IsNotExist(err) {
			owner.retain = true
		}
		if owner.retain {
			t.Errorf("command cleanup was not proven; private fixture tools retained at %s", fixture)
			return
		}
		_ = os.RemoveAll(fixture)
	})
	home, tools := filepath.Join(fixture, "User With Spaces"), filepath.Join(fixture, "tools")
	for _, directory := range []string{home, tools} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(fixture, "calls")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	stub := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" >> "$TEST_CALLS"
case "$*" in
  'print %[1]s/io.went.agent-heavy'|'print %[1]s/io.went.agent-resources'|'print %[1]s/io.went.agent-remote') exit 1 ;;
  "bootstrap %[1]s $HOME/Library/LaunchAgents/io.went.agent-heavy.plist"|"bootstrap %[1]s $HOME/Library/LaunchAgents/io.went.agent-resources.plist"|'kickstart -k %[1]s/io.went.agent-resources') exit 0 ;;
esac
printf 'UNEXPECTED %%s\n' "$*" >> "$TEST_CALLS"
exit 97
`, domain)
	if err := os.WriteFile(filepath.Join(tools, "launchctl"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	protocol := map[string]string{"HOME": home, "AGENT_QUEUE_INSTALL_TEST_MODE": "1", "AGENT_QUEUE_INSTALL_TEST_TOOLS": tools, "AGENT_QUEUE_INSTALL_TEST_TEAM": team, "TEST_PYTHON_PREFIX": tools, "TEST_PYTHON": filepath.Join(tools, "unused-python"), "TEST_CALLS": calls}
	body, err := json.Marshal(protocol)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "fixture-environment.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	// Allow only public process settings. Queue overrides and signing credentials
	// never reach the private daemon, installer, Homebrew or its child processes.
	environment := []string{"HOME=" + home, "HOMEBREW_DEVELOPER=1", "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_FROM_API=1", "TEST_CALLS=" + calls}
	for _, key := range []string{"PATH", "TMPDIR", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	return fixture, home, environment, owner
}

// Every Go-owned command gets a private group. Homebrew creates additional
// groups itself; FixtureSystemCommand owns those and leaves a durable cleanup
// acknowledgement. Uncertain teardown retains the tools instead of exposing a
// surviving installer to the host PATH after fixture deletion.
type notarizedCommandOwner struct {
	t      *testing.T
	retain bool
}

type notarizedOwnedCommand struct {
	owner    *notarizedCommandOwner
	ctx      context.Context
	cmd      *exec.Cmd
	done     chan struct{}
	waitErr  error
	output   bytes.Buffer
	stopOnce sync.Once
	stopErr  error
}

func (owner *notarizedCommandOwner) start(ctx context.Context, environment []string, output io.Writer, name string, args ...string) (*notarizedOwnedCommand, error) {
	owned := &notarizedOwnedCommand{owner: owner, ctx: ctx, done: make(chan struct{})}
	command := exec.CommandContext(ctx, name, args...)
	owned.cmd = command
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Env = environment
	if output == nil {
		output = &owned.output
	}
	command.Stdout, command.Stderr = output, output
	command.WaitDelay = 4 * time.Second
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGINT)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	go func() { owned.waitErr = command.Wait(); close(owned.done) }()
	owner.t.Cleanup(func() {
		if err := owned.stop(); err != nil {
			owner.t.Error(err)
		}
	})
	return owned, nil
}

func (owned *notarizedOwnedCommand) wait() ([]byte, error) {
	<-owned.done
	cleanup := owned.stop()
	if owned.ctx.Err() != nil {
		owned.owner.retain = true
	}
	return owned.output.Bytes(), errors.Join(owned.waitErr, cleanup)
}

func (owned *notarizedOwnedCommand) stop() error {
	owned.stopOnce.Do(func() {
		pid := owned.cmd.Process.Pid
		alive := func() bool {
			err := syscall.Kill(-pid, 0)
			return !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM)
		}
		if alive() {
			if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
				owned.stopErr = err
			}
		}
		for deadline := time.Now().Add(200 * time.Millisecond); alive() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		if alive() {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
				owned.stopErr = err
			}
		}
		deadline := time.Now().Add(3 * time.Second)
		select {
		case <-owned.done:
		case <-time.After(time.Until(deadline)):
			owned.stopErr = errors.Join(owned.stopErr, errors.New("owned command was not reaped"))
		}
		for alive() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if alive() {
			owned.stopErr = errors.Join(owned.stopErr, fmt.Errorf("owned command group %d survived cleanup", pid))
		}
		if owned.stopErr != nil {
			owned.owner.retain = true
		}
	})
	return owned.stopErr
}
