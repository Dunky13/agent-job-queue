package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func hookManifest(plugin, lane string, events ...string) string {
	hooks := []string{}
	for _, event := range events {
		hooks = append(hooks, fmt.Sprintf("%q:[\"hooks/record\"]", event))
	}
	return fmt.Sprintf(`{"schemaVersion":1,"name":%q,"lanes":[{"name":%q,"concurrency":1}],"hooks":{%s}}`, plugin, lane, strings.Join(hooks, ","))
}

// recordHook writes each event and its environment to a private directory
// outside the plugin, named by event and process ID.
func recordHook(output string) string {
	return fmt.Sprintf("#!/bin/sh\nmkdir -p %q\ncat > %q/\"$AGENT_QUEUE_EVENT.$$.json\"\nenv > %q/\"$AGENT_QUEUE_EVENT.$$.env\"\n", output, output, output)
}

func recordedEvents(t *testing.T, output string) map[string]Object {
	t.Helper()
	events := map[string]Object{}
	paths, _ := filepath.Glob(filepath.Join(output, "*.json"))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var event Object
		if err = json.Unmarshal(body, &event); err != nil {
			t.Fatalf("hook stdin %q: %v", body, err)
		}
		events[text(event["event"])] = event
		environment, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".env")
		if err != nil {
			t.Fatal(err)
		}
		event["environment"] = string(environment)
	}
	return events
}

func TestHooksReceiveOnlyTheirLanesEventsWithMinimalEnvironment(t *testing.T) {
	outputs := t.TempDir()
	d, _ := laneDaemonWith(t, 8, func(g *Gate) {
		pluginFixture(t, g, "alpha", hookManifest("alpha", "a", pluginEvents...), map[string]string{"hooks/record": recordHook(filepath.Join(outputs, "alpha"))})
		pluginFixture(t, g, "beta", hookManifest("beta", "b", pluginEvents...), map[string]string{"hooks/record": recordHook(filepath.Join(outputs, "beta"))})
	})
	t.Setenv("AGENT_HEAVY_SECRET_FIXTURE", "must-not-leak")
	lease := acquireTestLease(t, d, "a", Object{"label": "QA", "meta": Object{"ticket": "WM-1"}})
	id, _ := integer(lease["id"])
	if _, err := d.control("lease-release", Object{"id": id, "ownerPid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	d.hooks.wait()
	events := recordedEvents(t, filepath.Join(outputs, "alpha"))
	if len(events) != 3 || events["lease.queued"] == nil || events["lease.granted"] == nil || events["lease.released"] == nil {
		t.Fatalf("alpha events: %v", events)
	}
	granted := events["lease.granted"]
	if granted["schemaVersion"] != float64(1) || granted["lane"] != "a" || granted["kind"] != "lease" || granted["label"] != "QA" || obj(granted["meta"])["ticket"] != "WM-1" || granted["ownerPid"] != float64(os.Getpid()) || granted["time"] == nil || granted["id"] != float64(id) {
		t.Fatalf("event payload: %v", granted)
	}
	if _, present := granted["reason"]; present {
		t.Fatal("absent fields must be omitted")
	}
	if events["lease.released"]["reason"] != "released" {
		t.Fatalf("release reason: %v", events["lease.released"])
	}
	environment := text(granted["environment"])
	for _, expected := range []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "AGENT_QUEUE_EVENT=lease.granted", "AGENT_QUEUE_PLUGIN=alpha", "HOME="} {
		if !strings.Contains(environment, expected) {
			t.Fatalf("hook environment lacks %s: %q", expected, environment)
		}
	}
	if strings.Contains(environment, "AGENT_HEAVY") || strings.Contains(environment, "must-not-leak") {
		t.Fatalf("hook inherited the daemon environment: %q", environment)
	}
	if beta := recordedEvents(t, filepath.Join(outputs, "beta")); len(beta) != 0 {
		t.Fatalf("beta received another plugin's events: %v", beta)
	}
	log, err := os.ReadFile(filepath.Join(d.g.State, "plugins", "alpha", "hooks.log"))
	if err != nil || strings.Count(string(log), "exit 0") != 3 {
		t.Fatalf("hook log: %q %v", log, err)
	}
	if info, err := os.Stat(filepath.Join(d.g.State, "plugins", "alpha", "hooks.log")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("hook log is not private: %v %v", info, err)
	}
}

func hookRunnerFixture(t *testing.T, script string) (*hookRunner, queuePlugin) {
	t.Helper()
	g := testGate(t)
	pluginFixture(t, g, "fixture", hookManifest("fixture", "fixture", "lease.granted"), map[string]string{"hooks/record": script})
	registry := loadPluginRegistry(g)
	plugin := pluginByName(registry, "fixture")
	if !plugin.Loaded || len(plugin.Hooks["lease.granted"]) != 1 {
		t.Fatalf("fixture plugin: %+v", plugin)
	}
	return newHookRunner(g), plugin
}

func hookLog(t *testing.T, r *hookRunner) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(r.g.State, "plugins", "fixture", "hooks.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func processGone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

func TestHookTimeoutTerminatesProcessGroup(t *testing.T) {
	output := t.TempDir()
	r, plugin := hookRunnerFixture(t, fmt.Sprintf("#!/bin/sh\nsleep 60 &\necho $! > %q/child\nexec sleep 60\n", output))
	started := time.Now()
	r.dispatch(plugin, "lease.granted", Object{"event": "lease.granted"})
	r.wait()
	if elapsed := time.Since(started); elapsed < hookTimeout || elapsed > hookTimeout+5*time.Second {
		t.Fatalf("hook ran for %v", elapsed)
	}
	body, err := os.ReadFile(filepath.Join(output, "child"))
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	fixtureUntil(t, 2*time.Second, func() bool { return processGone(child) })
	if log := hookLog(t, r); !strings.Contains(log, "timed out after 10 seconds") {
		t.Fatalf("timeout not logged: %q", log)
	}
}

func TestHookBackgroundChildCannotOutliveTimeout(t *testing.T) {
	output := t.TempDir()
	r, plugin := hookRunnerFixture(t, fmt.Sprintf("#!/bin/sh\nsleep 60 &\necho $! > %q/child\nexit 0\n", output))
	r.dispatch(plugin, "lease.granted", Object{"event": "lease.granted"})
	fixtureUntil(t, 5*time.Second, func() bool { return acceptanceFileExists(filepath.Join(output, "child")) })
	body, err := os.ReadFile(filepath.Join(output, "child"))
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	r.wait()
	fixtureUntil(t, 2*time.Second, func() bool { return processGone(child) })
	if log := hookLog(t, r); !strings.Contains(log, "exit 0") {
		t.Fatalf("hook exit not logged: %q", log)
	}
}

func TestHookOutputIsBoundedAndRotated(t *testing.T) {
	r, plugin := hookRunnerFixture(t, "#!/bin/sh\nhead -c 300000 /dev/zero | tr '\\000' x\necho tail >&2\n")
	directory := filepath.Join(r.g.State, "plugins", "fixture")
	if err := privateDirectory(filepath.Join(r.g.State, "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := privateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "hooks.log"), []byte(strings.Repeat("old\n", hookLogLimit/4)), 0o600); err != nil {
		t.Fatal(err)
	}
	r.dispatch(plugin, "lease.granted", Object{"event": "lease.granted"})
	r.wait()
	log := hookLog(t, r)
	if !strings.Contains(log, "(output truncated to 64 KiB)") || len(log) > hookOutputLimit+1024 || strings.Contains(log, "old\n") {
		t.Fatalf("hook log was not bounded and rotated: %d bytes", len(log))
	}
	if rotated, err := os.ReadFile(filepath.Join(directory, "hooks.log.1")); err != nil || !strings.HasPrefix(string(rotated), "old\n") {
		t.Fatalf("previous log generation: %v", err)
	}
}

func TestHookSafetyIsCheckedAgainBeforeRun(t *testing.T) {
	output := t.TempDir()
	r, plugin := hookRunnerFixture(t, fmt.Sprintf("#!/bin/sh\ntouch %q/ran\n", output))
	if err := os.Chmod(filepath.Join(plugin.Dir, "hooks", "record"), 0o777); err != nil {
		t.Fatal(err)
	}
	r.dispatch(plugin, "lease.granted", Object{"event": "lease.granted"})
	r.wait()
	if acceptanceFileExists(filepath.Join(output, "ran")) {
		t.Fatal("a hook that became writable by others was run")
	}
	if log := hookLog(t, r); !strings.Contains(log, "skipped") {
		t.Fatalf("refusal not logged: %q", log)
	}
	if err := os.Chmod(filepath.Join(plugin.Dir, "hooks", "record"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plugin.Dir, 0o777); err != nil {
		t.Fatal(err)
	}
	r.dispatch(plugin, "lease.granted", Object{"event": "lease.granted"})
	r.wait()
	if acceptanceFileExists(filepath.Join(output, "ran")) {
		t.Fatal("a hook below a world-writable plugin directory was run")
	}
}

func TestHookConcurrencyCapDropsAndReportsEvents(t *testing.T) {
	d, _ := laneDaemonWith(t, 8, func(g *Gate) {
		pluginFixture(t, g, "busy", hookManifest("busy", "busy", "lease.queued"), map[string]string{"hooks/record": "#!/bin/sh\nsleep 2\n"})
	})
	for range hookConcurrency + 2 {
		acquireTestLease(t, d, "busy", nil)
	}
	if dropped := d.hooks.dropped("busy"); dropped != 2 {
		t.Fatalf("dropped %d events, want 2", dropped)
	}
	snapshot, err := d.control("snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	plugins := snapshot["plugins"].([]Object)
	if len(plugins) != 1 || !strings.Contains(text(plugins[0]["warning"]), "2 hook events were dropped") {
		t.Fatalf("drops not reported: %v", plugins)
	}
	d.hooks.wait()
}
