// Package tooling contains repository and release checks used by local hooks and CI.
package tooling

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

func ShiftFixture(input, output string) error {
	body, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	var fixture map[string]json.RawMessage
	if err = json.Unmarshal(body, &fixture); err != nil {
		return err
	}
	var updated string
	if err = json.Unmarshal(fixture["updatedAt"], &updated); err != nil {
		return err
	}
	previous, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	offset := now.Sub(previous)
	var jobs []map[string]json.RawMessage
	if err = json.Unmarshal(fixture["jobs"], &jobs); err != nil {
		return err
	}
	for _, job := range jobs {
		for _, key := range []string{"submittedAt", "startedAt", "finishedAt"} {
			if bytes.Equal(job[key], []byte("null")) {
				continue
			}
			var value string
			if err = json.Unmarshal(job[key], &value); err != nil {
				return err
			}
			timestamp, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return err
			}
			job[key], err = json.Marshal(timestamp.Add(offset).UTC().Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
		}
	}
	fixture["jobs"], err = json.Marshal(jobs)
	if err != nil {
		return err
	}
	fixture["updatedAt"], err = json.Marshal(now.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	body, err = json.Marshal(fixture)
	if err != nil {
		return err
	}
	return os.WriteFile(output, body, 0o600)
}

// WriteLicenses ships the Go runtime and module notices with distributed binaries.
func WriteLicenses(destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("go", "env", "GOROOT")
	root, err := cmd.Output()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(root)), "LICENSE"))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(destination, "Go-LICENSE"), body, 0o644); err != nil {
		return err
	}
	cmd = exec.Command("go", "list", "-deps", "-json", "./cmd/agent-heavy", "./cmd/agent-remote")
	modules, err := cmd.Output()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(modules))
	seen := map[string]bool{}
	for {
		var dependency struct {
			Module *struct {
				Path, Version, Dir string
				Main               bool
			}
		}
		if err = decoder.Decode(&dependency); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		module := dependency.Module
		if module == nil || module.Main || seen[module.Path] {
			continue
		}
		seen[module.Path] = true
		if module.Dir == "" {
			return fmt.Errorf("Module license source unavailable for %s", module.Path)
		}
		entries, err := os.ReadDir(module.Dir)
		if err != nil {
			return err
		}
		found := false
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := strings.ToUpper(entry.Name())
			if !strings.HasPrefix(name, "LICENSE") && !strings.HasPrefix(name, "COPYING") && !strings.HasPrefix(name, "NOTICE") && !strings.HasPrefix(name, "COPYRIGHT") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(module.Dir, entry.Name()))
			if err != nil {
				return err
			}
			filename := strings.ReplaceAll(module.Path, "/", "_") + "-" + module.Version + "-" + entry.Name()
			if err = os.WriteFile(filepath.Join(destination, filename), body, 0o644); err != nil {
				return err
			}
			found = true
		}
		if !found {
			return fmt.Errorf("No license found for module %s", module.Path)
		}
	}
	return nil
}

var homePath = regexp.MustCompile(`/Users/[A-Za-z0-9_.-]+/|/home/[A-Za-z0-9_.-]+/`)
var privateKey = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`)

func CheckSource(root string) error {
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	failures := []string{}
	count := 0
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		count++
		if info.Mode()&os.ModeSymlink != 0 {
			failures = append(failures, name+": source symlinks require an explicit packaging review")
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		for _, part := range strings.Split(filepath.ToSlash(name), "/") {
			if part == ".venv" || part == "node_modules" || part == "__pycache__" || part == ".t3" {
				failures = append(failures, name+": private or generated data must not be published")
				break
			}
		}
		switch filepath.Ext(path) {
		case ".log", ".sqlite", ".sqlite3", ".pem", ".key", ".p12", ".pfx":
			failures = append(failures, name+": private or generated data must not be published")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if homePath.Match(body) {
			failures = append(failures, name+": machine-specific home or checkout path")
		}
		if privateKey.Match(body) {
			failures = append(failures, name+": private key material")
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "\n"))
	}
	fmt.Printf("Public-source boundary passed (%d files).\n", count)
	return nil
}
func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
func CheckCommits(base, head string, local bool, repository string) error {
	resolvedHead, err := gitOutput("rev-parse", "--verify", "--end-of-options", head+"^{commit}")
	if err != nil {
		return err
	}
	revision := resolvedHead
	if base != "" && strings.Trim(base, "0") != "" {
		resolvedBase, err := gitOutput("rev-parse", "--verify", "--end-of-options", base+"^{commit}")
		if err != nil {
			return err
		}
		revision = resolvedBase + ".." + resolvedHead
	}
	out, err := gitOutput("rev-list", revision)
	if err != nil {
		return err
	}
	commits := strings.Fields(out)
	if repository != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repository) {
		return errors.New("Invalid GitHub repository")
	}
	for _, commit := range commits {
		info, err := gitOutput("show", "-s", "--format=%an%n%ae%n%B", commit)
		if err != nil {
			return err
		}
		fields := strings.SplitN(info, "\n", 3)
		if len(fields) != 3 {
			return errors.New("Invalid commit metadata")
		}
		required := fmt.Sprintf("Signed-off-by: %s <%s>", fields[0], fields[1])
		signed := false
		for _, line := range strings.Split(fields[2], "\n") {
			signed = signed || line == required
		}
		if !signed {
			return fmt.Errorf("%s: missing author-matching DCO sign-off", commit)
		}
		if local {
			cmd := exec.Command("git", "verify-commit", commit)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
		}
		if repository != "" {
			cmd := exec.Command("gh", "api", "repos/"+repository+"/commits/"+commit)
			data, err := cmd.Output()
			if err != nil {
				return err
			}
			var response struct {
				Commit struct {
					Verification struct {
						Verified bool `json:"verified"`
					} `json:"verification"`
				} `json:"commit"`
			}
			if err = json.Unmarshal(data, &response); err != nil {
				return err
			}
			if !response.Commit.Verification.Verified {
				return fmt.Errorf("%s: GitHub does not report a verified cryptographic signature", commit)
			}
		}
	}
	fmt.Printf("Commit policy passed (%d commits).\n", len(commits))
	return nil
}

func CreateManifest(archive, version, tag, commit string, build int) (backend.Manifest, error) {
	file, err := os.Open(archive)
	if err != nil {
		return backend.Manifest{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return backend.Manifest{}, err
	}
	manifest := backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: 1, Repository: backend.ReleaseRepository, Version: version, Commit: &commit, Build: &build}, Tag: tag, Asset: filepath.Base(archive), SHA256: hex.EncodeToString(hash.Sum(nil))}
	data, err := json.Marshal(manifest)
	if err != nil {
		return backend.Manifest{}, err
	}
	return backend.ValidateManifest(data)
}
func RenderCask(item backend.Manifest) (string, error) {
	data, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	if _, err = backend.ValidateManifest(data); err != nil {
		return "", err
	}
	return fmt.Sprintf(caskTemplate, item.Version, *item.Build, item.SHA256, backend.ReleaseRepository, item.Tag, item.Asset, backend.ReleaseRepository), nil
}
func RenderRelease(archive, version, tag, commit string, build int, output string) error {
	manifest, err := CreateManifest(archive, version, tag, commit, build)
	if err != nil {
		return err
	}
	cask, err := RenderCask(manifest)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(output, "agent-job-queue.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, "agent-job-queue.rb"), []byte(cask), 0o644)
}
func VerifyRelease(existing, assets, commit string) error {
	data, err := os.ReadFile(filepath.Join(existing, "release.json"))
	if err != nil {
		return err
	}
	var release struct {
		Target string `json:"targetCommitish"`
	}
	if err = json.Unmarshal(data, &release); err != nil {
		return err
	}
	if release.Target != commit {
		return errors.New("Existing immutable release has a different target commit")
	}
	entries, err := os.ReadDir(assets)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return errors.New("Release assets must be regular files")
		}
		left, err := os.ReadFile(filepath.Join(assets, entry.Name()))
		if err != nil {
			return err
		}
		right, err := os.ReadFile(filepath.Join(existing, entry.Name()))
		if err != nil {
			return err
		}
		a, b := sha256.Sum256(left), sha256.Sum256(right)
		if !bytes.Equal(a[:], b[:]) {
			return fmt.Errorf("Existing immutable release differs: %s", entry.Name())
		}
	}
	fmt.Println("Existing release target and all assets match; no overwrite performed.")
	return nil
}

const caskTemplate = `cask "agent-job-queue" do
  version "%s,%d"
  sha256 "%s"

  url "https://github.com/%s/releases/download/%s/%s"
  name "Agent Job Queue"
  desc "Shared heavy-command queue with a macOS menu-bar app"
  homepage "https://github.com/%s"

  depends_on macos: :sequoia

  # Preserve quarantine so macOS can assess the notarized release.

  payload = "#{staged_path}/Agent Job Queue Installer.app/Contents/Resources/payload"
  installer script: {
    executable: "#{payload}/source/bin/agent-heavy",
    args:       ["install", "--payload", payload],
    sudo:       false,
  }

  # Stable per-user wrappers survive staged cask cleanup and future upgrades.
  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/links/agent-heavy"
  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/links/agent-remote"

  uninstall quit: "io.went.agent-queue",
            delete: [
              "#{Dir.home}/Applications/Agent Queue.app",
              "#{Dir.home}/Library/LaunchAgents/io.went.agent-queue.plist",
            ]

  caveats <<~EOS
    Installs and opens Agent Queue for this account without Python, uv, or a Go toolchain.
    Release installer and app are Developer ID signed and notarized.
    Quarantine is retained. Phone access is off until enabled.
    Uninstall preserves the independent queue, jobs, credentials and remote access.
    Disable phone access in settings before uninstall to stop its HTTPS service.
  EOS
end
`
