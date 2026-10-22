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

// VerifyInstaller checks the distributed installer and both copies of its Go
// payload. Only retained legacy installers may omit the app's embedded payload.
func VerifyInstaller(installer string) error {
	if installer == "" {
		return errors.New("Installer path is required")
	}
	payload := filepath.Join(installer, "Contents/Resources/payload")
	raw, err := verifySource(filepath.Join(payload, "source"))
	if err != nil {
		return err
	}
	if err = verifyNativeBundle(installer, "Installer", "io.went.agent-queue.installer", raw); err != nil {
		return err
	}
	if err = verifyApp(filepath.Join(payload, "Agent Queue.app"), raw, false); err != nil {
		return err
	}
	if err = verifyPayloadTree(payload, false); err != nil {
		return err
	}
	if err = verifyCodeSignature(installer); err != nil {
		return err
	}
	fmt.Println("Universal native and Go payload, build identities, code signatures, licenses and private-state exclusion verified.")
	return nil
}

// VerifyApp validates a drag-to-Applications app without executing its helpers or
// touching the installed queue. A standalone app must contain its source payload.
func VerifyApp(app string) error {
	if app == "" {
		return errors.New("App path is required")
	}
	if err := verifyApp(app, nil, true); err != nil {
		return err
	}
	fmt.Println("Standalone universal app, embedded Go payload, build identities, code signatures, licenses and private-state exclusion verified.")
	return nil
}

func verifyApp(app string, expected []byte, requirePayload bool) error {
	if err := verifyPayloadTree(app, true); err != nil {
		return err
	}
	payload := filepath.Join(app, "Contents/Resources/payload")
	_, err := os.Lstat(payload)
	switch {
	case errors.Is(err, os.ErrNotExist) && !requirePayload:
		// Old retained installers predate the self-contained app. A present but
		// partial payload must never take this compatibility path.
	case err != nil:
		return fmt.Errorf("App embedded payload is missing: %w", err)
	default:
		raw, err := verifySource(filepath.Join(payload, "source"))
		if err != nil {
			return err
		}
		if expected != nil && !bytes.Equal(expected, raw) {
			return errors.New("Installer and embedded app source build identities differ")
		}
		expected = raw
	}
	if err := verifyNativeBundle(app, "AgentQueue", "io.went.agent-queue", expected); err != nil {
		return err
	}
	return verifyCodeSignature(app)
}

func verifySource(source string) ([]byte, error) {
	if err := verifyPayloadTree(source, true); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(source, "build-info.json"))
	if err != nil {
		return nil, err
	}
	identity, err := backend.ValidateBuildInfo(raw)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		binary := filepath.Join(source, "bin", name)
		if err := universalExecutable(binary); err != nil {
			return nil, err
		}
		if err := verifyCodeSignature(binary); err != nil {
			return nil, err
		}
		wrapperPath := filepath.Join(source, "links", name)
		if err := requiredPayloadFile(wrapperPath, true); err != nil {
			return nil, err
		}
		wrapper, err := os.ReadFile(wrapperPath)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(wrapper), "/.local/share/agent-heavy/bin/"+name) {
			return nil, fmt.Errorf("Cask wrapper does not use the stable backend: %s", name)
		}
	}
	for _, required := range []string{"scripts/install-user.sh", "licenses/Go-LICENSE", "LICENSE", "README.md", "THIRD-PARTY-NOTICES.md", "VERSION"} {
		if err := requiredPayloadFile(filepath.Join(source, required), required == "scripts/install-user.sh"); err != nil {
			return nil, err
		}
	}
	version, err := os.ReadFile(filepath.Join(source, "VERSION"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(version)) != identity.Version {
		return nil, errors.New("Payload VERSION differs from its build identity")
	}
	return raw, nil
}

func verifyNativeBundle(bundle, executable, identifier string, raw []byte) error {
	identity, err := backend.ValidateBuildInfo(raw)
	if err != nil {
		return err
	}
	build := "0"
	if identity.Build != nil {
		build = strconv.Itoa(*identity.Build)
	}
	infoBytes, err := os.ReadFile(filepath.Join(bundle, "Contents/Info.plist"))
	if err != nil {
		return err
	}
	var info map[string]any
	if _, err = plist.Unmarshal(infoBytes, &info); err != nil {
		return err
	}
	if info["LSMinimumSystemVersion"] != "15.0" || info["CFBundleIdentifier"] != identifier || info["CFBundleExecutable"] != executable || info["CFBundleShortVersionString"] != identity.Version || info["CFBundleVersion"] != build {
		return fmt.Errorf("Native bundle metadata differs from the payload: %s", executable)
	}
	if identifier == "io.went.agent-queue" && info["AgentQueueEmbeddedPayload"] == true {
		if info, err := os.Lstat(filepath.Join(bundle, "Contents/Resources/payload/source")); err != nil || !info.IsDir() {
			return errors.New("App declares an embedded setup payload but it is missing")
		}
	}
	metadata, err := os.ReadFile(filepath.Join(bundle, "Contents/Resources/build-info.json"))
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, metadata) {
		return errors.New("Installer, support and app build identities differ")
	}
	return universalExecutable(filepath.Join(bundle, "Contents/MacOS", executable))
}

func requiredPayloadFile(path string, executable bool) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("Required payload file missing or invalid: %s", path)
	}
	if executable && info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("Payload executable lacks executable permission: %s", path)
	}
	return nil
}

func verifyPayloadTree(root string, rejectApps bool) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("Payload directory is missing or invalid: %s", root)
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("Symlink or special file in payload: %s", path)
		}
		if rejectApps && path != root && strings.HasSuffix(strings.ToLower(name), ".app") {
			return fmt.Errorf("Recursive app in payload: %s", path)
		}
		switch name {
		case ".venv", "__pycache__", "remote.json", "state.json", "root.crt", "root.key", ".DS_Store", ".git", "node_modules", "credentials.json", "install-in-progress.json":
			return fmt.Errorf("Private or obsolete runtime data in payload: %s", name)
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".py", ".pyc":
			return errors.New("Python source or bytecode shipped in Go installer")
		case ".sqlite", ".sqlite3", ".db", ".log", ".key", ".pem", ".p12", ".pfx":
			return fmt.Errorf("Private runtime data in payload: %s", name)
		}
		return nil
	})
}

func verifyCodeSignature(path string) error {
	cmd := exec.Command("codesign", "--verify", "--strict", "--deep", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Code signature integrity failed for %s: %s: %w", filepath.Base(path), out, err)
	}
	return nil
}

func universalExecutable(path string) error {
	if err := requiredPayloadFile(path, true); err != nil {
		return err
	}
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
