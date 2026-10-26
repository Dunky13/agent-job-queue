package appservices

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExecutableNamespaceDoesNotInheritSharedReadException(t *testing.T) {
	boundary := "/Applications/Agent Queue.app"
	for _, mode := range []os.FileMode{0755, 0775, 0777} {
		path := t.TempDir()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			t.Fatal(err)
		}
		st.Uid = 0
		if got := serviceParentSafe("/Applications", boundary, true, true, &st); got != (mode == 0755) {
			t.Fatal("executable inherited shared directory permissions", mode, got)
		}
		if got := serviceParentSafe("/Applications", boundary, true, false, &st); got != (mode != 0777) {
			t.Fatal("read-only package inspection lost its bounded exception", mode, got)
		}
	}
	var temporary unix.Stat_t
	temporary.Uid = 0
	temporary.Mode = unix.S_IFDIR | unix.S_ISVTX | 0777
	if !serviceParentSafe("/private/tmp", "/private/tmp/private-app", true, true, &temporary) || serviceParentSafe(boundary, boundary, true, true, &temporary) {
		t.Fatal("sticky temporary exception escaped outside-ancestor scope")
	}
}

func TestDecodeStatusRejectsAmbiguousNativeResponses(t *testing.T) {
	for _, raw := range []string{
		`{"schemaVersion":1,"service":"queue","status":"enabled"}`,
		`{"schemaVersion":1,"service":"queue","status":"notRegistered"}`,
		`{"schemaVersion":1,"service":"queue","status":"requiresApproval"}`,
		`{"schemaVersion":1,"service":"queue","status":"notFound"}`,
	} {
		if value, err := DecodeStatus([]byte(raw), "queue"); err != nil || value.Status == "" {
			t.Fatalf("valid response rejected %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"schemaVersion":2,"service":"queue","status":"enabled"}`,
		`{"schemaVersion":1,"service":"remote","status":"enabled"}`,
		`{"schemaVersion":1,"service":"queue","status":"unknown"}`,
		`{"schemaVersion":1,"service":"queue","status":"enabled","extra":true}`,
		`{"schemaVersion":1,"service":"queue","status":"enabled","status":"notRegistered"}`,
		`{"schemaVersion":1,"service":"queue","status":"enabled"} {}`,
		`{"schemaVersion":"1","service":"queue","status":"enabled"}`,
		`{"schemaVersion":1,"service":"queue","status":null}`,
		`null`, `[]`, ``,
	} {
		if _, err := DecodeStatus([]byte(raw), "queue"); err == nil {
			t.Fatalf("invalid native response accepted %s", raw)
		}
	}
}

func TestLoadIsAbsentOnlyForMissingMetadata(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if value, err := Load(home); err != nil || value != nil {
		t.Fatal(value, err)
	}
	root := filepath.Join(home, ".local/share/agent-heavy")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "installation.json")
	valid := Installation{SchemaVersion: 1, App: filepath.Join(home, "Applications/Agent Queue.app"), ServiceManagement: true}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if actual, err := Load(home); err != nil || actual == nil || *actual != valid {
		t.Fatal(actual, err)
	}
	for _, invalid := range []string{
		`{}`, `{"schemaVersion":1,"app":"/tmp/Agent Queue.app","serviceManagement":true}`,
		`{"schemaVersion":1,"app":"/Applications/Agent Queue.app","serviceManagement":false}`,
		`{"schemaVersion":1,"app":"/Applications/Agent Queue.app","serviceManagement":true,"schemaVersion":1}`,
		`{"schemaVersion":1,"app":"/Applications/Agent Queue.app","serviceManagement":true,"unknown":true}`,
	} {
		if err := os.WriteFile(filename, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(home); err == nil {
			t.Fatal("invalid metadata accepted", invalid)
		}
	}
}

func TestLoadAcceptsSystemTemporaryAliasAbovePrivateHome(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "agent-queue-metadata-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	value, err := Load(home)
	if err != nil || value != nil {
		t.Fatal("missing metadata below system alias did not report absent", value, err)
	}
}

func TestLoadRejectsSymlinksAndUnsafePrivateMetadata(t *testing.T) {
	for _, kind := range []string{"parent-symlink", "file-symlink", "hardlink", "writable", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".local/share/agent-heavy")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "installation.json")
			raw := []byte(`{"schemaVersion":1,"app":"/Applications/Agent Queue.app","serviceManagement":true}`)
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "parent-symlink":
				if err := os.Rename(root, root+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root+"-real", root); err != nil {
					t.Fatal(err)
				}
			case "file-symlink":
				if err := os.Rename(file, file+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(file+"-real", file); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(file, file+"-link"); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(file, 0666); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(file, make([]byte, 16385), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Load(home); err == nil {
				t.Fatal("unsafe metadata accepted", kind)
			}
		})
	}
}

func TestCanonicalHelperAndArgumentsContract(t *testing.T) {
	app := "/Applications/Agent Queue.app"
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if bundle, ok := BundleForExecutable(filepath.Join(app, "Contents/MacOS", name)); !ok || bundle != app {
			t.Fatal(bundle, ok)
		}
	}
	for _, path := range []string{app + "/Contents/Resources/payload/source/bin/agent-heavy", app + "/Contents/MacOS/../MacOS/agent-heavy", app + "/Contents/MacOS/AgentQueue", "relative.app/Contents/MacOS/agent-heavy"} {
		if _, ok := BundleForExecutable(path); ok {
			t.Fatal("noncanonical helper accepted", path)
		}
	}
	for _, action := range []string{"status", "register", "unregister"} {
		for _, service := range []string{"queue", "resources", "remote", "login"} {
			args, err := Arguments(app, action, service)
			if err != nil || len(args) != 4 || args[0] != app+"/Contents/MacOS/AgentQueue" || args[1] != "--service-management" {
				t.Fatal(args, err)
			}
		}
	}
	for _, args := range [][3]string{{app, "enable", "queue"}, {app, "status", "unknown"}, {"relative.app", "status", "queue"}, {"/Applications/../Bad.app", "status", "queue"}} {
		if _, err := Arguments(args[0], args[1], args[2]); err == nil {
			t.Fatal("invalid native command accepted", args)
		}
	}
}
