package tooling

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

// This test is deliberately opt-in: an ad hoc PR build cannot establish the
// quarantined release's Developer ID trust or notarized Gatekeeper behavior.
func TestNotarizedHomebrew(t *testing.T) {
	supplied := os.Getenv("AGENT_QUEUE_TEST_NOTARIZED_APP")
	if supplied == "" {
		t.Skip("final notarized release app not supplied")
	}
	team := os.Getenv("APPLE_TEAM_ID")
	if runtime.GOOS != "darwin" || !filepath.IsAbs(supplied) || !regexp.MustCompile(`^[A-Z0-9]{10}$`).MatchString(team) {
		t.Fatal("notarized runtime requires macOS, an absolute app path and APPLE_TEAM_ID")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal(err)
	}
	app, err := filepath.EvalSymlinks(supplied)
	if err != nil || filepath.Base(app) != "Agent Queue.app" {
		t.Fatalf("the supplied path must be the extracted Agent Queue.app: %v", err)
	}
	version, build := bundleIdentity(t, app)
	fixture, home, environment := caskFixture(t)
	copyBundle(t, environment, app, filepath.Join(fixture, "Agent Queue.app"))
	cask := fixtureCask(t, fixture, version, build)
	output, err := runBrewRuby(t, 2*time.Minute, environment, brew, "--", "../../packaging/homebrew/test-cask.rb", cask, fixture, "--quarantined-runtime", team)
	if err != nil {
		t.Fatalf("real cask did not install the notarized app with quarantine retained: %v\n%s", err, output)
	}
	assertFixtureDelivery(t, output)
	// Homebrew only places the app; setup is the app's job on first launch.
	for _, name := range []string{".local", "Library/LaunchAgents"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("Homebrew installation performed app setup: %s (%v)", name, err)
		}
	}
	t.Logf("Real quarantined Homebrew app delivery completed for %s (%d)", version, build)
}

func TestNotarizedHomebrewRejectsUnsignedApp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew runtime verification requires macOS")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Skip("Homebrew is unavailable")
	}
	fixture, home, environment := caskFixture(t)
	// Executing any unsigned helper would leave observable evidence.
	writeFixtureApp(t, filepath.Join(fixture, "Agent Queue.app"), 123)
	cask := fixtureCask(t, fixture, "0.1.0", 123)
	output, err := runBrewRuby(t, 30*time.Second, environment, brew, "--", "../../packaging/homebrew/test-cask.rb", cask, fixture, "--quarantined-runtime", "AAAAAAAAAA")
	if err == nil || !strings.Contains(string(output), "failed Developer ID/notarization verification") {
		t.Fatalf("unsigned app was not rejected by the trust check: %v\n%s", err, output)
	}
	for _, name := range []string{filepath.Join(home, "forbidden-execution"), filepath.Join(home, "Applications"), filepath.Join(fixture, "Caskroom")} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("unsigned rejection executed or staged content: %s (%v)", filepath.Base(name), err)
		}
	}
}

// A genuinely notarized app that lost a helper no longer matches its seal, so
// it must fail before anything is staged or moved.
func TestNotarizedHomebrewRejectsIncompleteApp(t *testing.T) {
	supplied := os.Getenv("AGENT_QUEUE_TEST_NOTARIZED_APP")
	if supplied == "" {
		t.Skip("final notarized release app not supplied")
	}
	team := os.Getenv("APPLE_TEAM_ID")
	if runtime.GOOS != "darwin" || !filepath.IsAbs(supplied) || !regexp.MustCompile(`^[A-Z0-9]{10}$`).MatchString(team) {
		t.Fatal("notarized runtime requires macOS, an absolute app path and APPLE_TEAM_ID")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal(err)
	}
	app, err := filepath.EvalSymlinks(supplied)
	if err != nil {
		t.Fatal(err)
	}
	version, build := bundleIdentity(t, app)
	fixture, home, environment := caskFixture(t)
	incomplete := filepath.Join(fixture, "Agent Queue.app")
	copyBundle(t, environment, app, incomplete)
	if err := os.Remove(filepath.Join(incomplete, "Contents/MacOS/agent-remote")); err != nil {
		t.Fatal(err)
	}
	cask := fixtureCask(t, fixture, version, build)
	output, err := runBrewRuby(t, 30*time.Second, environment, brew, "--", "../../packaging/homebrew/test-cask.rb", cask, fixture, "--quarantined-runtime", team)
	if err == nil || !strings.Contains(string(output), "failed Developer ID/notarization verification") {
		t.Fatalf("incomplete app was not rejected: %v\n%s", err, output)
	}
	for _, name := range []string{filepath.Join(home, "Applications"), filepath.Join(fixture, "Caskroom")} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("incomplete app rejection staged content: %s (%v)", filepath.Base(name), err)
		}
	}
}

// bundleIdentity reads the version and build the cask must advertise.
func bundleIdentity(t *testing.T, app string) (string, int) {
	t.Helper()
	read := func(key string) string {
		output, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", filepath.Join(app, "Contents/Info.plist")).Output()
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return strings.TrimSpace(string(output))
	}
	build, err := strconv.Atoi(read("CFBundleVersion"))
	if err != nil {
		t.Fatal(err)
	}
	return read("CFBundleShortVersionString"), build
}

func copyBundle(t *testing.T, environment []string, source, destination string) {
	t.Helper()
	// Keep quarantine, signatures and any stapled ticket intact.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", source, destination)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("copy app: %v\n%s", err, output)
	}
}

func assertFixtureDelivery(t *testing.T, output []byte) {
	t.Helper()
	var result struct {
		AppInstalled bool           `json:"fixture_app_installed"`
		Attrs        map[string]int `json:"fixture_attributes"`
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil || !result.AppInstalled {
		t.Fatalf("real cask did not move its app: %v\n%s", err, output)
	}
	if result.Attrs["entries"] == 0 || len(result.Attrs) != 5 {
		t.Fatalf("missing staged attribute evidence: %v", result.Attrs)
	}
	for name, count := range result.Attrs {
		if count != result.Attrs["entries"] {
			t.Fatalf("staged %s was not retained: %v", name, result.Attrs)
		}
	}
}

func fixtureCask(t *testing.T, fixture, version string, build int) string {
	t.Helper()
	commit := strings.Repeat("a", 40)
	cask, err := RenderCask(backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: version, Commit: &commit, Build: &build}, Tag: fmt.Sprintf("build-%d-%.7s", build, commit), Asset: "Agent-Queue-" + version + "-universal.zip", SHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture, "agent-job-queue.rb")
	if err := os.WriteFile(path, []byte(cask), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// caskFixture creates an owned private directory with a spaced HOME. Homebrew
// runs with only public process settings, so no queue override or signing
// credential reaches it or the app it moves.
func caskFixture(t *testing.T) (string, string, []string) {
	t.Helper()
	fixture, err := os.MkdirTemp("/private/tmp", "agent-queue-cask-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("private cask failure evidence retained at %s", fixture)
			return
		}
		_ = os.RemoveAll(fixture)
	})
	home := filepath.Join(fixture, "User With Spaces")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	// Each fixture has a fresh HOME and runs brew once, so a cold Bootsnap cache
	// would only consume the command budget without a later reuse.
	environment := []string{"HOME=" + home, "HOMEBREW_DEVELOPER=1", "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_FROM_API=1", "HOMEBREW_NO_BOOTSNAP=1"}
	for _, key := range []string{"PATH", "TMPDIR", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	return fixture, home, environment
}

// runBrewRuby bounds Homebrew and its children by an explicit deadline.
func runBrewRuby(t *testing.T, timeout time.Duration, environment []string, brew string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, brew, append([]string{"ruby"}, args...)...)
	command.Env = environment
	command.WaitDelay = 4 * time.Second
	return command.CombinedOutput()
}

// writeFixtureApp builds a data-only Agent Queue.app. Every executable records
// forbidden-execution, because Homebrew must never run the app it delivers.
func writeFixtureApp(t *testing.T, app string, build int) {
	t.Helper()
	sentinel := "#!/bin/sh\ntouch \"$HOME/forbidden-execution\"\nexit 95\n"
	files := map[string]struct {
		body string
		mode os.FileMode
	}{
		"Contents/Info.plist":                                         {fmt.Sprintf(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>io.went.agent-queue</string><key>CFBundleShortVersionString</key><string>0.1.0</string><key>CFBundleVersion</key><string>%d</string></dict></plist>`, build), 0600},
		"Contents/MacOS/AgentQueue":                                   {sentinel, 0700},
		"Contents/MacOS/agent-heavy":                                  {sentinel, 0700},
		"Contents/MacOS/agent-remote":                                 {sentinel, 0700},
		"Contents/Library/LaunchAgents/io.went.agent-heavy.plist":     {"<plist/>", 0600},
		"Contents/Library/LaunchAgents/io.went.agent-resources.plist": {"<plist/>", 0600},
		"Contents/Library/LaunchAgents/io.went.agent-remote.plist":    {"<plist/>", 0600},
	}
	for name, file := range files {
		path := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.body), file.mode); err != nil {
			t.Fatal(err)
		}
	}
}
