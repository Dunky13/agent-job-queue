package tooling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

// Only the owned fixture protocol can enable the production private-native seam.
// Homebrew filters environment variables before Ruby starts, so the bounded
// fixture loader rehydrates this explicit value alongside its private HOME.
func enableManagedCaskFixture(t *testing.T, root string) {
	t.Helper()
	filename := filepath.Join(root, "fixture-environment.json")
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var environment map[string]string
	if err := json.Unmarshal(raw, &environment); err != nil {
		t.Fatal(err)
	}
	environment["AGENT_QUEUE_INSTALL_TEST_MANAGED"] = "private-native-lifecycle"
	raw, err = json.Marshal(environment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

type managedCaskReply struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

// This client is invoked only by private fixture tools. The socket and tool are
// literal wrapper arguments; Homebrew cannot lose them while filtering env.
func TestManagedCaskFixtureClient(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) < 2 || !filepath.IsAbs(args[0]) {
		os.Exit(97)
	}
	root := filepath.Dir(args[0])
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !strings.HasPrefix(filepath.Base(root), "agent-queue-install-test-") {
		os.Exit(97)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		os.Exit(97)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", args[0])
	}}
	defer transport.CloseIdleConnections()
	body, _ := json.Marshal(args[1:])
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture/command", bytes.NewReader(body))
	if err != nil {
		os.Exit(97)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(97)
	}
	var reply managedCaskReply
	err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&reply)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || reply.Code < 0 || reply.Code > 255 {
		os.Exit(97)
	}
	fmt.Print(reply.Output)
	os.Exit(reply.Code)
}

func proveManagedNativeFixture(t *testing.T, owner *notarizedCommandOwner, ctx context.Context, environment []string, root, app string, helpers ...string) {
	t.Helper()
	// The production preflight correctly rejects a bundle outside Applications.
	// Use a byte-preserving private copy for this read-only proof, then remove it
	// before Homebrew exercises its own app relocation.
	proofApp := filepath.Join(root, "User With Spaces/Applications/Agent Queue.app")
	if err := os.MkdirAll(filepath.Dir(proofApp), 0700); err != nil {
		t.Fatal(err)
	}
	copy, err := owner.start(ctx, environment, nil, "/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", app, proofApp)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := copy.wait(); err != nil {
		t.Fatalf("private preflight app copy failed: %v: %s", err, output)
	}
	for _, helper := range helpers {
		before, err := os.ReadFile(filepath.Join(root, "calls"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		probe, err := owner.start(ctx, environment, nil, helper, "install", "--app-bundle", proofApp, "--preflight", "--json")
		if err != nil {
			t.Fatal(err)
		}
		output, err := probe.wait()
		var report struct {
			Issues []json.RawMessage `json:"issues"`
		}
		if err != nil || json.Unmarshal(output, &report) != nil || report.Issues == nil || len(report.Issues) != 0 {
			t.Fatalf("private managed preflight for %s failed: %v: %s", helper, err, output)
		}
		after, err := os.ReadFile(filepath.Join(root, "calls"))
		if err != nil || strings.Count(string(after), "service-management|status|queue") <= strings.Count(string(before), "service-management|status|queue") || strings.Contains(string(after), "service-management|register|") {
			t.Fatalf("packaged helper %s lacks the proven private native-lifecycle seam; no installation attempted: %v", helper, err)
		}
	}
	if err := os.RemoveAll(proofApp); err != nil {
		t.Fatal(err)
	}
}

// The test parent owns daemon groups outside Homebrew's per-command groups.
// No code here invokes a real native service command or a real launchctl.
type managedCaskController struct {
	mu          sync.Mutex
	ctx         context.Context
	home, root  string
	environment []string
	owner       *notarizedCommandOwner
	statuses    map[string]string
	processes   map[string]*notarizedOwnedCommand
}

func newManagedCaskController(t *testing.T, root, home string, environment []string, owner *notarizedCommandOwner) *managedCaskController {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root || home != filepath.Join(root, "User With Spaces") || !strings.HasPrefix(filepath.Base(root), "agent-queue-install-test-") {
		t.Fatal("managed controller requires the canonical private installer fixture", err)
	}
	for _, path := range []string{root, home, filepath.Join(root, "tools")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatal("unsafe managed controller directory", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			t.Fatal("unowned managed controller directory", path)
		}
	}
	deadline := time.Now().Add(2 * time.Minute)
	if limit, ok := t.Deadline(); ok && limit.Add(-10*time.Second).Before(deadline) {
		deadline = limit.Add(-10 * time.Second)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	t.Cleanup(cancel)
	c := &managedCaskController{ctx: ctx, home: home, root: root, environment: environment, owner: owner,
		statuses:  map[string]string{"queue": "notRegistered", "resources": "notRegistered", "remote": "notRegistered", "login": "notRegistered"},
		processes: map[string]*notarizedOwnedCommand{}}
	socket := filepath.Join(root, "native.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args []string
		if r.Method != http.MethodPost || r.URL.Path != "/command" || json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&args) != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		reply := c.command(args)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, command := range c.processes {
			if err := command.stop(); err != nil {
				t.Error(err)
			}
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	for _, tool := range []string{"service-management", "launchctl"} {
		// Every query starts this test binary again. Keep race detection and
		// inherited options, but avoid paying its default one-second exit sleep
		// for every successful fixture reply within the acceptance deadline.
		wrapper := "#!/bin/sh\nGORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\"\nexport GORACE\nexec " + quote(executable) + " -test.run='^TestManagedCaskFixtureClient$' -- " + quote(socket) + " " + quote(tool) + " \"$@\"\n"
		verificationWrite(t, filepath.Join(root, "tools", tool), wrapper, 0700)
	}
	return c
}

func (c *managedCaskController) command(args []string) managedCaskReply {
	failure := func(err error) managedCaskReply { return managedCaskReply{Output: err.Error() + "\n", Code: 97} }
	if len(args) != 3 {
		return failure(errors.New("unsupported private lifecycle arguments"))
	}
	record, err := os.OpenFile(filepath.Join(c.root, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return failure(err)
	}
	_, err = fmt.Fprintln(record, strings.Join(args, "|"))
	err = errors.Join(err, record.Close())
	if err != nil {
		return failure(err)
	}
	app := filepath.Join(c.home, "Applications/Agent Queue.app")
	if args[0] == "service-management" && args[1] == "prepare-app-replacement" {
		if args[2] != app {
			return failure(errors.New("foreign app replacement target"))
		}
		info, err := os.Lstat(app)
		if err != nil || !info.IsDir() {
			return failure(errors.New("private app replacement target is missing"))
		}
		body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "app": app, "status": "stopped"})
		return managedCaskReply{Output: string(body) + "\n"}
	}
	labels := map[string]string{"queue": "io.went.agent-heavy", "resources": "io.went.agent-resources", "remote": "io.went.agent-remote"}
	if args[0] == "launchctl" && args[1] == "print" {
		if args[2] == fmt.Sprintf("gui/%d/io.went.agent-queue", os.Getuid()) {
			return managedCaskReply{Output: fmt.Sprintf("Bad request.\nCould not find service %q in domain for user gui: %d\n", "io.went.agent-queue", os.Getuid()), Code: 113}
		}
		for service, label := range labels {
			if args[2] != fmt.Sprintf("gui/%d/%s", os.Getuid(), label) {
				continue
			}
			if c.statuses[service] != "enabled" {
				return managedCaskReply{Output: fmt.Sprintf("Bad request.\nCould not find service %q in domain for user gui: %d\n", label, os.Getuid()), Code: 113}
			}
			process := c.processes[service]
			if process == nil {
				return failure(errors.New("enabled private service has no owned process"))
			}
			select {
			case <-process.done:
				return failure(errors.New("private canonical process exited before ownership readback"))
			default:
			}
			argument := "daemon"
			if service == "resources" {
				argument = "scheduler"
			}
			return managedCaskReply{Output: fmt.Sprintf("program = %s\narguments = {\nagent-heavy\n%s\n}\n", filepath.Join(app, "Contents/MacOS/agent-heavy"), argument)}
		}
		return failure(errors.New("foreign launchctl target"))
	}
	action, service := args[1], args[2]
	if args[0] != "service-management" || c.statuses[service] == "" {
		return failure(errors.New("foreign native target"))
	}
	switch action {
	case "status":
		body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "service": service, "status": c.statuses[service]})
		return managedCaskReply{Output: string(body) + "\n"}
	case "register":
		if service == "remote" {
			return failure(errors.New("remote registration is outside this fixture"))
		}
		if c.statuses[service] == "enabled" {
			return failure(errors.New("duplicate private registration"))
		}
		if service != "login" {
			helper := filepath.Join(app, "Contents/MacOS/agent-heavy")
			info, err := os.Lstat(helper)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return failure(errors.New("private canonical packaged helper is missing"))
			}
			log, err := os.Create(filepath.Join(c.root, service+".log"))
			if err != nil {
				return failure(err)
			}
			c.owner.t.Cleanup(func() { _ = log.Close() })
			argument := "daemon"
			if service == "resources" {
				argument = "scheduler"
			}
			command, err := c.owner.start(c.ctx, c.environment, log, helper, argument)
			if err != nil {
				return failure(err)
			}
			c.processes[service] = command
		}
		c.statuses[service] = "enabled"
	case "unregister":
		if command := c.processes[service]; command != nil {
			if err := command.stop(); err != nil {
				return failure(err)
			}
			delete(c.processes, service)
		}
		c.statuses[service] = "notRegistered"
	default:
		return failure(errors.New("unsupported private native action"))
	}
	return managedCaskReply{}
}

func TestManagedCaskPrivateNativeLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("private notarized installer fixture requires macOS")
	}
	root, home, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
	c := newManagedCaskController(t, root, home, environment, owner)
	// Under -race this delay exceeds each command's bound. Both generated
	// wrappers must remove only the exit sleep while retaining race detection.
	environment = append(environment, "GORACE=atexit_sleep_ms=30000")
	invoke := func(tool string, code int, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command, err := owner.start(ctx, environment, nil, filepath.Join(root, "tools", tool), args...)
		if err != nil {
			t.Fatal(err)
		}
		output, err := command.wait()
		actual := 0
		if err != nil {
			var exited *exec.ExitError
			if !errors.As(err, &exited) {
				t.Fatal(err)
			}
			actual = exited.ExitCode()
		}
		if actual != code {
			t.Fatalf("private %s %v exit=%d, want=%d: %s", tool, args, actual, code, output)
		}
		return string(output)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(invoke("service-management", 0, "status", "queue")), &status); err != nil || status.Status != "notRegistered" {
		t.Fatal("unexpected initial private native status", status, err)
	}
	invoke("launchctl", 113, "print", fmt.Sprintf("gui/%d/io.went.agent-heavy", os.Getuid()))
	invoke("launchctl", 113, "print", fmt.Sprintf("gui/%d/io.went.agent-queue", os.Getuid()))
	invoke("launchctl", 97, "print", "system/io.went.agent-heavy")
	invoke("launchctl", 97, "bootout", fmt.Sprintf("gui/%d/io.went.agent-heavy", os.Getuid()))
	invoke("service-management", 97, "register", "queue") // No canonical helper yet.
	invoke("service-management", 97, "register", "remote")
	invoke("service-management", 97, "register", "foreign")
	helper := filepath.Join(home, "Applications/Agent Queue.app/Contents/MacOS/agent-heavy")
	verificationWrite(t, helper, "#!/bin/sh\n[ \"$1\" = daemon ] || exit 98\nexec /bin/sleep 60\n", 0700)
	invoke("service-management", 97, "prepare-app-replacement", "/Applications/Agent Queue.app")
	invoke("service-management", 0, "prepare-app-replacement", filepath.Join(home, "Applications/Agent Queue.app"))
	invoke("service-management", 0, "register", "queue")
	invoke("service-management", 97, "register", "queue")
	loaded := invoke("launchctl", 0, "print", fmt.Sprintf("gui/%d/io.went.agent-heavy", os.Getuid()))
	if !strings.Contains(loaded, "program = "+helper+"\n") || !strings.Contains(loaded, "agent-heavy\ndaemon\n") {
		t.Fatal("private ownership readback lost canonical helper", loaded)
	}
	c.mu.Lock()
	process := c.processes["queue"]
	c.mu.Unlock()
	invoke("service-management", 0, "unregister", "queue")
	select {
	case <-process.done:
	default:
		t.Fatal("private native unregistration did not reap its process")
	}
	invoke("launchctl", 113, "print", fmt.Sprintf("gui/%d/io.went.agent-heavy", os.Getuid()))
}

// A supplied managed release must fail its trust/runtime gate when it cannot
// qualify. A successful subprocess containing SKIP would reproduce the old gap.
func TestNotarizedHomebrewManagedInputCannotSkipGate(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("notarized release gate requires macOS")
	}
	root, _, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
	payload := filepath.Join(root, "Agent Job Queue Installer.app/Contents/Resources/payload")
	verificationWrite(t, filepath.Join(payload, "Agent Queue.app/Contents/Info.plist"), `<?xml version="1.0"?><plist version="1.0"><dict><key>AgentQueueServiceManagement</key><true/></dict></plist>`, 0600)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment = append(environment, "AGENT_QUEUE_TEST_NOTARIZED_PAYLOAD="+payload, "APPLE_TEAM_ID=AAAAAAAAAA")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command, err := owner.start(ctx, environment, nil, executable, "-test.run=^TestNotarizedHomebrew$", "-test.v")
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.wait()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() == 0 || strings.Contains(string(output), "--- SKIP") || !strings.Contains(string(output), "bash failed") || !strings.Contains(string(output), "macOS signing:") {
		t.Fatalf("supplied managed payload did not fail its required trust gate: %v: %s", err, output)
	}
}

// Local packaged integration deliberately has no Developer ID/notarization
// claim. The separate TestNotarizedHomebrew retains every signed release gate.
func TestManagedInstallerPackagedNativeFixture(t *testing.T) {
	payload := os.Getenv("AGENT_QUEUE_TEST_INSTALLER_PAYLOAD")
	if payload == "" {
		t.Skip("packaged installer payload not supplied")
	}
	if runtime.GOOS != "darwin" || !filepath.IsAbs(payload) {
		t.Fatal("packaged installer fixture requires macOS and an absolute payload")
	}
	marked, err := managedApp(filepath.Join(payload, "Agent Queue.app"))
	if err != nil || !marked {
		t.Fatal("private managed acceptance requires the marked app", err)
	}
	root, home, environment, owner := notarizedCaskFixture(t, "AAAAAAAAAA")
	native := newManagedCaskController(t, root, home, environment, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run := func(name string, args ...string) []byte {
		t.Helper()
		command, err := owner.start(ctx, environment, nil, name, args...)
		if err != nil {
			t.Fatal(err)
		}
		output, err := command.wait()
		if err != nil {
			t.Fatalf("private packaged %s: %v: %s", name, err, output)
		}
		return output
	}
	installer := filepath.Dir(filepath.Dir(filepath.Dir(payload)))
	if filepath.Join(installer, "Contents/Resources/payload") != payload {
		t.Fatal("packaged payload must retain its enclosing installer bundle")
	}
	privateInstaller := filepath.Join(root, "Agent Job Queue Installer.app")
	run("/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", installer, privateInstaller)
	privatePayload := filepath.Join(privateInstaller, "Contents/Resources/payload")
	environment = append(environment, "AGENT_QUEUE_INSTALL_TEST_MODE=1", "AGENT_QUEUE_INSTALL_TEST_MANAGED=private-native-lifecycle", "AGENT_QUEUE_INSTALL_TEST_TOOLS="+filepath.Join(root, "tools"))
	app := filepath.Join(privatePayload, "Agent Queue.app")
	proveManagedNativeFixture(t, owner, ctx, environment, root, app,
		filepath.Join(app, "Contents/MacOS/agent-heavy"), filepath.Join(privatePayload, "source/bin/agent-heavy"))
	enableManagedCaskFixture(t, root)
	// This local artifact has ad hoc signatures. The fixture injects its trust
	// adapter while running the actual cask Installer and isolated real helpers.
	// The separate release acceptance above runs the real Apple verifiers.
	protocol := filepath.Join(root, "fixture-environment.json")
	rawProtocol, err := os.ReadFile(protocol)
	if err != nil {
		t.Fatal(err)
	}
	var protocolEnvironment map[string]string
	if err := json.Unmarshal(rawProtocol, &protocolEnvironment); err != nil {
		t.Fatal(err)
	}
	delete(protocolEnvironment, "AGENT_QUEUE_INSTALL_TEST_TEAM")
	rawProtocol, err = json.Marshal(protocolEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protocol, rawProtocol, 0600); err != nil {
		t.Fatal(err)
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(privatePayload, "source/build-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := backend.ValidateBuildInfo(metadata)
	if err != nil {
		t.Fatal(err)
	}
	cask := notarizedFixtureCask(t, root, identity.Version)
	output := run(brew, "ruby", "--", "../../packaging/homebrew/test-cask.rb", cask, root)
	var installed struct {
		Executed bool `json:"fixture_installer_executed"`
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &installed); err != nil || !installed.Executed {
		t.Fatalf("private packaged cask did not execute vendor installation: %v: %s", err, output)
	}
	cli := filepath.Join(home, ".local/bin/agent-heavy")
	run(cli, "status", "--json")
	native.mu.Lock()
	daemon, scheduler := native.processes["queue"], native.processes["resources"]
	native.mu.Unlock()
	if daemon == nil || scheduler == nil {
		t.Fatal("Homebrew vendor installer did not start both real packaged private services")
	}
	// The Homebrew installer artifact has ended. Its parent-owned services must
	// still accept a real job with exact arguments and exit status.
	job, err := owner.start(ctx, append(slices.Clone(environment), "PACKAGED_TEST_VALUE=fixture-env"), nil, cli,
		"run", "--label", "Private managed installer job", "--", "/bin/sh", "-c", `printf '%s|%s|%s\n' "$1" "$2" "$PACKAGED_TEST_VALUE"; exit 7`, "fixture", "argument with spaces", "literal;$HOME")
	if err != nil {
		t.Fatal(err)
	}
	output, err = job.wait()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 7 || !strings.Contains(string(output), "argument with spaces|literal;$HOME|fixture-env") {
		t.Fatalf("private installer packaged job contract failed: %v: %s", err, output)
	}
	sentinel := filepath.Join(home, ".config/agent-heavy/credentials.fixture")
	verificationWrite(t, sentinel, "private credential sentinel", 0600)
	// A scheduler tick can own this lock when removal is requested. Exercise
	// that refusal deliberately, then retry only a proven unaccepted operation.
	unlock, err := backend.LockPrivate(&backend.Gate{State: filepath.Join(home, ".local/state/agent-heavy")}, "scheduling.lock")
	if err != nil {
		t.Fatal(err)
	}
	var released sync.Once
	release := func() { released.Do(unlock) }
	defer release()
	removed := false
	for attempt := 0; attempt < 3; attempt++ {
		calls, err := os.ReadFile(filepath.Join(root, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		identity := filepath.Join(home, ".local/share/agent-heavy/installation.json")
		before, err := os.ReadFile(identity)
		if err != nil {
			t.Fatal(err)
		}
		wrappers := map[string][]byte{}
		for _, name := range []string{"agent-heavy", "agent-remote"} {
			path := filepath.Join(home, ".local/bin", name)
			wrappers[path], err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		}
		command, err := owner.start(ctx, environment, nil, cli, "install", "--app-bundle", filepath.Join(home, "Applications/Agent Queue.app"), "--unregister-services")
		if err != nil {
			t.Fatal(err)
		}
		output, err := command.wait()
		if err == nil {
			if attempt == 0 {
				t.Fatal("removal ignored the deliberately held scheduling lock")
			}
			removed = true
			break
		}
		var exited *exec.ExitError
		if !errors.As(err, &exited) || exited.ExitCode() != 1 || !strings.HasPrefix(strings.TrimSpace(string(output)), "Agent Queue installer: Another installer or queue change is running; finish it before trying again") {
			t.Fatalf("non-retryable private removal failed: %v: %s", err, output)
		}
		pending := filepath.Join(home, ".local/share/agent-heavy/install-in-progress.json")
		if _, err := os.Lstat(pending); !os.IsNotExist(err) {
			t.Fatal("busy removal created pending state; mutation cannot be retried", err)
		}
		after, err := os.ReadFile(identity)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("busy removal changed completed installation identity", err)
		}
		for path, before := range wrappers {
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("busy removal changed a command wrapper", path, err)
			}
		}
		afterCalls, err := os.ReadFile(filepath.Join(root, "calls"))
		if err != nil || !bytes.HasPrefix(afterCalls, calls) {
			t.Fatal("busy removal lost its native action witness", err)
		}
		delta := string(afterCalls[len(calls):])
		for _, mutation := range []string{"service-management|register|", "service-management|unregister|", "service-management|prepare-app-replacement|"} {
			if strings.Contains(delta, mutation) {
				t.Fatal("busy removal accepted a native mutation; refusal cannot be retried", delta)
			}
		}
		t.Logf("Proven unaccepted busy removal attempt %d: %s", attempt+1, strings.TrimSpace(string(output)))
		release()
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if !removed {
		t.Fatal("private removal remained busy within its original attempt/deadline bounds")
	}
	if _, err := os.Stat(filepath.Join(home, "Applications/Agent Queue.app/Contents/MacOS/AgentQueue")); err != nil {
		t.Fatal("explicit service removal unexpectedly deleted the app", err)
	}
	for _, path := range []string{backend.QueueDatabasePath(filepath.Join(home, ".local/state/agent-heavy")), sentinel} {
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("private installer removal lost private data: %s: %v: %v", path, info, err)
		}
	}
	retained, err := os.ReadFile(sentinel)
	if err != nil || string(retained) != "private credential sentinel" {
		t.Fatal("private installer removal changed retained credentials", err)
	}
	for _, command := range []*notarizedOwnedCommand{daemon, scheduler} {
		select {
		case <-command.done:
		default:
			t.Fatal("private installer removal did not stop and reap its real packaged services")
		}
	}
	for _, name := range []string{".local/bin/agent-heavy", ".local/bin/agent-remote", ".local/share/agent-heavy/installation.json"} {
		if _, err := os.Lstat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("private installer removal retained generated location/wrapper %s: %v", name, err)
		}
	}
	t.Log("Actual packaged helpers passed noninteractive Homebrew setup and private runtime acceptance; trust/native lifecycle were injected, actual Apple trust and OS registration remain unqualified")
}
