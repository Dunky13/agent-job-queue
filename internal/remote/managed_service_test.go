package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"howett.net/plist"
)

func TestManagedPhoneDeadlinesReachActualCommand(t *testing.T) {
	c := fixtureManagedController(t)
	lifecycle, cancel := defaultLifecycleContext()
	defer cancel()
	deadline, ok := lifecycle.Deadline()
	if !ok || time.Until(deadline) < 2*appservices.NativeCommandTimeout {
		t.Fatal("lifecycle cannot contain native stop and start")
	}
	// Account for earlier status and ownership work without sleeping. The real
	// command seam must still give unregister its complete native command bound.
	c.ctx, cancel = context.WithDeadline(lifecycle, deadline.Add(-20*time.Second))
	defer cancel()
	launcher := filepath.Join(c.p.managedApp, "Contents/MacOS/AgentQueue")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schemaVersion\":1,\"service\":\"remote\",\"status\":\"notRegistered\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"status", "unregister"} {
		var commandDeadline time.Time
		c.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			commandDeadline, _ = ctx.Deadline()
			if name != launcher || !slices.Equal(args, []string{"--service-management", action, "remote"}) {
				t.Fatal("deadline probe escaped owning native command", name, args)
			}
			return exec.CommandContext(ctx, name, args...)
		}
		started := time.Now()
		if state, err := c.managedServiceState(action); err != nil || state != "notRegistered" {
			t.Fatal("actual private native command failed", state, err)
		}
		want := appservices.NativeCommandTimeout
		if action == "status" {
			want = appservices.NativeStatusTimeout
		}
		if got := commandDeadline.Sub(started); got < want-time.Second || got > want+time.Second {
			t.Fatalf("parent lifecycle truncated %s command: %s, want %s", action, got, want)
		}
	}
	short, stop := context.WithTimeout(context.Background(), time.Millisecond)
	defer stop()
	c.ctx = short
	var actual time.Time
	c.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		actual, _ = ctx.Deadline()
		return exec.CommandContext(ctx, name, args...)
	}
	_, _ = c.managedServiceState("unregister")
	shortDeadline, _ := short.Deadline()
	if actual.IsZero() || actual.After(shortDeadline) {
		t.Fatal("native command extended caller cancellation", actual, shortDeadline)
	}
}

func fixtureManagedController(t *testing.T) *controller {
	t.Helper()
	c := fixtureController(t)
	canonicalHome, err := filepath.EvalSymlinks(c.p.home)
	if err != nil {
		t.Fatal(err)
	}
	c.p.home = canonicalHome
	c.p.managedApp = filepath.Join(c.p.home, "Applications/Agent Queue.app")
	write := func(name string, raw []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, raw, mode); err != nil {
			t.Fatal(err)
		}
	}
	info, err := plist.Marshal(map[string]any{"CFBundleIdentifier": "io.went.agent-queue", "AgentQueueServiceManagement": true}, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(c.p.managedApp, "Contents/Info.plist"), info, 0600)
	write(filepath.Join(c.p.managedApp, "Contents/Resources/payload/source/build-info.json"),
		[]byte(`{"schemaVersion":1,"repository":"Dunky13/agent-job-queue","version":"1.2.3","commit":null,"build":null}`), 0600)
	for _, name := range []string{"AgentQueue", "agent-heavy", "agent-remote"} {
		write(filepath.Join(c.p.managedApp, "Contents/MacOS", name), []byte("#!/bin/sh\nexit 99\n"), 0700)
	}
	for _, role := range []string{"heavy", "resources", "remote"} {
		helper := "agent-heavy"
		action := "daemon"
		if role == "remote" {
			helper = "agent-remote"
			action = "serve"
		} else if role == "resources" {
			action = "scheduler"
		}
		definition, err := plist.Marshal(map[string]any{"Label": "io.went.agent-" + role, "BundleProgram": "Contents/MacOS/" + helper,
			"ProgramArguments": []string{helper, action}}, plist.XMLFormat)
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(c.p.managedApp, "Contents/Library/LaunchAgents/io.went.agent-"+role+".plist"), definition, 0600)
	}
	return c
}

func TestManagedPhoneServiceUsesOwningBundleAndNoUserPlist(t *testing.T) {
	c := fixtureManagedController(t)
	state := "enabled"
	var actions []string
	c.managedCommand = func(args []string) ([]byte, error) {
		if len(args) != 4 || args[0] != filepath.Join(c.p.managedApp, "Contents/MacOS/AgentQueue") || args[1] != "--service-management" || args[3] != "remote" {
			t.Fatalf("unexpected native call: %v", args)
		}
		actions = append(actions, args[2])
		if args[2] == "register" {
			t.Fatal("managed phone access registered a Service Management job")
		} else if args[2] == "unregister" {
			state = "notRegistered"
		}
		return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: state})
	}
	if err := c.launchAgent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(c.p.plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed phone service wrote a user LaunchAgent: %v", err)
	}
	if err := c.serviceAction("start"); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("starting managed phone access called the native bridge: %v", actions)
	}
	// A registration left by an earlier version is retired once; afterwards
	// stopping has nothing to unregister.
	for range 2 {
		if err := c.serviceAction("stop"); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(actions, []string{"status", "unregister", "status"}) {
		t.Fatalf("unexpected lifecycle actions: %v", actions)
	}
}

func TestManagedPhoneStopRetiresRegistrationAwaitingApproval(t *testing.T) {
	c := fixtureManagedController(t)
	state := "requiresApproval"
	var actions []string
	c.managedCommand = func(args []string) ([]byte, error) {
		actions = append(actions, args[2])
		if args[2] == "unregister" {
			state = "notRegistered"
		}
		return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: state})
	}
	if err := c.serviceAction("stop"); err != nil {
		t.Fatal("a leftover registration awaiting approval blocked migration", err)
	}
	if !slices.Equal(actions, []string{"status", "unregister"}) {
		t.Fatalf("unexpected lifecycle actions: %v", actions)
	}
}

func TestManagedPhoneEnableIgnoresApprovalAndReturnsWithoutWaiting(t *testing.T) {
	c := fixtureManagedController(t)
	c.bootFn = func() (string, error) { return "fixture-boot", nil }
	settings := testSettings()
	settings.Enabled = false
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	state := "requiresApproval"
	var actions []string
	c.managedCommand = func(args []string) ([]byte, error) {
		actions = append(actions, args[2])
		switch args[2] {
		case "register":
			t.Fatal("enable registered a Service Management job")
		case "unregister":
			state = "notRegistered"
		}
		return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: state})
	}
	started := time.Now()
	if err := c.enable(); err != nil {
		t.Fatal("approval state of a leftover registration blocked enable", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("enable waited for the service to start: %s", elapsed)
	}
	actual, err := loadSettings(c.p)
	if err != nil || actual == nil || !actual.Enabled || actual.PasswordHash != settings.PasswordHash || actual.SecretKey != settings.SecretKey {
		t.Fatal("enable did not record the user's choice with credentials intact", actual, err)
	}
	if !slices.Equal(actions, []string{"status", "unregister"}) {
		t.Fatalf("unexpected native calls: %v", actions)
	}
	// Only an unmanaged install starts its own agent and waits for readiness.
	c.p.testing = false
	if c.waitsForServiceStart() {
		t.Fatal("a managed install waits for a service its app supervises")
	}
	c.p.managedApp = ""
	if !c.waitsForServiceStart() {
		t.Fatal("an unmanaged install no longer confirms its service started")
	}
}

// A terminal user who runs enable on a managed install must learn that the
// app, not this command, starts phone access. Only the text of the existing
// "error" field changes: the keys and every other value stay as they were.
func TestManagedPhoneEnableExplainsThatTheAppStartsIt(t *testing.T) {
	c := fixtureManagedController(t)
	c.bootFn = func() (string, error) { return "fixture-boot", nil }
	settings := testSettings()
	settings.Enabled = false
	if err := writePrivate(c.p.config, settings); err != nil {
		t.Fatal(err)
	}
	c.managedCommand = func(args []string) ([]byte, error) {
		return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: "notRegistered"})
	}
	if err := c.enable(); err != nil {
		t.Fatal(err)
	}
	status, err := c.status()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if status["error"] != remoteNotRunning {
		t.Fatalf("status of a stopped service changed: %v", status["error"])
	}
	c.explainManagedEnable(status)
	if !strings.Contains(fmt.Sprint(status["error"]), "starts when Agent Queue is running") {
		t.Fatalf("enable did not explain who starts phone access: %v", status["error"])
	}
	var before, after map[string]any
	explained, _ := json.Marshal(status)
	if err := json.Unmarshal(plain, &before); err != nil || json.Unmarshal(explained, &after) != nil {
		t.Fatal("status is not JSON", err)
	}
	if len(before) != len(after) {
		t.Fatalf("enable output keys changed: %v vs %v", before, after)
	}
	for key, value := range before {
		if _, ok := after[key]; !ok {
			t.Fatalf("enable output lost %s", key)
		}
		if key != "error" && fmt.Sprint(after[key]) != fmt.Sprint(value) {
			t.Fatalf("enable output changed %s: %v vs %v", key, value, after[key])
		}
	}
	// A running service, an unrelated failure and an unmanaged install are left alone.
	for name, change := range map[string]func(*controller, map[string]any){
		"running":   func(_ *controller, s map[string]any) { s["running"] = true; s["error"] = nil },
		"failure":   func(_ *controller, s map[string]any) { s["error"] = "Gateway could not start" },
		"unmanaged": func(c *controller, s map[string]any) { c.p.managedApp = "" },
	} {
		c.p.managedApp = filepath.Join(c.p.home, "Applications/Agent Queue.app")
		other := map[string]any{"running": false, "error": remoteNotRunning}
		change(c, other)
		want := fmt.Sprint(other["error"])
		c.explainManagedEnable(other)
		if fmt.Sprint(other["error"]) != want {
			t.Fatalf("%s: enable rewrote %q", name, want)
		}
	}
}

func TestManagedPhoneStartNeverRegisters(t *testing.T) {
	for _, state := range []string{"notRegistered", "notFound", "requiresApproval", "enabled"} {
		t.Run(state, func(t *testing.T) {
			c := fixtureManagedController(t)
			c.managedCommand = func(args []string) ([]byte, error) {
				t.Fatal("starting managed phone access reached the native bridge", args)
				return nil, nil
			}
			if err := c.managedServiceAction("start"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedPhoneEnableNotFoundStillRequiresCredentials(t *testing.T) {
	c := fixtureManagedController(t)
	c.managedCommand = func(args []string) ([]byte, error) {
		t.Fatal("unconfigured phone access reached the native bridge", args)
		return nil, nil
	}
	if err := c.enable(); err == nil || !strings.Contains(err.Error(), "Configure a password") {
		t.Fatal("lookup miss bypassed or obscured the credential requirement", err)
	}
}

func TestManagedPhoneConfiguredEnableAfterLookupMissNeverRegisters(t *testing.T) {
	for _, action := range []string{"enable", "migrate"} {
		t.Run(action, func(t *testing.T) {
			c := fixtureManagedController(t)
			c.bootFn = func() (string, error) { return "fixture-boot", nil }
			settings := testSettings()
			settings.Enabled = action == "migrate"
			if err := writePrivate(c.p.config, settings); err != nil {
				t.Fatal(err)
			}
			var actions []string
			c.managedCommand = func(args []string) ([]byte, error) {
				actions = append(actions, args[2])
				state := "notFound"
				if args[2] == "register" {
					state = "enabled"
				}
				return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: state})
			}
			absence := fmt.Sprintf("Bad request.\nCould not find service \"io.went.agent-remote\" in domain for user gui: %d\n", os.Getuid())
			c.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if name != "/bin/launchctl" || !slices.Equal(args, []string{"print", fmt.Sprintf("gui/%d/io.went.agent-remote", os.Getuid())}) {
					t.Fatal("absence probe escaped the owned service", name, args)
				}
				return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s' "$1" >&2; exit 113`, "fixture", absence)
			}
			var err error
			if action == "enable" {
				err = c.enable()
			} else {
				err = c.migrate()
			}
			if err != nil {
				t.Fatal("configured phone access did not complete after verified shutdown", err)
			}
			actual, err := loadSettings(c.p)
			if err != nil || actual == nil || !actual.Enabled || actual.PasswordHash != settings.PasswordHash || actual.SecretKey != settings.SecretKey {
				t.Fatal("phone recovery lost credentials or enabled intent", actual, err)
			}
			if !slices.Equal(actions, []string{"status"}) {
				t.Fatal("unexpected native lifecycle or a registration", actions)
			}
		})
	}
}

func TestManagedPhoneLookupMissRequiresExactAbsenceProof(t *testing.T) {
	expected := fmt.Sprintf("Bad request.\nCould not find service \"io.went.agent-remote\" in domain for user gui: %d\n", os.Getuid())
	for _, test := range []struct {
		name, output string
		code         int
	}{
		{"loaded", "program = unexpected\n", 0},
		{"permission", "Operation not permitted\n", 1},
		{"wrong-code", expected, 1},
		{"wrong-label", strings.ReplaceAll(expected, "io.went.agent-remote", "another.service"), 113},
		{"extra-error", expected + "Unexpected failure\n", 113},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := fixtureManagedController(t)
			c.managedCommand = func(args []string) ([]byte, error) {
				if args[2] != "status" {
					t.Fatal("uncertain shutdown invoked a native mutation")
				}
				return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: "notFound"})
			}
			c.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s' "$1" >&2; exit "$2"`, "fixture", test.output, fmt.Sprint(test.code))
			}
			if err := c.serviceAction("stop"); err == nil {
				t.Fatal("lookup miss bypassed independent loaded-service/uncertainty checks")
			}
		})
	}
}

func TestManagedPhoneAbsentJobStillRequiresOwnedCleanup(t *testing.T) {
	c := fixtureManagedController(t)
	c.bootFn = func() (string, error) { return "fixture-boot", nil }
	c.managedCommand = func(args []string) ([]byte, error) {
		if args[2] != "status" {
			t.Fatal("shutdown attempted registration")
		}
		return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: "notFound"})
	}
	absence := fmt.Sprintf("Bad request.\nCould not find service \"io.went.agent-remote\" in domain for user gui: %d\n", os.Getuid())
	c.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s' "$1" >&2; exit 113`, "fixture", absence)
	}
	reconciled := false
	c.reconcileFn = func(string) error {
		reconciled = true
		return errors.New("retained child ownership is unresolved")
	}
	if err := c.stopOwnedService(); err == nil || !strings.Contains(err.Error(), "retained child ownership is unresolved") || !reconciled {
		t.Fatal("absent launch job bypassed owned process cleanup", err, reconciled)
	}
}

func TestManagedPhoneAbsenceCannotOutliveCancellation(t *testing.T) {
	c := fixtureManagedController(t)
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	cancel()
	absence := fmt.Sprintf("Bad request.\nCould not find service \"io.went.agent-remote\" in domain for user gui: %d\n", os.Getuid())
	c.newCommand = func(context.Context, string, ...string) *exec.Cmd {
		// Deliver a matching observation after cancellation to verify that it
		// cannot authorize shutdown, even if the command already completed.
		return exec.Command("/bin/sh", "-c", `printf '%s' "$1" >&2; exit 113`, "fixture", absence)
	}
	if err := c.managedServiceAbsent(); !errors.Is(err, context.Canceled) {
		t.Fatal("late absence observation bypassed cancellation", err)
	}
}

func TestManagedPhoneServiceFailsClosedOnNativeErrors(t *testing.T) {
	for _, response := range []string{`{}`, `{"schemaVersion":1,"service":"queue","status":"enabled"}`, `{"schemaVersion":1,"service":"remote","status":"enabled","status":"notRegistered"}`} {
		t.Run(response, func(t *testing.T) {
			c := fixtureManagedController(t)
			c.managedCommand = func([]string) ([]byte, error) { return []byte(response), nil }
			if err := c.serviceAction("stop"); err == nil {
				t.Fatal("invalid native status accepted")
			}
			if _, err := os.Lstat(c.p.plist); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid native status fell back to a user plist")
			}
		})
	}
	c := fixtureManagedController(t)
	witness := errors.New("native service unavailable")
	c.managedCommand = func([]string) ([]byte, error) { return nil, witness }
	if err := c.serviceAction("stop"); !errors.Is(err, witness) {
		t.Fatalf("native service error was lost: %v", err)
	}
}

func TestManagedPhoneUnfinishedRemovalIsNotSuccess(t *testing.T) {
	c := fixtureManagedController(t)
	c.managedCommand = func(args []string) ([]byte, error) {
		state := "enabled"
		if args[2] == "unregister" {
			state = "requiresApproval"
		}
		return json.Marshal(appservices.Status{SchemaVersion: 1, Service: "remote", Status: state})
	}
	if err := c.serviceAction("stop"); err == nil || !strings.Contains(err.Error(), "Login Items") {
		t.Fatalf("unfinished removal reported success: %v", err)
	}
}

func TestManagedPhoneIncompleteInstallCannotEnableLegacyService(t *testing.T) {
	c := fixtureController(t)
	c.p.managedCandidate = filepath.Join(c.p.home, "Applications/Agent Queue.app")
	if err := c.enable(); err == nil {
		t.Fatal("uninstalled managed helper enabled a legacy service")
	}
	if _, err := os.Lstat(c.p.config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete installation changed phone configuration: %v", err)
	}
}

func TestManagedPhonePendingInstallBlocksOrdinaryEnableBeforeMutation(t *testing.T) {
	c := fixtureManagedController(t)
	pending := filepath.Join(c.p.home, ".local/share/agent-heavy/install-in-progress.json")
	if err := os.MkdirAll(filepath.Dir(pending), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, []byte(`{"build":42}`), 0600); err != nil {
		t.Fatal(err)
	}
	c.managedCommand = func([]string) ([]byte, error) {
		t.Fatal("pending installation reached the native service bridge")
		return nil, nil
	}
	if err := c.enable(); err == nil || !strings.Contains(err.Error(), "Installation is incomplete") {
		t.Fatalf("pending install did not block ordinary enable: %v", err)
	}
	if _, err := os.Lstat(c.p.config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending installation changed phone access intent: %v", err)
	}
}
