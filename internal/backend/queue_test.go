package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Private queue fixtures execute this test binary as their controller. This
	// exercises the real CLI and worker implementation without a nested build.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon", "pause", "resume", "execute-attempt", "execute", "cleanup", "run", "status", "cancel", "limit", "next", "last", "priority", "start", "scheduler", "scheduling", "profiles", "agents", "updates", "version", "install":
			os.Exit(Main(os.Args[1:]))
		}
	}
	syscall.Umask(0o077)
	os.Exit(m.Run())
}
func testGate(t *testing.T) *Gate {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENT_HEAVY_STATE", filepath.Join(root, "state"))
	t.Setenv("AGENT_HEAVY_CONFIG", filepath.Join(root, "config.yml"))
	t.Setenv("AGENT_HEAVY_LEGACY_SLOT", filepath.Join(root, "slot"))
	t.Setenv("AGENT_HEAVY_ACTIVE", "")
	g, err := NewGate()
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Prepare(); err != nil {
		t.Fatal(err)
	}
	return g
}
func mustWrite(t *testing.T, path string, value Object) {
	t.Helper()
	if err := WritePrivate(path, value); err != nil {
		t.Fatal(err)
	}
}
func fakeQueue(t *testing.T, g *Gate, snapshot Object) {
	t.Helper()
	g.queueControl = func(operation string, payload Object) (Object, error) {
		if operation != "snapshot" {
			return Object{}, nil
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			return nil, err
		}
		return decodeObject(data)
	}
}
func testSnapshot(tasks Object) Object {
	return Object{"tasks": tasks, "groups": Object{"default": Object{"status": "Running", "parallel_tasks": 1}}}
}
func marker(id int) Object {
	return Object{"task_id": id, "token": fmt.Sprintf("%032x", id+1), "pgid": 2_000_000_000 + id, "pid": 2_000_000_000 + id, "boot_uuid": "fixture-boot", "initial_members": []Object{{"pid": 2_000_000_000 + id, "birth": "fixture-birth"}}, "members": []Object{{"pid": 2_000_000_000 + id, "birth": "fixture-birth"}}}
}

func TestResultsRejectUnknownShapes(t *testing.T) {
	for _, test := range []struct {
		value any
		code  int
		valid bool
	}{{"Success", 0, true}, {"Killed", 137, true}, {"Errored", 125, true}, {"DependencyFailed", 125, true}, {Object{"Failed": 7}, 7, true}, {Object{"Failed": json.Number("255")}, 255, true}, {Object{"FailedToSpawn": "no such file"}, 125, true}, {Object{"Failed": 0}, 0, false}, {Object{"Failed": 256}, 0, false}, {Object{"Failed": true}, 0, false}, {Object{"Failed": 7, "extra": 1}, 0, false}, {nil, 0, false}, {"Other", 0, false}} {
		code, err := ResultCode(test.value)
		if (err == nil) != test.valid || (test.valid && code != test.code) {
			t.Errorf("%v: %d %v", test.value, code, err)
		}
	}
}
func TestPrivateFilesRejectUnsafeInputs(t *testing.T) {
	g := testGate(t)
	path := filepath.Join(g.State, "private.json")
	mustWrite(t, path, Object{"large": json.Number("9007199254740993")})
	value, err := ReadPrivate(path)
	if err != nil || value["large"] != json.Number("9007199254740993") {
		t.Fatalf("numeric precision: %v %v", value, err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadPrivate(path); err == nil {
		t.Fatal("read public file")
	}
	if err = WritePrivate(path, Object{}); err == nil {
		t.Fatal("replaced unsafe file")
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(g.State, "symlink")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadPrivate(link); err == nil {
		t.Fatal("followed symlink")
	}
	if err = WritePrivate(link, Object{}); err == nil {
		t.Fatal("replaced symlink")
	}
	for _, body := range []string{"null", "[]", "true", "{bad}", "{} {}"} {
		if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err = ReadPrivate(path); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadPrivate(link); err == nil {
		t.Fatal("accepted dangling symlink")
	}
}
func TestRetainedLeaseConfiguration(t *testing.T) {
	g := testGate(t)
	os.Unsetenv("AGENT_HEAVY_LEGACY_SLOT")
	path := filepath.Join(g.State, "legacy-slot.json")
	for _, value := range []Object{{}, {"slot": "relative"}, {"slot": nil}, {"slot": ""}, {"slot": "/fixture\x00"}, {"slot": "/fixture", "extra": true}} {
		mustWrite(t, path, value)
		if _, err := LeaseSlot(g.State); err == nil {
			t.Errorf("accepted %v", value)
		}
	}
	retained := filepath.Join(t.TempDir(), "old.slot")
	mustWrite(t, path, Object{"slot": retained})
	if value, err := LeaseSlot(g.State); err != nil || value != retained {
		t.Fatalf("%s %v", value, err)
	}
	t.Setenv("AGENT_HEAVY_LEGACY_SLOT", g.Slot)
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := LeaseSlot(g.State); err != nil || value != g.Slot {
		t.Fatal("override must precede retained setting", err)
	}
}
func TestPrivateLockRejectsSymlinkAndHardlink(t *testing.T) {
	g := testGate(t)
	target := filepath.Join(g.State, "target")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.State, "lease.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if unlock, err := LockPrivate(g, ""); err == nil {
		unlock()
		t.Fatal("accepted symlink")
	}
	os.Remove(path)
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}
	if unlock, err := LockPrivate(g, ""); err == nil {
		unlock()
		t.Fatal("accepted hardlink")
	}
	if unlock, err := LockPrivate(g, "../other"); err == nil {
		unlock()
		t.Fatal("accepted traversal")
	}
}
func TestMissingDaemonNeverFallsBack(t *testing.T) {
	g := testGate(t)
	g.ControlEnv["AGENT_HEAVY_PUEUE"] = "/bin/true"
	if _, err := g.Snapshot(); err == nil {
		t.Fatal("missing embedded daemon accepted")
	}
}
func TestCommandTokenAndSafeSummary(t *testing.T) {
	value := strings.Repeat("a", 32)
	for _, command := range []string{"exec '/path with space/controller' execute " + value, "exec /python /old/agent_heavy.py execute " + value} {
		if CommandToken(Object{"command": command}) != value {
			t.Fatal(command)
		}
	}
	for _, command := range []string{"execute bad", "execute " + strings.Repeat("A", 32), "'broken execute " + value, "echo execute " + value + " extra"} {
		if CommandToken(Object{"command": command}) != "" {
			t.Fatal(command)
		}
	}
	for _, test := range []struct {
		args  []string
		title string
	}{{[]string{"bash", "-c", "echo SECRET"}, "Shell batch"}, {[]string{"python3", "-c", "SECRET"}, "Python batch"}, {[]string{"go", "test", "SECRET"}, "Go tests"}, {[]string{"npm", "run", "test", "SECRET"}, "Tests"}, {[]string{"bash", "/private/scripts/validate.sh", "SECRET"}, "validate.sh"}, {[]string{"node", "/private/check.mjs", "SECRET"}, "check.mjs"}} {
		title, _ := SafeSummary(test.args)
		if title != test.title || strings.Contains(title, "SECRET") {
			t.Errorf("%v: %s", test.args, title)
		}
	}
}
func TestDescriptionLabelValidation(t *testing.T) {
	g := testGate(t)
	for _, label := range []string{" ", "bad\nlabel", strings.Repeat("x", 121)} {
		if _, err := MakeDescription(g, []string{"go", "test"}, g.State, label); err == nil {
			t.Errorf("accepted %q", label)
		}
	}
	description, err := MakeDescription(g, []string{"go", "test"}, g.State, "  Explicit label  ")
	if err != nil || description["title"] != "Explicit label" {
		t.Fatalf("%v %v", description, err)
	}
}
func TestSharedLeaseCapacityAndIndependentCleanup(t *testing.T) {
	g := testGate(t)
	first, second := marker(1), marker(2)
	for _, m := range []Object{first, second} {
		ok, err := AcquireLease(g, m, 2)
		if err != nil || !ok {
			t.Fatalf("acquire %v %v", ok, err)
		}
	}
	if ok, err := AcquireLease(g, marker(3), 2); err != nil || ok {
		t.Fatalf("over capacity %v %v", ok, err)
	}
	if err := Cleanup(g, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.Slot); err != nil {
		t.Fatal("removed other owner's slot")
	}
	if _, err := ValidateMarker(g, 2); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(g, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.Slot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("final lease retained", err)
	}
}
func TestExternalLeaseRemainsExclusive(t *testing.T) {
	g := testGate(t)
	if err := os.Mkdir(g.Slot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Slot, "owner"), []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := AcquireLease(g, marker(1), 8); ok || err != nil {
		t.Fatalf("joined external owner %v %v", ok, err)
	}
	if !LegacyOccupied(g) {
		t.Fatal("external owner hidden")
	}
	mustWrite(t, g.ReceiptPath(7), Object{"task_id": 7, "token": strings.Repeat("b", 32)})
	if err := Cleanup(g, 7); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(g.Slot, "owner"))
	if err != nil || string(body) != "external" {
		t.Fatal("external owner changed", err)
	}
}
func TestLeaseUnknownMetadataFailsClosed(t *testing.T) {
	g := testGate(t)
	m := marker(1)
	if ok, err := AcquireLease(g, m, 2); !ok || err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Slot, "unknown"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(g, 1); err == nil {
		t.Fatal("cleaned unexplained lease")
	}
	if !LegacyOccupied(g) {
		t.Fatal("unsafe lease hidden")
	}
	if _, err := os.Stat(g.ReceiptPath(1)); err != nil {
		t.Fatal("receipt removed", err)
	}
}
func TestMarkerIdentityMismatchRetainsSlot(t *testing.T) {
	for _, mutation := range []string{"token", "pgid", "members", "task", "boot"} {
		t.Run(mutation, func(t *testing.T) {
			g := testGate(t)
			m := marker(1)
			if ok, err := AcquireLease(g, m, 1); !ok || err != nil {
				t.Fatal(err)
			}
			receipt, err := g.Receipt(1)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "token":
				receipt["token"] = strings.Repeat("c", 32)
			case "pgid":
				receipt["pgid"] = syscall.Getpgrp()
			case "members":
				m["members"] = []Object{{"pid": true, "birth": "bad"}}
				mustWrite(t, JobMarkerPath(g, m), m)
			case "task":
				receipt["task_id"] = 2
			case "boot":
				receipt["boot_uuid"] = "changed"
			}
			mustWrite(t, g.ReceiptPath(1), receipt)
			if err := Cleanup(g, 1); err == nil {
				t.Fatal("cleaned mismatched owner")
			}
			if _, err := os.Stat(g.Slot); err != nil {
				t.Fatal("slot removed")
			}
		})
	}
}
func TestDynamicMemberSnapshotPreservesReceipt(t *testing.T) {
	g := testGate(t)
	m := marker(1)
	if ok, err := AcquireLease(g, m, 1); !ok || err != nil {
		t.Fatal(err)
	}
	m["members"] = append(list(m["members"]), Object{"pid": 2_000_000_002, "birth": "child"})
	mustWrite(t, JobMarkerPath(g, m), m)
	if got, err := ValidateMarker(g, 1); err != nil || len(list(got["members"])) != 2 {
		t.Fatalf("%v %v", got, err)
	}
}
func TestReusedGroupBootIsNeverSignalled(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS boot identity")
	}
	cmd := exec.Command("/bin/sleep", "10")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	members, err := ProcessGroupMembers(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	m := Object{"pgid": cmd.Process.Pid, "boot_uuid": "wrong-boot", "members": members}
	if ok, err := VerifyGroupIdentity(m); ok || err == nil {
		t.Fatalf("accepted reused boot %v %v", ok, err)
	}
	if err = cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("signalled unrelated group", err)
	}
}
func TestStatusOrderRetentionAndSecretExclusion(t *testing.T) {
	g := testGate(t)
	tasks := Object{}
	for i := 0; i < 45; i++ {
		tasks[strconv.Itoa(i)] = Object{"command": "echo SECRET", "path": g.State, "priority": 0, "created_at": "2026-10-01T00:00:00Z", "status": Object{"Done": Object{"result": "Success", "end": fmt.Sprintf("2026-10-01T00:00:%02dZ", i)}}}
	}
	tasks["50"] = Object{"command": "echo SECRET", "path": g.State, "priority": 0, "status": Object{"Running": Object{}}}
	tasks["51"] = Object{"command": "echo SECRET", "path": g.State, "priority": 2, "status": Object{"Queued": Object{}}}
	tasks["52"] = Object{"command": "echo SECRET", "path": g.State, "priority": 5, "status": Object{"Queued": Object{}}}
	fakeQueue(t, g, testSnapshot(tasks))
	data, err := StatusData(g)
	if err != nil {
		t.Fatal(err)
	}
	jobs := list(data["jobs"])
	if len(jobs) != 33 {
		t.Fatal("retention", len(jobs))
	}
	for i, id := range []int{50, 52, 51, 44} {
		actual, _ := integer(obj(jobs[i])["id"])
		if actual != id {
			t.Fatalf("order %d: %d", i, actual)
		}
	}
	position, _ := integer(obj(jobs[1])["queuePosition"])
	if position != 1 {
		t.Fatal("queue position", position)
	}
	body, _ := json.Marshal(data)
	if bytes.Contains(body, []byte("SECRET")) {
		t.Fatal("leaked command")
	}
}
func TestExplicitGenericLabelSurvivesCachedDescription(t *testing.T) {
	g := testGate(t)
	value := strings.Repeat("d", 32)
	argv := []string{"bash", "/private/validate.sh"}
	description, err := MakeDescription(g, argv, g.State, "Shell batch")
	if err != nil {
		t.Fatal(err)
	}
	if err = CacheDescription(g, value, description); err != nil {
		t.Fatal(err)
	}
	path, _ := g.PayloadPath(value)
	mustWrite(t, path, Object{"argv": argv, "cwd": g.State, "label": "Shell batch", "explicit_label": true})
	result, err := DescribeTask(g, Object{"command": "exec /binary execute " + value, "label": "Shell batch"}, "queued")
	if err != nil || result["title"] != "Shell batch" {
		t.Fatalf("%v %v", result, err)
	}
	mustWrite(t, path, Object{"argv": argv, "cwd": g.State, "label": "Shell batch", "explicit_label": false})
	result, err = DescribeTask(g, Object{"command": "exec /binary execute " + value, "label": "Shell batch"}, "queued")
	if err != nil || result["title"] != "validate.sh" {
		t.Fatalf("upgrade %v %v", result, err)
	}
}
func TestInvalidLimitsAndPrioritiesFailBeforeQueueMutation(t *testing.T) {
	g := testGate(t)
	for _, value := range []int{-1, 0, 9, 100} {
		if err := SetLimit(g, value); err == nil {
			t.Errorf("limit %d", value)
		}
	}
	for _, value := range []int{-1<<31 - 1, 1 << 31} {
		if err := ValidatePriority(value); err == nil {
			t.Errorf("priority %d", value)
		}
	}
	t.Setenv("AGENT_HEAVY_ACTIVE", "parent")
	if _, err := Run(g, []string{"/bin/true"}, ""); err == nil || !strings.Contains(err.Error(), "Nested") {
		t.Fatal("nested submission", err)
	}
}
func TestPipedInputDoesNotBlock(t *testing.T) {
	original := os.Stdin
	defer func() { os.Stdin = original }()
	for _, kind := range []string{"data", "eof", "open-empty"} {
		t.Run(kind, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			os.Stdin = read
			if kind == "data" {
				write.Write([]byte("input"))
			}
			if kind == "eof" {
				write.Close()
			}
			started := time.Now()
			err = RejectPipedInput()
			if (err != nil) != (kind == "data") {
				t.Fatalf("%s %v", kind, err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("blocked on input")
			}
		})
	}
}

func TestRunArgumentFormsPreserveArgv(t *testing.T) {
	for _, test := range []struct {
		input []string
		label string
		argv  []string
	}{{[]string{"--label", "Build", "--", "sh", "-c", "literal --label"}, "Build", []string{"sh", "-c", "literal --label"}}, {[]string{"--label=Build", "--", "sh"}, "Build", []string{"sh"}}, {[]string{"--label", "first", "--label=second", "sh"}, "second", []string{"sh"}}, {[]string{"--", "sh", "--label=target"}, "", []string{"sh", "--label=target"}}} {
		argv, label, err := runArguments(test.input)
		if err != nil || label != test.label || !jsonEqual(argv, test.argv) {
			t.Fatalf("%v: argv=%v label=%q err=%v", test.input, argv, label, err)
		}
	}
	for _, input := range [][]string{{"--label"}, {"--label="}, {"--label", " "}} {
		if _, _, err := runArguments(input); err == nil {
			t.Fatalf("accepted malformed label %v", input)
		}
	}
}

func TestSubcommandHelpDoesNotReadQueueConfiguration(t *testing.T) {
	t.Setenv("AGENT_HEAVY_LEGACY_SLOT", "invalid-relative-lease")
	for _, args := range [][]string{{"run", "--help"}, {"run", "--label=name", "--help"}, {"status", "--help"}, {"profiles", "list", "--help"}, {"scheduler", "--help"}} {
		if code := Main(args); code != 0 {
			t.Fatalf("help read queue config for %v: %d", args, code)
		}
	}
	for _, args := range [][]string{{"run", "--", "--help"}, {"run", "sh", "--help"}, {"run", "--label", "--help", "sh"}} {
		if commandHelp(args) {
			t.Fatalf("interpreted target or label as controller help: %v", args)
		}
	}
}
