package backend

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type integrationFixture struct{ home, codex, claude string }

func integrationTestFixture(t *testing.T) integrationFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home with 'quote")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	return integrationFixture{home, filepath.Join(home, ".codex/AGENTS.md"), filepath.Join(home, ".claude/CLAUDE.md")}
}

func integrationTestWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func integrationTestRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func integrationTestRow(t *testing.T, runner string) Object {
	t.Helper()
	for _, row := range IntegrationSnapshot()["runners"].([]Object) {
		if row["id"] == runner {
			return row
		}
	}
	t.Fatal("missing runner")
	return nil
}

func TestIntegrationStatusAndRemovalAreReadOnly(t *testing.T) {
	f := integrationTestFixture(t)
	for _, runner := range []string{"codex", "claude"} {
		row := integrationTestRow(t, runner)
		if row["configured"] != false || row["error"] != nil {
			t.Fatalf("initial state: %v", row)
		}
		if err := IntegrationChange(runner, "remove"); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(f.home)
	if len(entries) != 0 {
		t.Fatalf("read-only call created %v", entries)
	}
	command := strings.Split(strings.Split(IntegrationInstruction(), "```sh\n")[1], " run --label ")[0]
	output, err := exec.Command("/bin/sh", "-c", "printf '%s' "+command).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != filepath.Join(f.home, ".local/bin/agent-heavy") {
		t.Fatalf("unsafe quoting: %q", output)
	}
	for _, text := range []string{"stdin receives EOF", "do not nest", "Do not change the operator's parallel job limit"} {
		if !strings.Contains(IntegrationInstruction(), text) {
			t.Errorf("missing instruction %q", text)
		}
	}
}

func TestIntegrationInstallIsExplicitAndIdempotent(t *testing.T) {
	f := integrationTestFixture(t)
	if err := IntegrationChange("codex", "install"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(f.codex)
	if err != nil {
		t.Fatal(err)
	}
	content := integrationTestRead(t, f.codex)
	if before.Mode().Perm() != 0600 || integrationTestRow(t, "codex")["configured"] != true {
		t.Fatal("not privately configured")
	}
	if _, err := os.Stat(f.claude); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sibling integration created")
	}
	if err := IntegrationChange("codex", "install"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(f.codex)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(content, integrationTestRead(t, f.codex)) {
		t.Fatal("idempotent call changed file")
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(f.codex), "*.agent-job-queue-backup-*"))
	if len(backups) != 0 {
		t.Fatal("idempotent call created backups")
	}
}

func TestIntegrationPreservesBytesModesAndBackups(t *testing.T) {
	for _, original := range [][]byte{nil, []byte("no final newline"), []byte("import shared.md\n"), []byte("# Heading\r\n\r\nPrivate text\r\n"), {0xff, 0xfe, 0}} {
		t.Run(string(original), func(t *testing.T) {
			f := integrationTestFixture(t)
			integrationTestWrite(t, f.claude, original)
			if err := os.Chmod(f.claude, 0640); err != nil {
				t.Fatal(err)
			}
			var backupSeen bool
			hooks := integrationHooks{write: func(parent *os.File, name string, data []byte) error {
				if strings.HasSuffix(name, ".tmp") {
					backups, _ := filepath.Glob(filepath.Join(filepath.Dir(f.claude), "*.agent-job-queue-backup-*"))
					if len(backups) != 1 || !bytes.Equal(integrationTestRead(t, backups[0]), original) {
						t.Fatal("backup must precede replacement")
					}
					info, _ := os.Stat(backups[0])
					if info.Mode().Perm() != 0600 {
						t.Fatal("backup permissions")
					}
					backupSeen = true
				}
				return integrationPrivateWrite(parent, name, data)
			}}
			if err := integrationChange("claude", "install", hooks); err != nil {
				t.Fatal(err)
			}
			if !backupSeen {
				t.Fatal("backup missing")
			}
			info, _ := os.Stat(f.claude)
			if info.Mode().Perm() != 0640 || !bytes.HasPrefix(integrationTestRead(t, f.claude), original) {
				t.Fatal("lost bytes or permissions")
			}
			if err := IntegrationChange("claude", "remove"); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(integrationTestRead(t, f.claude), original) {
				t.Fatal("round trip changed unrelated bytes")
			}
		})
	}
}

func TestIntegrationPreservesEditsOutsideBlockAndRefusesManagedEdits(t *testing.T) {
	f := integrationTestFixture(t)
	integrationTestWrite(t, f.claude, []byte("Original"))
	if err := IntegrationChange("claude", "install"); err != nil {
		t.Fatal(err)
	}
	valid := integrationTestRead(t, f.claude)
	data := append(append([]byte("New prefix\r\n"), valid...), []byte("\r\nNew suffix")...)
	integrationTestWrite(t, f.claude, data)
	if err := IntegrationChange("claude", "remove"); err != nil {
		t.Fatal(err)
	}
	if string(integrationTestRead(t, f.claude)) != "New prefix\r\nOriginal\r\nNew suffix" {
		t.Fatal("unrelated edits lost")
	}
	for _, damaged := range [][]byte{bytes.Replace(valid, []byte("conservatively"), []byte("SECRET"), 1), append(append([]byte{}, valid...), valid...), bytes.Replace(valid, []byte(integrationEnd), nil, 1), []byte(integrationEnd), []byte("<!-- agent-job-queue:begin v9 -->\nSECRET\n"), bytes.Replace(valid, []byte("Original\n\n"), []byte("Original\n"), 1)} {
		integrationTestWrite(t, f.claude, damaged)
		row := integrationTestRow(t, "claude")
		if row["configured"] != false || row["error"] == nil || strings.Contains(row["error"].(string), "SECRET") {
			t.Fatalf("unsafe snapshot: %v", row)
		}
		for _, action := range []string{"install", "remove"} {
			err := IntegrationChange("claude", action)
			if err == nil || !strings.Contains(err.Error(), "edited") {
				t.Fatalf("accepted managed modification: %v", err)
			}
			if !bytes.Equal(integrationTestRead(t, f.claude), damaged) {
				t.Fatal("damaged block overwritten")
			}
		}
	}
}

func TestIntegrationOverrideSelectionMatchesRustWhitespace(t *testing.T) {
	for _, content := range [][]byte{nil, []byte(" \n"), []byte("\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"), []byte("Actual override"), {0x1c}, {0x1d}, {0x1e}, {0x1f}, {0xff}, {0}, []byte("\u200b"), []byte("\ufeff")} {
		t.Run(string(content), func(t *testing.T) {
			f := integrationTestFixture(t)
			original := []byte("Base instructions")
			integrationTestWrite(t, f.codex, original)
			override := filepath.Join(filepath.Dir(f.codex), "AGENTS.override.md")
			integrationTestWrite(t, override, content)
			active := strings.TrimSpace(string(content)) != ""
			expected := f.codex
			if active {
				expected = override
			}
			if integrationTestRow(t, "codex")["path"] != expected {
				t.Fatal("wrong precedence")
			}
			if err := IntegrationChange("codex", "install"); err != nil {
				t.Fatal(err)
			}
			if err := IntegrationChange("codex", "remove"); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(integrationTestRead(t, f.codex), original) || !bytes.Equal(integrationTestRead(t, override), content) {
				t.Fatal("override round trip lost content")
			}
		})
	}
}

func TestIntegrationRejectsUnsafePathsAndInstructionBudget(t *testing.T) {
	f := integrationTestFixture(t)
	for _, value := range []string{"relative", "", f.home + "/nested/../elsewhere"} {
		t.Setenv("CODEX_HOME", value)
		if integrationTestRow(t, "codex")["error"] == nil {
			t.Fatal("unsafe path accepted")
		}
		if err := IntegrationChange("codex", "install"); err == nil {
			t.Fatal("unsafe path changed")
		}
	}
	t.Setenv("CODEX_HOME", filepath.Dir(f.codex))
	integrationTestWrite(t, f.codex, bytes.Repeat([]byte{'x'}, integrationCodexMaxBytes))
	if err := IntegrationChange("codex", "install"); err == nil || !strings.Contains(err.Error(), "32 KiB") {
		t.Fatalf("budget not enforced: %v", err)
	}
	integrationTestWrite(t, f.codex, nil)
	if err := IntegrationChange("codex", "install"); err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, f.codex, append(integrationTestRead(t, f.codex), bytes.Repeat([]byte{'x'}, integrationCodexMaxBytes)...))
	if integrationTestRow(t, "codex")["configured"] != false {
		t.Fatal("oversized instruction claimed active")
	}
	if err := IntegrationChange("codex", "remove"); err != nil {
		t.Fatal(err)
	}
	if len(integrationTestRead(t, f.codex)) != integrationCodexMaxBytes {
		t.Fatal("budget cleanup lost data")
	}
}

func TestIntegrationRejectsSymlinksHardlinksFIFOAndWritableFiles(t *testing.T) {
	for _, kind := range []string{"symlink-file", "symlink-parent", "symlink-override", "hardlink", "fifo", "writable-file", "writable-parent"} {
		t.Run(kind, func(t *testing.T) {
			f := integrationTestFixture(t)
			outside := filepath.Join(f.home, "outside")
			integrationTestWrite(t, outside, []byte("Do not edit"))
			if err := os.Mkdir(filepath.Dir(f.claude), 0700); err != nil {
				t.Fatal(err)
			}
			runner := "claude"
			switch kind {
			case "symlink-file":
				if err := os.Symlink(outside, f.claude); err != nil {
					t.Fatal(err)
				}
			case "symlink-parent":
				if err := os.Remove(filepath.Dir(f.claude)); err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(f.home, "other")
				if err := os.Mkdir(other, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, filepath.Dir(f.claude)); err != nil {
					t.Fatal(err)
				}
			case "symlink-override":
				runner = "codex"
				if err := os.Mkdir(filepath.Dir(f.codex), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(filepath.Dir(f.codex), "AGENTS.override.md")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, f.claude); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(f.claude, 0600); err != nil {
					t.Fatal(err)
				}
			case "writable-file":
				integrationTestWrite(t, f.claude, nil)
				if err := os.Chmod(f.claude, 0666); err != nil {
					t.Fatal(err)
				}
			case "writable-parent":
				if err := os.Chmod(filepath.Dir(f.claude), 0770); err != nil {
					t.Fatal(err)
				}
			}
			if integrationTestRow(t, runner)["error"] == nil {
				t.Fatal("unsafe instruction accepted")
			}
			if err := IntegrationChange(runner, "install"); err == nil {
				t.Fatal("unsafe instruction changed")
			}
			if string(integrationTestRead(t, outside)) != "Do not edit" {
				t.Fatal("symlink target changed")
			}
		})
	}
}

func TestIntegrationLockAndConcurrentMutationProtection(t *testing.T) {
	for _, kind := range []string{"locked", "unsafe-lock", "file", "parent", "override", "backup-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := integrationTestFixture(t)
			runner, path := "claude", f.claude
			if kind == "override" {
				runner, path = "codex", f.codex
			}
			integrationTestWrite(t, path, []byte("Original"))
			if kind == "locked" || kind == "unsafe-lock" {
				lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".agent-job-queue.lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if kind == "locked" {
					if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := lock.Chmod(0644); err != nil {
						t.Fatal(err)
					}
				}
				if err := IntegrationChange(runner, "install"); err == nil {
					t.Fatal("unsafe lock accepted")
				}
				return
			}
			hooks := integrationHooks{write: func(parent *os.File, name string, data []byte) error {
				if kind == "backup-failure" {
					return errors.New("backup unavailable")
				}
				if err := integrationPrivateWrite(parent, name, data); err != nil {
					return err
				}
				if strings.HasSuffix(name, ".tmp") {
					switch kind {
					case "file":
						integrationTestWrite(t, path, []byte("Concurrent human edit"))
					case "parent":
						if err := os.Rename(filepath.Dir(path), filepath.Join(f.home, "old-dir")); err != nil {
							t.Fatal(err)
						}
						integrationTestWrite(t, path, []byte("New directory contents"))
					case "override":
						integrationTestWrite(t, filepath.Join(filepath.Dir(path), "AGENTS.override.md"), []byte("New higher-priority rules"))
					}
				}
				return nil
			}}
			err := integrationChange(runner, "install", hooks)
			if err == nil {
				t.Fatal("concurrent edit overwritten")
			}
			expected := "Original"
			if kind == "file" {
				expected = "Concurrent human edit"
			}
			if kind == "parent" {
				expected = "New directory contents"
			}
			if string(integrationTestRead(t, path)) != expected {
				t.Fatalf("lost concurrent contents: %q", integrationTestRead(t, path))
			}
			temporaries, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
			if len(temporaries) != 0 {
				t.Fatal("temporary leaked")
			}
		})
	}
}
