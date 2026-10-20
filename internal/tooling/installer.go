package tooling

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/backend"
	"howett.net/plist"
)

// VerifyInstaller checks the actual distributed native app and Go payload.
func VerifyInstaller(installer string) error {
	payload := filepath.Join(installer, "Contents/Resources/payload")
	source := filepath.Join(payload, "source")
	raw, err := os.ReadFile(filepath.Join(source, "build-info.json"))
	if err != nil {
		return err
	}
	identity, err := backend.ValidateBuildInfo(raw)
	if err != nil {
		return err
	}
	build := "0"
	if identity.Build != nil {
		build = strconv.Itoa(*identity.Build)
	}
	for _, bundle := range []struct{ path, name, id string }{{installer, "Installer", "io.went.agent-queue.installer"}, {filepath.Join(payload, "Agent Queue.app"), "AgentQueue", "io.went.agent-queue"}} {
		infoBytes, err := os.ReadFile(filepath.Join(bundle.path, "Contents/Info.plist"))
		if err != nil {
			return err
		}
		var info map[string]any
		if _, err = plist.Unmarshal(infoBytes, &info); err != nil {
			return err
		}
		if info["LSMinimumSystemVersion"] != "15.0" || info["CFBundleIdentifier"] != bundle.id || info["CFBundleExecutable"] != bundle.name || info["CFBundleShortVersionString"] != identity.Version || info["CFBundleVersion"] != build {
			return fmt.Errorf("Native bundle metadata differs from the payload: %s", bundle.name)
		}
		metadata, err := os.ReadFile(filepath.Join(bundle.path, "Contents/Resources/build-info.json"))
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, metadata) {
			return errors.New("Installer, support and app build identities differ")
		}
		if err = universalExecutable(filepath.Join(bundle.path, "Contents/MacOS", bundle.name)); err != nil {
			return err
		}
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if err = universalExecutable(filepath.Join(source, "bin", name)); err != nil {
			return err
		}
		wrapper, err := os.ReadFile(filepath.Join(source, "links", name))
		if err != nil {
			return err
		}
		if !strings.Contains(string(wrapper), "/.local/share/agent-heavy/bin/"+name) {
			return fmt.Errorf("Cask wrapper does not use the stable backend: %s", name)
		}
	}
	for _, required := range []string{"scripts/install-user.sh", "licenses/Go-LICENSE", "LICENSE", "THIRD-PARTY-NOTICES.md", "VERSION"} {
		if info, err := os.Stat(filepath.Join(source, required)); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("Required payload file missing: %s", required)
		}
	}
	if err = filepath.WalkDir(payload, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		switch name {
		case ".venv", "__pycache__", "remote.json", "state.json", "root.crt", "root.key", ".DS_Store":
			return fmt.Errorf("Private or obsolete runtime data in payload: %s", name)
		}
		if strings.HasSuffix(name, ".py") || strings.HasSuffix(name, ".pyc") {
			return errors.New("Python source or bytecode shipped in Go installer")
		}
		return nil
	}); err != nil {
		return err
	}
	cmd := exec.Command("codesign", "--verify", "--strict", "--deep", installer)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Installer code signature integrity failed: %s: %w", out, err)
	}
	fmt.Println("Universal native and Go payload, build identities, code signatures, licenses and private-state exclusion verified.")
	return nil
}
func universalExecutable(path string) error {
	cmd := exec.Command("xcrun", "lipo", "-archs", path)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	architectures := strings.Fields(string(out))
	sort.Strings(architectures)
	if len(architectures) != 2 || architectures[0] != "arm64" || architectures[1] != "x86_64" {
		return fmt.Errorf("Executable is not universal arm64/x86_64: %s", filepath.Base(path))
	}
	return nil
}
