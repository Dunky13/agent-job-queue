package tooling

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"github.com/Dunky13/agent-job-queue/internal/backend"
	"howett.net/plist"
)

// VerifyApp validates the drag-to-Applications app without executing its helpers
// or touching the installed queue. Older installer layouts are rejected: they
// lack the bundle-owned services and embedded support payload checked here.
func VerifyApp(app string) error {
	if app == "" {
		return errors.New("App path is required")
	}
	if err := verifyPayloadTree(app); err != nil {
		return err
	}
	managed, err := managedApp(app)
	if err != nil {
		return err
	}
	if !managed {
		return errors.New("App does not own its background services; installer-era layouts are not supported")
	}
	raw, err := verifySourceLayout(filepath.Join(app, "Contents/Resources/payload/source"), app)
	if err != nil {
		return err
	}
	if err := verifyNativeBundle(app, "AgentQueue", "io.went.agent-queue", raw); err != nil {
		return err
	}
	if err := verifyCodeSignature(app); err != nil {
		return err
	}
	fmt.Println("Standalone universal app, embedded Go payload, build identities, code signatures, licenses and private-state exclusion verified.")
	return nil
}

func verifySourceLayout(source, app string) ([]byte, error) {
	if err := verifyPayloadTree(source); err != nil {
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
		binary := filepath.Join(app, "Contents/MacOS", name)
		if err := universalExecutable(binary); err != nil {
			return nil, err
		}
		if err := verifyCodeSignature(binary); err != nil {
			return nil, err
		}
	}
	for _, required := range []string{"licenses/Go-LICENSE", "LICENSE", "README.md", "THIRD-PARTY-NOTICES.md", "VERSION"} {
		if err := requiredPayloadFile(filepath.Join(source, required), false); err != nil {
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
	if _, err := os.Lstat(filepath.Join(source, "bin")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("App must not embed source/bin")
	}
	if err := verifyManagedAgents(app); err != nil {
		return nil, err
	}
	// Reuse the runtime's no-follow ownership and permission boundary.
	// macOS temporary-directory ancestors may use their /var aliases.
	canonical, err := filepath.EvalSymlinks(app)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	if err := appservices.ValidateBundle(canonical); err != nil {
		return nil, err
	}
	return raw, nil
}

// Presence of a malformed marker must not select retained-artifact compatibility.
func managedApp(app string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(app, "Contents/Info.plist"))
	if err != nil {
		return false, err
	}
	var info map[string]any
	if _, err := plist.Unmarshal(raw, &info); err != nil {
		return false, err
	}
	marker, present := info["AgentQueueServiceManagement"]
	if present && marker != true {
		return false, errors.New("Invalid service management packaging marker")
	}
	return present, nil
}

func verifyManagedAgents(app string) error {
	directory := filepath.Join(app, "Contents/Library/LaunchAgents")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 3 {
		return errors.New("Managed app must contain exactly three bundled LaunchAgents")
	}
	for service, command := range map[string]string{"heavy": "daemon", "resources": "scheduler", "remote": "serve"} {
		helper := "agent-heavy"
		if service == "remote" {
			helper = "agent-remote"
		}
		label := "io.went.agent-" + service
		filename := filepath.Join(directory, label+".plist")
		if err := requiredPayloadFile(filename, false); err != nil {
			return err
		}
		raw, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		var actual map[string]any
		if _, err := plist.Unmarshal(raw, &actual); err != nil {
			return err
		}
		expected := map[string]any{"Label": label, "BundleProgram": "Contents/MacOS/" + helper, "ProgramArguments": []any{helper, command}, "ThrottleInterval": uint64(10), "Umask": uint64(63), "ProcessType": "Background"}
		if service == "resources" {
			expected["KeepAlive"] = true
		} else {
			expected["RunAtLoad"] = true
			expected["KeepAlive"] = map[string]any{"SuccessfulExit": false}
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("Invalid bundled LaunchAgent definition: %s", label)
		}
	}
	return nil
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
	metadata, err := os.ReadFile(filepath.Join(bundle, "Contents/Resources/build-info.json"))
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, metadata) {
		return errors.New("Support and app build identities differ")
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

func verifyPayloadTree(root string) error {
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
		if path != root && strings.HasSuffix(strings.ToLower(name), ".app") {
			return fmt.Errorf("Recursive app in payload: %s", path)
		}
		switch name {
		case ".venv", "__pycache__", "remote.json", "state.json", "root.crt", "root.key", ".DS_Store", ".git", "node_modules", "credentials.json", "install-in-progress.json":
			return fmt.Errorf("Private or obsolete runtime data in payload: %s", name)
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".py", ".pyc":
			return errors.New("Python source or bytecode shipped in the app")
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
