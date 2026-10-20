package backend

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixtureEnvironment(environment []string, key, value string, present bool) []string {
	result := []string{}
	for _, entry := range environment {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	if present {
		result = append(result, key+"="+value)
	}
	return result
}

func fixtureExecutable(t *testing.T, path, script string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRealDaemonPATHRetriesOnlyFailedStarts(t *testing.T) {
	f := daemonAcceptance(t)
	baseEnvironment := f.environment
	for _, test := range []struct {
		name, script string
		mode         os.FileMode
		code         int
		output       string
	}{
		{"owner-denied", "#!/bin/sh\nprintf forbidden", 0610, 0, "fallback"},
		{"missing-interpreter", "#!/nonexistent/queue-fixture-interpreter\nprintf forbidden", 0700, 0, "fallback"},
		{"invalid-executable-format", "printf forbidden", 0700, 0, "fallback"},
		{"command-exits-126", "#!/bin/sh\nprintf first; exit 126", 0700, 126, "first"},
		{"command-exits-127", "#!/bin/sh\nprintf first; exit 127", 0700, 127, "first"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := filepath.Join(f.root, test.name, "first")
			second := filepath.Join(f.root, test.name, "second")
			fixtureExecutable(t, filepath.Join(first, "queue-command"), test.script, test.mode)
			fixtureExecutable(t, filepath.Join(second, "queue-command"), "#!/bin/sh\nprintf fallback", 0700)
			f.environment = fixtureEnvironment(baseEnvironment, "PATH", first+":"+second, true)
			if output := f.launch("queue-command").wait(t, test.code); output != test.output {
				t.Fatalf("PATH execution output=%q want=%q", output, test.output)
			}
		})
	}
}

func TestRealDaemonPATHDefaultsRelativeEntriesAndErrors(t *testing.T) {
	f := daemonAcceptance(t)
	baseEnvironment := f.environment
	fixtureExecutable(t, filepath.Join(f.root, "queue-command"), "#!/bin/sh\nprintf cwd", 0700)
	fixtureExecutable(t, filepath.Join(f.root, "relative", "queue-command"), "#!/bin/sh\nprintf relative", 0700)
	fixtureExecutable(t, filepath.Join(f.root, "denied", "queue-command"), "#!/bin/sh\nprintf forbidden", 0600)
	for _, test := range []struct {
		name, path, command, output string
		present                     bool
		code                        int
	}{
		{"absent", "", "sh", "default", false, 0},
		{"empty", "", "queue-command", "cwd", true, 0},
		{"relative", "relative", "queue-command", "relative", true, 0},
		{"permission-before-missing", "denied:missing", "queue-command", "", true, 126},
		{"permission-after-missing", "missing:denied", "queue-command", "", true, 126},
		{"missing", "missing", "queue-command", "", true, 127},
		{"not-a-directory", "queue-command", "queue-command", "", true, 125},
		{"explicit-relative", "relative", "./queue-command", "cwd", true, 0},
		{"explicit-denied", "relative", "./denied/queue-command", "", true, 126},
		{"explicit-missing", "relative", "./missing/queue-command", "", true, 127},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.environment = fixtureEnvironment(baseEnvironment, "PATH", test.path, test.present)
			args := []string{test.command}
			if test.name == "absent" {
				args = append(args, "-c", "printf default")
			}
			if output := f.launch(args...).wait(t, test.code); (test.code == 0 || test.output != "") && output != test.output {
				t.Fatalf("PATH output=%q want=%q", output, test.output)
			}
		})
	}
}

func TestRealDaemonPreservesRawUnixArgumentsAndEnvironment(t *testing.T) {
	f := daemonAcceptance(t)
	f.environment = append(f.environment, "QUEUE_BYTES=env-\xfe", "QUEUE_FILE=environment-résultat")
	output := f.launch("/bin/sh", "-c", `printf '%s' "$1" > "$2"; printf '%s' "$QUEUE_BYTES" > "$QUEUE_FILE"; printf '%s\000%s' "$1" "$QUEUE_BYTES"`, "fixture", "arg-\xff", "argument-résultat").wait(t, 0)
	if output != "arg-\xff\x00env-\xfe" {
		t.Fatalf("raw argument/environment bytes changed: %x", []byte(output))
	}
	for filename, expected := range map[string]string{"argument-résultat": "arg-\xff", "environment-résultat": "env-\xfe"} {
		body, err := os.ReadFile(filepath.Join(f.root, filename))
		if err != nil || string(body) != expected {
			t.Fatalf("literal filename/data changed: filename=%q body=%x err=%v", filename, body, err)
		}
	}
}

func TestRealDaemonPreservesRawUnixEnvironmentNames(t *testing.T) {
	f := daemonAcceptance(t)
	pair := "NAME-\xff=value-\xfe"
	f.environment = append(f.environment, pair)
	output := f.launch("/usr/bin/env").wait(t, 0)
	if !strings.Contains("\n"+output, "\n"+pair+"\n") {
		t.Fatal("raw Unix environment name/value changed")
	}
}

func TestRealDaemonPreservesRawUnixDirectoryExecutableAndFilenames(t *testing.T) {
	f := daemonAcceptance(t)
	f.root = filepath.Join(f.root, "directory-\xff")
	if err := os.Mkdir(f.root, 0700); err != nil {
		if errors.Is(err, syscall.EILSEQ) {
			t.Skip("filesystem rejects invalid UTF-8 pathnames with EILSEQ; raw cwd execution needs a byte-preserving filesystem")
		}
		t.Fatal(err)
	}
	executable := "command-\xfe"
	fixtureExecutable(t, filepath.Join(f.root, executable), "#!/bin/sh\nprintf '%s\\000%s\\000%s' \"$1\" \"$QUEUE_BYTES\" \"$PWD\"; printf '%s' \"$1\" > \"$2\"; printf '%s' \"$QUEUE_BYTES\" > \"$QUEUE_FILE\"; exit 9", 0700)
	f.environment = fixtureEnvironment(f.environment, "PATH", "", true)
	f.environment = append(f.environment, "QUEUE_BYTES=env-\xfe", "QUEUE_FILE=env-file-\xfd")
	for _, command := range []string{executable, "./" + executable} {
		output := f.launch(command, "arg-\xff", "arg-file-\xfc").wait(t, 9)
		if output != "arg-\xff\x00env-\xfe\x00"+f.root {
			t.Fatalf("raw command execution changed bytes: %x", []byte(output))
		}
		for name, expected := range map[string]string{"arg-file-\xfc": "arg-\xff", "env-file-\xfd": "env-\xfe"} {
			body, err := os.ReadFile(filepath.Join(f.root, name))
			if err != nil || string(body) != expected {
				t.Fatalf("raw filename %x: body=%x err=%v", []byte(name), body, err)
			}
		}
	}
}

func TestRealDaemonRawUnixDirectoryCannotAliasReplacementCharacter(t *testing.T) {
	f := daemonAcceptance(t)
	replacement := filepath.Join(f.root, "directory-\ufffd")
	if err := os.Mkdir(replacement, 0700); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(f.root, "directory-\xff")
	value := token()
	path, _ := f.gate.PayloadPath(value)
	if err := WriteJobPayload(path, Object{"token": value, "argv": []string{"/bin/sh", "-c", "printf forbidden > should-not-exist"}, "cwd": raw, "env": Object{}}); err != nil {
		t.Fatal(err)
	}
	added, err := f.gate.QueueControl("submit", Object{"token": value})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := integer(added["id"])
	if !ok {
		t.Fatal("missing submission identity")
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		task, err := f.gate.Task(id)
		if err != nil {
			t.Fatal(err)
		}
		name, details, err := TaskStatus(task)
		if err != nil {
			t.Fatal(err)
		}
		if name != "Done" {
			return false
		}
		code, err := ResultCode(details["result"])
		if err != nil || (code != 125 && code != 127) {
			t.Fatalf("raw directory must remain missing/inaccessible: code=%d err=%v", code, err)
		}
		return true
	})
	if _, err := os.Stat(filepath.Join(replacement, "should-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("raw cwd aliased a Unicode replacement directory", err)
	}
}

func TestRealDaemonRejectsUnknownExecutionByteVersion(t *testing.T) {
	f := daemonAcceptance(t)
	value := token()
	path, _ := f.gate.PayloadPath(value)
	encoded := validBytePayload()
	encoded["version"] = 2
	mustWrite(t, path, Object{"token": value, "execution_bytes": encoded})
	if _, err := f.gate.QueueControl("submit", Object{"token": value}); err == nil {
		t.Fatal("daemon acknowledged an unsupported execution byte payload")
	}
	if ids := f.taskIDs(); len(ids) != 0 {
		t.Fatal("malformed payload created queue work", ids)
	}
}
