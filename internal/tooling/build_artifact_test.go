package tooling

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

func buildArtifactFixture(t *testing.T, change func(map[string][]byte)) (string, string, string, string) {
	t.Helper()
	commit, build := strings.Repeat("a", 40), 21
	info := backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: "0.1.0", Commit: &commit, Build: &build}
	metadata, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	base := "Agent Queue.app/"
	contents := map[string][]byte{
		base + "Contents/Resources/build-info.json":                metadata,
		base + "Contents/Resources/payload/source/build-info.json": metadata,
		base + "Contents/Resources/payload/source/VERSION":         []byte("0.1.0\n"),
		base + "Contents/MacOS/AgentQueue":                         []byte("original executable bytes"),
	}
	if change != nil {
		change(contents)
	}
	var entries []checkpointEntry
	for name, data := range contents {
		entries = append(entries, checkpointEntry{name, data, 0755})
	}
	inner := checkpointZIP(t, entries...)
	asset := "Agent-Queue-0.1.0-universal.zip"
	image := strings.TrimSuffix(asset, ".zip") + ".dmg"
	manifest := backend.Manifest{BuildInfo: info, Tag: "build-21-aaaaaaa", Asset: asset, SHA256: fmt.Sprintf("%x", sha256.Sum256(inner))}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	outer := checkpointZIP(t,
		checkpointEntry{asset, inner, 0600}, checkpointEntry{image, []byte("original disk image"), 0600},
		checkpointEntry{asset + ".sha256", []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(inner), asset)), 0600},
		checkpointEntry{image + ".sha256", []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("original disk image")), image)), 0600},
		checkpointEntry{"agent-job-queue.json", data, 0600}, checkpointEntry{"agent-job-queue.rb", []byte("original cask"), 0600})
	root := t.TempDir()
	archive := filepath.Join(root, "artifact.zip")
	if err := os.WriteFile(archive, outer, 0600); err != nil {
		t.Fatal(err)
	}
	return archive, fmt.Sprintf("sha256:%x", sha256.Sum256(outer)), commit, filepath.Join(root, "verified")
}

func TestBuildArtifactVerifiesOriginalBytesAndEmbeddedIdentity(t *testing.T) {
	archive, digest, commit, output := buildArtifactFixture(t, nil)
	version, err := VerifyBuildArtifact(archive, digest, commit, 21, output)
	if err != nil || version != "0.1.0" {
		t.Fatalf("verify original: version=%s err=%v", version, err)
	}
	if _, err := os.Stat(filepath.Join(output, "Agent-Queue-0.1.0-universal.zip")); err != nil {
		t.Fatal(err)
	}
	// An automation checkout's VERSION is deliberately not consulted.
	if _, err := VerifyBuildArtifact(archive, digest, commit, 21, output); err == nil {
		t.Fatal("existing destination overwritten")
	}
}

func TestBuildArtifactRejectsUntrustedIdentityAndArchivePaths(t *testing.T) {
	for _, variant := range []string{"digest", "commit", "build", "embedded repository", "embedded SHA", "embedded build", "embedded version", "source version", "traversal", "newline", "shadowed directory", "installer layout"} {
		t.Run(variant, func(t *testing.T) {
			archive, digest, commit, output := buildArtifactFixture(t, func(contents map[string][]byte) {
				name := "Agent Queue.app/Contents/Resources/payload/source/build-info.json"
				if strings.HasPrefix(variant, "embedded ") {
					var info map[string]any
					if err := json.Unmarshal(contents[name], &info); err != nil {
						t.Fatal(err)
					}
					switch variant {
					case "embedded repository":
						info["repository"] = "fork/project"
					case "embedded SHA":
						info["commit"] = strings.Repeat("b", 40)
					case "embedded build":
						info["build"] = 22
					case "embedded version":
						info["version"] = "9.9.9"
					}
					data, err := json.Marshal(info)
					if err != nil {
						t.Fatal(err)
					}
					contents[name] = data
				}
				switch variant {
				case "source version":
					contents["Agent Queue.app/Contents/Resources/payload/source/VERSION"] = []byte("9.9.9\n")
				case "traversal":
					contents["../escaped"] = []byte("bad")
				case "newline":
					contents["Agent Queue.app/a\nb"] = []byte("bad")
				case "shadowed directory":
					contents["Agent Queue.app/Contents"] = []byte("bad")
				case "installer layout":
					contents["Agent Job Queue Installer.app/Contents/Resources/build-info.json"] = contents["Agent Queue.app/Contents/Resources/build-info.json"]
				}
			})
			switch variant {
			case "digest":
				digest = "sha256:" + strings.Repeat("0", 64)
			case "commit":
				commit = strings.Repeat("b", 40)
			}
			build := 21
			if variant == "build" {
				build = 22
			}
			if _, err := VerifyBuildArtifact(archive, digest, commit, build, output); err == nil {
				t.Fatal("invalid original accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("rejected artifact left output: %v", err)
			}
		})
	}
}

func TestCheckpointOriginRequiresExactBytesModesAndResourceMetadata(t *testing.T) {
	for _, variant := range []string{"same", "bytes", "mode", "xattr", "missing", "link"} {
		t.Run(variant, func(t *testing.T) {
			entries := []checkpointEntry{{"Agent Queue.app/", nil, os.ModeDir | 0755}, {"Agent Queue.app/executable", []byte("original"), 0755}, {"__MACOSX/._Agent Queue.app", []byte("opaque resource metadata"), 0600}}
			original := checkpointZIP(t, entries...)
			switch variant {
			case "bytes":
				entries[1].data = []byte("modified")
			case "mode":
				entries[1].mode = 0644
			case "xattr":
				entries[2].data = []byte("modified resources")
			case "missing":
				entries = entries[:2]
			case "link":
				entries[1].mode = os.ModeSymlink | 0777
			}
			root := t.TempDir()
			left, right := filepath.Join(root, "original.zip"), filepath.Join(root, "initial.zip")
			if err := os.WriteFile(left, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(right, checkpointZIP(t, entries...), 0600); err != nil {
				t.Fatal(err)
			}
			err := VerifyCheckpointOrigin(left, right)
			if (err == nil) != (variant == "same") {
				t.Fatalf("origin comparison: %v", err)
			}
		})
	}
}

func TestCheckpointOriginNativeDittoRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real ditto/xattr round-trip requires macOS")
	}
	for _, mask := range []string{"022", "077"} {
		t.Run(mask, func(t *testing.T) {
			root := t.TempDir()
			app := filepath.Join(root, "Agent Queue.app")
			if err := os.MkdirAll(filepath.Join(app, "Contents/MacOS"), 0755); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(app, "Contents/MacOS/AgentQueue")
			if err := os.WriteFile(executable, []byte("inert original bytes\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(executable, 0755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("xattr", "-w", "io.went.origin-fixture", "retained metadata", executable).CombinedOutput(); err != nil {
				t.Fatalf("xattr: %v: %s", err, out)
			}
			original, initial := filepath.Join(root, "original.zip"), filepath.Join(root, "initial.zip")
			command := exec.Command("/bin/bash", "-c", `set -euo pipefail
 umask "$1"
 ditto -c -k --sequesterRsrc --keepParent "$2" "$3"
 mkdir "$4/extracted" "$4/work"
 ditto -x -k "$3" "$4/extracted"
 ditto "$4/extracted/Agent Queue.app" "$4/work/Agent Queue.app"
 ditto -c -k --sequesterRsrc --keepParent "$4/work/Agent Queue.app" "$5"`, "origin-fixture", mask, app, original, root, initial)
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("ditto roundtrip: %v: %s", err, out)
			}
			if err := VerifyCheckpointOrigin(original, initial); err != nil {
				t.Fatalf("authentic ditto initial.zip rejected (umask %s): %v", mask, err)
			}
		})
	}
}
