package tooling

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

const (
	sparkleFeedURL   = "https://dunky13.github.io/agent-job-queue/appcast.xml"
	sparkleFramework = backend.SparkleFramework
)

// sparkleLinks are the only symlinks the trimmed framework may contain. The headers,
// modules and XPC services are removed from the app, which is not sandboxed. Setup
// accepts the same links.
var sparkleLinks = backend.SparkleLinks()

// verifySparkle checks the one framework layout the app may carry: exactly Updater.app
// as nested code, no XPC services, universal Mach-O files and the app's own signing team.
func verifySparkle(app string) error {
	frameworks := filepath.Join(app, "Contents/Frameworks")
	entries, err := os.ReadDir(frameworks)
	if err != nil {
		return fmt.Errorf("App is missing Sparkle: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "Sparkle.framework" || !entries[0].IsDir() {
		return errors.New("App must contain exactly Sparkle.framework in Contents/Frameworks")
	}
	framework := filepath.Join(app, sparkleFramework)
	links := map[string]bool{}
	err = filepath.WalkDir(framework, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(framework, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		name := entry.Name()
		if name == "XPCServices" || strings.HasSuffix(name, ".xpc") {
			return fmt.Errorf("Sparkle XPC services must be removed from the app: %s", relative)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil || sparkleLinks[relative] != target {
				return fmt.Errorf("Unexpected symlink in Sparkle.framework: %s", relative)
			}
			links[relative] = true
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("Special file in Sparkle.framework: %s", relative)
		}
		if entry.IsDir() && relative != "Versions/B/Updater.app" && (strings.HasSuffix(name, ".app") || strings.HasSuffix(name, ".framework")) {
			return fmt.Errorf("Unexpected nested code in Sparkle.framework: %s", relative)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(links) != len(sparkleLinks) {
		return errors.New("Sparkle.framework does not have the expected layout")
	}
	version := filepath.Join(framework, "Versions/B")
	for _, path := range []string{filepath.Join(version, "Sparkle"), filepath.Join(version, "Autoupdate"), filepath.Join(version, "Updater.app/Contents/MacOS/Updater")} {
		if err := universalExecutable(path); err != nil {
			return err
		}
	}
	team, err := teamIdentifier(app)
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(version, "Autoupdate"), filepath.Join(version, "Updater.app"), framework} {
		if err := verifyCodeSignature(path); err != nil {
			return err
		}
		if other, err := teamIdentifier(path); err != nil || other != team {
			return fmt.Errorf("Sparkle code is not signed by the app's team: %s", filepath.Base(path))
		}
	}
	return nil
}

// looseCodeSuffixes name code containers and libraries that the app never carries.
var looseCodeSuffixes = []string{".dylib", ".so", ".bundle", ".plugin", ".kext", ".appex", ".xpc", ".framework", ".app"}

// machOExecutables are the only Mach-O files the app may hold outside Sparkle.framework, relative to the app.
var machOExecutables = []string{"Contents/MacOS/AgentQueue", "Contents/MacOS/agent-heavy", "Contents/MacOS/agent-remote"}

// sparkleMachO are the Mach-O files of the trimmed framework: its library, Autoupdate and Updater.app's executable.
var sparkleMachO = []string{"Versions/B/Sparkle", "Versions/B/Autoupdate", "Versions/B/Updater.app/Contents/MacOS/Updater"}

// verifyNoLooseCode rejects code anywhere in the app outside the allowed layout: the
// app's own executables, Sparkle.framework and, inside it, exactly Updater.app. Libraries,
// plugins, bundles and any other Mach-O file are loose code that nothing signs or reviews.
func verifyNoLooseCode(app string) error {
	framework := filepath.Join(app, sparkleFramework)
	allowedFiles := map[string]bool{}
	for _, name := range machOExecutables {
		allowedFiles[filepath.Join(app, name)] = true
	}
	for _, name := range sparkleMachO {
		allowedFiles[filepath.Join(framework, name)] = true
	}
	return filepath.WalkDir(app, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == app || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, _ := filepath.Rel(app, path)
		name := strings.ToLower(entry.Name())
		for _, suffix := range looseCodeSuffixes {
			if !strings.HasSuffix(name, suffix) {
				continue
			}
			if path == framework || path == filepath.Join(framework, "Versions/B/Updater.app") {
				return nil
			}
			return fmt.Errorf("Loose code outside the allowed layout: %s", relative)
		}
		if entry.IsDir() || allowedFiles[path] {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		magic := make([]byte, 4)
		if n, _ := file.Read(magic); n == 4 && isMachOMagic(magic) {
			return fmt.Errorf("Mach-O file outside the allowed layout: %s", relative)
		}
		return nil
	})
}

func isMachOMagic(magic []byte) bool {
	for _, known := range [][]byte{{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
		{0xca, 0xfe, 0xba, 0xbe}, {0xca, 0xfe, 0xba, 0xbf}, {0xbe, 0xba, 0xfe, 0xca}, {0xbf, 0xba, 0xfe, 0xca}} {
		if string(magic) == string(known) {
			return true
		}
	}
	return false
}

// teamIdentifier reads the signing team, or "not set" for ad hoc code.
func teamIdentifier(path string) (string, error) {
	out, err := exec.Command("codesign", "--display", "--verbose=2", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Could not read the code signature of %s: %w", filepath.Base(path), err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if team, ok := strings.CutPrefix(line, "TeamIdentifier="); ok {
			return strings.TrimSpace(team), nil
		}
	}
	return "", fmt.Errorf("No team identifier in the code signature of %s", filepath.Base(path))
}

var sparkleKey = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// verifySparkleConfiguration pins how the app finds, authenticates and verifies updates.
// A tracked (published) build must carry the public EdDSA key; an untracked one may omit it.
func verifySparkleConfiguration(info map[string]any, tracked bool) error {
	if info["SUFeedURL"] != sparkleFeedURL || info["SURequireSignedFeed"] != true || info["SUVerifyUpdateBeforeExtraction"] != true {
		return errors.New("App does not require a signed Sparkle feed verified before extraction")
	}
	if _, present := info["SUEnableAutomaticChecks"]; present {
		return errors.New("SUEnableAutomaticChecks must stay unset so Sparkle asks for consent")
	}
	if _, present := info["NSDownloadsFolderUsageDescription"]; present {
		return errors.New("The app no longer writes to Downloads")
	}
	key, present := info["SUPublicEDKey"]
	if !present {
		if tracked {
			return errors.New("A published build must carry SUPublicEDKey")
		}
		return nil
	}
	text, ok := key.(string)
	if !ok || !sparkleKey.MatchString(text) {
		return errors.New("SUPublicEDKey is not a base64 Ed25519 public key")
	}
	if raw, err := base64.StdEncoding.DecodeString(text); err != nil || len(raw) != 32 {
		return errors.New("SUPublicEDKey is not a base64 Ed25519 public key")
	}
	return nil
}
