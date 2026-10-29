package tooling

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const checkpointSHA = "1111111111111111111111111111111111111111"

type checkpointFixture struct {
	root, state, script string
	env                 []string
}

func newCheckpointFixture(t *testing.T) checkpointFixture {
	t.Helper()
	for _, tool := range []string{"jq", "unzip", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("checkpoint fixture requires %s", tool)
		}
	}
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	testTool(t, tools, "gh", `printf '%s\n' "$*" >> "$CHECKPOINT_TEST_ROOT/calls"
[ "$1" = api ] || exit 99
case "$2" in
  repos/owner/project/actions/runs/123) cat "$CHECKPOINT_TEST_ROOT/run.json";;
  repos/owner/project/actions/runs/123/artifacts\?per_page=100\&page=*)
    page=${2##*=}
    cat "$CHECKPOINT_TEST_ROOT/page-$page.json";;
  repos/owner/project/actions/artifacts/*/zip)
    id=${2%/zip}; id=${id##*/}
    cat "$CHECKPOINT_TEST_ROOT/artifact-$id.zip";;
  *) exit 99;;
esac
`)
	if runtime.GOOS != "darwin" {
		// Unix extraction preserves executable modes. The native macOS test below
		// separately exercises ditto and extended attributes.
		testTool(t, tools, "ditto", `[ "$1" = -x ] && [ "$2" = -k ] || exit 99
exec unzip -q "$3" -d "$4"
`)
	}
	script, err := filepath.Abs("../../scripts/macos-checkpoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	f := checkpointFixture{root: root, state: filepath.Join(root, "notarization-state"), script: script}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "GITHUB_") || name == "CHECKPOINT_TEST_ROOT" || name == "PATH" {
			continue
		}
		f.env = append(f.env, value)
	}
	f.env = append(f.env, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_ACTIONS=true", "GITHUB_EVENT_NAME=push", "GITHUB_REF=refs/heads/main",
		"GITHUB_SHA="+checkpointSHA, "GITHUB_REPOSITORY=owner/project", "GITHUB_RUN_ID=123", "GITHUB_RUN_NUMBER=45",
		"GITHUB_RUN_ATTEMPT=11", "GITHUB_OUTPUT="+filepath.Join(root, "output"), "CHECKPOINT_TEST_ROOT="+root)
	f.writeJSON(t, "run.json", map[string]any{
		"id": 123, "run_number": 45, "path": ".github/workflows/ci.yml", "event": "push", "head_branch": "main", "head_sha": checkpointSHA,
		"repository": map[string]any{"full_name": "owner/project"}, "head_repository": map[string]any{"full_name": "owner/project"},
	})
	f.inventory(t)
	return f
}

func (f checkpointFixture) writeJSON(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f checkpointFixture) inventory(t *testing.T, artifacts ...map[string]any) {
	t.Helper()
	if artifacts == nil {
		artifacts = []map[string]any{}
	}
	f.writeJSON(t, "page-1.json", map[string]any{"total_count": len(artifacts), "artifacts": artifacts})
}

type checkpointEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func checkpointZIP(t *testing.T, entries ...checkpointEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func (f checkpointFixture) artifactBytes(t *testing.T, id, attempt, slot int, data []byte) map[string]any {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, fmt.Sprintf("artifact-%d.zip", id)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": id, "name": fmt.Sprintf("Apple-notarization-123-a%d-s%02d", attempt, slot), "expired": false,
		"digest":       fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
		"created_at":   time.Date(2026, 10, 22, 13, attempt, slot, 0, time.UTC).Format(time.RFC3339),
		"workflow_run": map[string]any{"id": 123, "head_branch": "main", "head_sha": checkpointSHA},
	}
}

func checkpointEnvelope(t *testing.T, inner []byte, checksum string) []byte {
	t.Helper()
	if checksum == "" {
		checksum = fmt.Sprintf("%x  checkpoint.zip\n", sha256.Sum256(inner))
	}
	return checkpointZIP(t, checkpointEntry{"checkpoint.zip", inner, 0o600}, checkpointEntry{"checkpoint.zip.sha256", []byte(checksum), 0o600})
}

func (f checkpointFixture) artifact(t *testing.T, id, attempt, slot int) map[string]any {
	t.Helper()
	inner := checkpointZIP(t, checkpointEntry{"notarization-state/", nil, os.ModeDir | 0o700},
		checkpointEntry{"notarization-state/executable", []byte(fmt.Sprintf("checkpoint %d", id)), 0o755})
	return f.artifactBytes(t, id, attempt, slot, checkpointEnvelope(t, inner, ""))
}

func (f checkpointFixture) run(t *testing.T, extra []string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command("/bin/bash", append([]string{f.script}, args...)...)
	command.Dir = f.root
	command.Env = append(append([]string{}, f.env...), extra...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (f checkpointFixture) reject(t *testing.T, reason string, extra ...string) {
	t.Helper()
	output, err := f.run(t, extra, "restore", f.state)
	if err == nil || !strings.Contains(output, reason) {
		t.Fatalf("recovery accepted or wrong refusal, want %q: %v: %s", reason, err, output)
	}
	if _, err := os.Lstat(f.state); !os.IsNotExist(err) {
		t.Fatalf("failed restore left state: %v", err)
	}
}

func TestMacOSCheckpointSelectsLatestNumericAttemptAndSlot(t *testing.T) {
	f := newCheckpointFixture(t)
	f.inventory(t, f.artifact(t, 209, 2, 9), f.artifact(t, 1001, 10, 1), f.artifact(t, 1009, 10, 9), f.artifact(t, 1101, 11, 1))
	output, err := f.run(t, nil, "restore", f.state)
	if err != nil {
		t.Fatalf("restore: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(f.state, "executable"))
	if err != nil || string(data) != "checkpoint 1009" {
		t.Fatalf("wrong checkpoint selected: %s: %v", data, err)
	}
	info, err := os.Stat(filepath.Join(f.state, "executable"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode lost: %v: %v", info, err)
	}
	result, err := os.ReadFile(filepath.Join(f.root, "output"))
	if err != nil || string(result) != "restored=true\n" {
		t.Fatalf("missing restore output: %s: %v", result, err)
	}
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if strings.Count(string(calls), "/zip") != 1 || !strings.Contains(string(calls), "/artifacts/1009/zip") {
		t.Fatalf("download was not bound to selected artifact ID: %s", calls)
	}
}

func TestMacOSCheckpointInitialStateCanPrepareFirstSubmissionAfterRestore(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("snapshot uses native ditto")
	}
	checkpoint := newCheckpointFixture(t)
	resume := newResumeFixture(t, "GITHUB_SHA="+checkpointSHA, "GITHUB_REPOSITORY=owner/project",
		"GITHUB_RUN_ID=123", "GITHUB_RUN_NUMBER=45")
	if err := os.Rename(resume.state, checkpoint.state); err != nil {
		t.Fatal(err)
	}
	resume.state = checkpoint.state
	snapshot := filepath.Join(checkpoint.root, "initial-snapshot")
	if output, err := checkpoint.run(t, []string{"GITHUB_RUN_ATTEMPT=1"}, "snapshot", checkpoint.state, snapshot); err != nil {
		t.Fatalf("initial snapshot: %v: %s", err, output)
	}
	inner, err := os.ReadFile(filepath.Join(snapshot, "checkpoint.zip"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.inventory(t, checkpoint.artifactBytes(t, 1000, 1, 0, checkpointEnvelope(t, inner, "")))
	if err := os.Rename(checkpoint.state, filepath.Join(checkpoint.root, "original-state")); err != nil {
		t.Fatal(err)
	}
	if output, err := checkpoint.run(t, []string{"GITHUB_RUN_ATTEMPT=2"}, "restore", checkpoint.state); err != nil {
		t.Fatalf("restore before any signing or upload: %v: %s", err, output)
	}
	resume.command(t, []string{"GITHUB_RUN_ATTEMPT=2"}, "verify-state", resume.state, "0.1.0")
	resume.step(t, "prepare", "app", "GITHUB_RUN_ATTEMPT=2")
	resume.step(t, "submit", "app", "GITHUB_RUN_ATTEMPT=2")
	if strings.Count(resume.recorded(t), "xcrun|notarytool|submit|") != 1 {
		t.Fatal("initial checkpoint recovery must make exactly its first submission")
	}
}

func TestMacOSCheckpointDoesNotFallBackToInitialStateAfterSubmission(t *testing.T) {
	f := newCheckpointFixture(t)
	initial := f.artifact(t, 1000, 10, 0)
	submitted := f.artifact(t, 1002, 10, 2)
	submitted["expired"] = true
	f.inventory(t, initial, submitted)
	f.reject(t, "Newest checkpoint expired")
	calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
	if strings.Contains(string(calls), "/zip") {
		t.Fatal("expired submitted state fell back to initial state and could duplicate an upload")
	}
}

func TestMacOSCheckpointRequiresTrustedContext(t *testing.T) {
	for _, environment := range []string{"GITHUB_ACTIONS=false", "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_EVENT_NAME=pull_request_target", "GITHUB_REF=refs/heads/feature", "GITHUB_SHA=bad", "GITHUB_REPOSITORY=bad", "GITHUB_RUN_ID=0", "GITHUB_RUN_NUMBER=01", "GITHUB_RUN_ATTEMPT=-1"} {
		t.Run(environment, func(t *testing.T) {
			f := newCheckpointFixture(t)
			output, err := f.run(t, []string{environment}, "restore", f.state)
			if err == nil || !strings.Contains(output, "macOS checkpoint:") {
				t.Fatalf("invalid context accepted: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
				t.Fatalf("untrusted context reached API: %v", err)
			}
		})
	}
}

func TestMacOSCheckpointRestoresRelativeStatePath(t *testing.T) {
	f := newCheckpointFixture(t)
	f.inventory(t, f.artifact(t, 1001, 10, 1))
	output, err := f.run(t, nil, "restore", "notarization-state")
	if err != nil {
		t.Fatalf("relative restore: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(f.state, "executable")); err != nil {
		t.Fatal(err)
	}
}

func TestMacOSCheckpointRefusesExistingRestoreDestination(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint("symlink=", symlink), func(t *testing.T) {
			f := newCheckpointFixture(t)
			var err error
			if symlink {
				err = os.Symlink(filepath.Join(f.root, "absent"), f.state)
			} else {
				err = os.Mkdir(f.state, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			output, err := f.run(t, nil, "restore", f.state)
			if err == nil || !strings.Contains(output, "absent notarization-state") {
				t.Fatalf("existing destination accepted: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(f.root, "calls")); !os.IsNotExist(err) {
				t.Fatalf("existing destination reached API: %v", err)
			}
		})
	}
}

func TestMacOSCheckpointRejectsRunProvenance(t *testing.T) {
	for _, field := range []string{"id", "run_number", "path", "event", "head_branch", "head_sha", "repository", "head_repository"} {
		t.Run(field, func(t *testing.T) {
			f := newCheckpointFixture(t)
			data, err := os.ReadFile(filepath.Join(f.root, "run.json"))
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata[field] = "untrusted"
			f.writeJSON(t, "run.json", metadata)
			f.reject(t, "Run provenance")
		})
	}
}

func TestMacOSCheckpointRejectsMixedArtifactProvenance(t *testing.T) {
	for _, variant := range []string{"wrong run name", "slot", "attempt", "future", "name suffix", "wrong source", "wrong run", "wrong branch", "duplicate name"} {
		t.Run(variant, func(t *testing.T) {
			f := newCheckpointFixture(t)
			good := f.artifact(t, 1001, 10, 1)
			bad := f.artifact(t, 1002, 10, 2)
			switch variant {
			case "wrong run name":
				bad["name"] = "Apple-notarization-999-a10-s02"
			case "slot":
				bad["name"] = "Apple-notarization-123-a10-s10"
			case "attempt":
				bad["name"] = "Apple-notarization-123-a010-s02"
			case "future":
				bad["name"] = "Apple-notarization-123-a12-s02"
			case "name suffix":
				bad["name"] = "Apple-notarization-123-a10-s02-extra"
			case "wrong source":
				bad["workflow_run"] = map[string]any{"id": 123, "head_branch": "main", "head_sha": strings.Repeat("2", 40)}
			case "wrong run":
				bad["workflow_run"] = map[string]any{"id": 999, "head_branch": "main", "head_sha": checkpointSHA}
			case "wrong branch":
				bad["workflow_run"] = map[string]any{"id": 123, "head_branch": "feature", "head_sha": checkpointSHA}
			case "duplicate name":
				bad["name"] = good["name"]
			}
			f.inventory(t, good, bad)
			f.reject(t, "Mixed or malformed checkpoint provenance")
		})
	}
}

func TestMacOSCheckpointMissingAndExpiredFailClosed(t *testing.T) {
	t.Run("first attempt", func(t *testing.T) {
		f := newCheckpointFixture(t)
		output, err := f.run(t, []string{"GITHUB_RUN_ATTEMPT=1"}, "restore", f.state)
		if err != nil {
			t.Fatalf("initial run: %v: %s", err, output)
		}
		result, _ := os.ReadFile(filepath.Join(f.root, "output"))
		if string(result) != "restored=false\n" {
			t.Fatalf("initial run output: %s", result)
		}
	})
	t.Run("missing rerun", func(t *testing.T) {
		f := newCheckpointFixture(t)
		f.reject(t, "Rerun has no surviving checkpoint")
	})
	t.Run("expired newest", func(t *testing.T) {
		f := newCheckpointFixture(t)
		latest := f.artifact(t, 1009, 10, 9)
		latest["expired"] = true
		f.inventory(t, f.artifact(t, 1001, 10, 1), latest)
		f.reject(t, "Newest checkpoint expired")
		calls, _ := os.ReadFile(filepath.Join(f.root, "calls"))
		if strings.Contains(string(calls), "/zip") {
			t.Fatal("expired checkpoint fell back to download")
		}
	})
}

func TestMacOSCheckpointValidatesBothDigestsBeforeRestore(t *testing.T) {
	for _, variant := range []string{"missing", "malformed", "outer tamper", "inner tamper", "checksum path"} {
		t.Run(variant, func(t *testing.T) {
			f := newCheckpointFixture(t)
			artifact := f.artifact(t, 1001, 10, 1)
			reason := "valid GitHub SHA-256 digest"
			switch variant {
			case "missing":
				delete(artifact, "digest")
			case "malformed":
				artifact["digest"] = "sha256:invalid"
			case "outer tamper":
				artifact["digest"] = "sha256:" + strings.Repeat("0", 64)
				reason = "GitHub artifact digest mismatch"
			case "inner tamper", "checksum path":
				checksum := strings.Repeat("0", 64) + "  checkpoint.zip\n"
				reason = "Inner checkpoint digest mismatch"
				if variant == "checksum path" {
					checksum = strings.Repeat("0", 64) + "  ../../other.zip\n"
					reason = "Invalid inner checkpoint digest"
				}
				artifact = f.artifactBytes(t, 1001, 10, 1, checkpointEnvelope(t, []byte("changed bytes"), checksum))
			}
			f.inventory(t, artifact)
			f.reject(t, reason)
		})
	}
}

func TestMacOSCheckpointRejectsUnsafeArchiveEntries(t *testing.T) {
	for _, variant := range []string{"outer extra", "outer symlink", "inner traversal", "inner absolute", "inner symlink escape", "inner duplicate", "inner newline"} {
		t.Run(variant, func(t *testing.T) {
			f := newCheckpointFixture(t)
			var envelope []byte
			reason := "archive"
			switch variant {
			case "outer extra":
				envelope = checkpointZIP(t, checkpointEntry{"unexpected", nil, 0o600})
			case "outer symlink":
				envelope = checkpointZIP(t, checkpointEntry{"checkpoint.zip", []byte("target"), os.ModeSymlink | 0o777}, checkpointEntry{"checkpoint.zip.sha256", nil, 0o600})
			default:
				entry := checkpointEntry{"notarization-state/../../escaped", []byte("unsafe"), 0o600}
				switch variant {
				case "inner absolute":
					entry.name = "/escaped"
				case "inner symlink escape":
					entry = checkpointEntry{"notarization-state/link", []byte("../escaped"), os.ModeSymlink | 0o777}
					reason = "contains a symlink"
				case "inner duplicate":
					entry.name = "notarization-state/"
					entry.data = nil
				case "inner newline":
					entry.name = "notarization-state/a\nnotarization-state/b"
				}
				envelope = checkpointEnvelope(t, checkpointZIP(t, checkpointEntry{"notarization-state/", nil, os.ModeDir | 0o700}, entry), "")
			}
			f.inventory(t, f.artifactBytes(t, 1001, 10, 1, envelope))
			f.reject(t, reason)
			if _, err := os.Stat(filepath.Join(f.root, "escaped")); !os.IsNotExist(err) {
				t.Fatal("unsafe archive wrote outside extraction directory")
			}
		})
	}
}

func TestMacOSCheckpointPaginatesAndRejectsInconsistentInventory(t *testing.T) {
	for _, variant := range []string{"success", "truncated", "changed count", "duplicate id"} {
		t.Run(variant, func(t *testing.T) {
			f := newCheckpointFixture(t)
			first := []map[string]any{{"id": 1, "name": "unrelated-report"}}
			latest := f.artifact(t, 1009, 10, 9)
			second := []map[string]any{latest}
			secondTotal := 2
			switch variant {
			case "truncated":
				second = []map[string]any{}
			case "changed count":
				secondTotal = 3
			case "duplicate id":
				first[0]["id"] = 1009
			}
			f.writeJSON(t, "page-1.json", map[string]any{"total_count": 2, "artifacts": first})
			f.writeJSON(t, "page-2.json", map[string]any{"total_count": secondTotal, "artifacts": second})
			if variant == "success" {
				output, err := f.run(t, nil, "restore", f.state)
				if err != nil {
					t.Fatalf("paginated restore: %v: %s", err, output)
				}
			} else {
				reason := "inventory"
				if variant == "duplicate id" {
					reason = "duplicate artifact identity"
				}
				f.reject(t, reason)
			}
		})
	}
}

func TestMacOSCheckpointSnapshotPreservesModesAndResources(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("ditto resource fork round-trip requires macOS")
	}
	f := newCheckpointFixture(t)
	if err := os.Mkdir(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(f.state, "executable")
	if err := os.WriteFile(executable, []byte("signed executable fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The queue's private umask can restrict creation to 0700. Establish the
	// exact input permissions whose preservation this round-trip exercises.
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(executable)
	if err != nil || before.Mode().Perm() != 0o755 {
		t.Fatalf("fixture executable permissions: %v: %v", before, err)
	}
	if output, err := exec.Command("xattr", "-w", "io.went.checkpoint-fixture", "resource preserved", executable).CombinedOutput(); err != nil {
		t.Fatalf("create fixture xattr: %v: %s", err, output)
	}
	destination := filepath.Join(f.root, "snapshot")
	if output, err := f.run(t, nil, "snapshot", f.state, destination); err != nil {
		t.Fatalf("snapshot: %v: %s", err, output)
	}
	if err := os.Rename(f.state, filepath.Join(f.root, "original-state")); err != nil {
		t.Fatal(err)
	}
	inner, err := os.ReadFile(filepath.Join(destination, "checkpoint.zip"))
	if err != nil {
		t.Fatal(err)
	}
	f.inventory(t, f.artifactBytes(t, 1001, 10, 1, checkpointEnvelope(t, inner, "")))
	if output, err := f.run(t, nil, "restore", f.state); err != nil {
		t.Fatalf("round-trip restore: %v: %s", err, output)
	}
	info, err := os.Stat(executable)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("snapshot lost executable permissions: %v: %v", info, err)
	}
	if output, err := exec.Command("xattr", "-p", "io.went.checkpoint-fixture", executable).CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "resource preserved" {
		t.Fatalf("snapshot lost xattr: %v: %s", err, output)
	}
}

func TestMacOSCheckpointSnapshotRejectsActiveTemporaryOrLinkedState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("snapshot uses native ditto")
	}
	for _, variant := range []string{"active lock", "temporary work", "linked root", "linked member", "public mode"} {
		t.Run(variant, func(t *testing.T) {
			f := newCheckpointFixture(t)
			if err := os.Mkdir(f.state, 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch variant {
			case "active lock":
				err = os.Mkdir(filepath.Join(f.state, ".lock"), 0o700)
			case "temporary work":
				err = os.Mkdir(filepath.Join(f.state, "work"), 0o700)
			case "linked root":
				err = os.Rename(f.state, filepath.Join(f.root, "original-state"))
				if err == nil {
					err = os.Symlink("original-state", f.state)
				}
			case "linked member":
				err = os.Symlink("missing", filepath.Join(f.state, "member"))
			case "public mode":
				err = os.Chmod(f.state, 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(f.root, "snapshot")
			output, err := f.run(t, nil, "snapshot", f.state, destination)
			if err == nil || !strings.Contains(output, "macOS checkpoint:") {
				t.Fatalf("unsafe snapshot accepted: %v: %s", err, output)
			}
			if _, err := os.Stat(filepath.Join(destination, "checkpoint.zip")); !os.IsNotExist(err) {
				t.Fatal("unsafe state published a checkpoint")
			}
			if variant == "linked member" {
				if _, err := os.Stat(filepath.Join(f.state, ".lock")); !os.IsNotExist(err) {
					t.Fatal("failed snapshot retained its state lock")
				}
			}
		})
	}
}
