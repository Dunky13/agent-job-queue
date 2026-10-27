package tooling

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const signingTestTeam = "U9GXK89565"

func signingTestEnvironment(extra ...string) []string {
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "SIGNING_MODE" || name == "QUEUE_NOTARY_TOOL" || strings.HasPrefix(name, "APPLE_") || strings.HasPrefix(name, "NOTARY_") || strings.HasPrefix(name, "SIGNING_TEST_") || name == "BUILD_DMG" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, extra...)
}

func TestMacOSSigningRejectsIncompleteConfigurationBeforeBuild(t *testing.T) {
	script, err := filepath.Abs("../../scripts/build-installer.sh")
	if err != nil {
		t.Fatal(err)
	}
	identity := "APPLE_SIGNING_IDENTITY=Developer ID Application: Fixture (" + signingTestTeam + ")"
	for _, test := range []struct {
		name string
		env  []string
	}{
		{"unknown mode", []string{"SIGNING_MODE=release"}},
		{"identity in default mode", []string{identity}},
		{"team in adhoc mode", []string{"SIGNING_MODE=adhoc", "APPLE_TEAM_ID=" + signingTestTeam}},
		{"profile in default mode", []string{"NOTARY_KEYCHAIN_PROFILE=fixture"}},
		{"missing identity", []string{"SIGNING_MODE=developer-id", "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture"}},
		{"adhoc production identity", []string{"SIGNING_MODE=developer-id", "APPLE_SIGNING_IDENTITY=-", "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture"}},
		{"development certificate", []string{"SIGNING_MODE=developer-id", "APPLE_SIGNING_IDENTITY=Apple Development: Fixture", "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture"}},
		{"missing team", []string{"SIGNING_MODE=developer-id", identity, "NOTARY_KEYCHAIN_PROFILE=fixture"}},
		{"invalid team", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=bad-team", "NOTARY_KEYCHAIN_PROFILE=fixture"}},
		{"missing notary auth", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=" + signingTestTeam}},
		{"partial API key", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEY_ID=EXAMPLE1234"}},
		{"mixed credentials", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture", "NOTARY_ISSUER_ID=00000000-0000-0000-0000-000000000001"}},
		{"orphan Keychain", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN=missing.keychain-db"}},
		{"unreadable Keychain", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture", "NOTARY_KEYCHAIN=missing.keychain-db"}},
		{"invalid timeout", []string{"SIGNING_MODE=developer-id", identity, "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture", "NOTARY_TIMEOUT=0m"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			events := filepath.Join(root, "events")
			for _, tool := range []string{"go", "xcrun", "codesign"} {
				testTool(t, root, tool, `printf 'unexpected compilation or signing\n' >> "$SIGNING_TEST_EVENTS"; exit 93`)
			}
			cmd := exec.Command("/bin/bash", script)
			cmd.Env = signingTestEnvironment(append(test.env, "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "SIGNING_TEST_EVENTS="+events, "DIST_DIR="+filepath.Join(root, "artifacts"))...)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "macOS signing:") {
				t.Fatalf("configuration was not rejected: %v: %s", err, output)
			}
			if data, _ := os.ReadFile(events); len(data) != 0 {
				t.Fatalf("configuration failure reached a compiler or signing tool: %s", data)
			}
			if _, err := os.Stat(filepath.Join(root, "artifacts")); !os.IsNotExist(err) {
				t.Fatalf("configuration failure created artifacts: %v", err)
			}
		})
	}
}

type signingFixture struct {
	script, root, installer, destination, events string
	environment                                  []string
}

func newSigningFixture(t *testing.T) signingFixture {
	t.Helper()
	root := t.TempDir()
	script, err := filepath.Abs("../../scripts/macos-signing.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(root, "prepared Installer.app")
	for _, path := range []string{"Contents/MacOS/Installer", "Contents/Resources/payload/Agent Queue.app/Contents/MacOS/AgentQueue", "Contents/Resources/payload/source/bin/agent-heavy", "Contents/Resources/payload/source/bin/agent-remote"} {
		full := filepath.Join(installer, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("fixture executable"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "tools")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"codesign", "xcrun", "ditto", "hdiutil", "spctl", "syspolicy_check", "sync", "shasum", "plutil"} {
		testTool(t, bin, tool, signingTestTools)
	}
	tmp := filepath.Join(root, "temporary")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(root, "events")
	return signingFixture{
		script: script, root: root, installer: installer, destination: filepath.Join(root, "public assets"), events: events,
		environment: signingTestEnvironment("PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp,
			"SIGNING_TEST_EVENTS="+events, "SIGNING_TEST_FAILURE=", "SIGNING_TEST_TEAM="+signingTestTeam),
	}
}

func (f signingFixture) run(t *testing.T, extra []string, arguments ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/bash", append([]string{f.script}, arguments...)...)
	cmd.Env = append(append([]string{}, f.environment...), extra...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func signingProductionEnvironment() []string {
	return []string{"SIGNING_MODE=developer-id", "APPLE_SIGNING_IDENTITY=Developer ID Application: Fixture (" + signingTestTeam + ")", "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEYCHAIN_PROFILE=fixture-profile"}
}

func (f signingFixture) recorded(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.events)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertNotarySubmissionNames(t *testing.T, events, version string) {
	t.Helper()
	var names []string
	for _, line := range strings.Split(events, "\n") {
		for _, prefix := range []string{"xcrun|notarytool|submit|", "queue-tool|notarize|submit|--archive|"} {
			if archive, ok := strings.CutPrefix(line, prefix); ok {
				path, _, _ := strings.Cut(archive, "|")
				names = append(names, filepath.Base(path))
			}
		}
	}
	want := []string{
		"Agent-Job-Queue-Payload-" + version + "-universal.zip",
		"Agent-Job-Queue-Installer-" + version + "-universal.zip",
		"Agent-Job-Queue-Installer-" + version + "-universal.dmg",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected Apple submission names: %v, want %v", names, want)
	}
}

func (f signingFixture) embedAppSupport(t *testing.T) string {
	t.Helper()
	source := filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app/Contents/Resources/payload/source")
	if err := os.MkdirAll(filepath.Join(source, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if err := os.WriteFile(filepath.Join(source, "bin", name), []byte("embedded fixture helper"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "build-info.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestMacOSSigningDragInstallSignsEmbeddedHelpersBeforeApp(t *testing.T) {
	f := newSigningFixture(t)
	source := f.embedAppSupport(t)
	if output, err := f.run(t, signingProductionEnvironment(), "package", f.installer, f.destination, "1.2.3"); err != nil {
		t.Fatalf("drag packaging failed: %v: %s", err, output)
	}
	record := f.recorded(t)
	app := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(source))))
	seal := strings.Index(record, "|"+app+"\n")
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		sign := strings.Index(record, "|"+filepath.Join(source, "bin", name)+"\n")
		if sign < 0 || seal < sign || !strings.Contains(record, "|--identifier|io.went.agent-queue."+name+"|") {
			t.Fatalf("embedded helper was not signed before sealing the app: %s", record)
		}
	}
	if !strings.Contains(record, "disk-image-layout|Agent Job Queue Installer.app|automatic-setup\n") {
		t.Fatalf("disk image did not contain the complete automatic installer: %s", record)
	}
}

func TestMacOSSigningRejectsPartialEmbeddedSetupWithoutPublishing(t *testing.T) {
	f := newSigningFixture(t)
	source := f.embedAppSupport(t)
	if err := os.Remove(filepath.Join(source, "bin/agent-remote")); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run(t, signingProductionEnvironment(), "package", f.installer, f.destination, "1.2.3"); err == nil || !strings.Contains(output, "setup helper is missing") {
		t.Fatalf("partial embedded setup was packaged: %v: %s", err, output)
	}
	if entries, err := os.ReadDir(f.destination); !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
		t.Fatalf("failed drag package published artifacts: %v: %v", entries, err)
	}
}

func TestMacOSSigningRejectsDeclaredMissingAndLinkedPayload(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint("linked=", linked), func(t *testing.T) {
			f := newSigningFixture(t)
			app := filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app")
			if err := os.WriteFile(filepath.Join(app, "Contents/Info.plist"), []byte(`{"AgentQueueEmbeddedPayload":"true"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if linked {
				if err := os.MkdirAll(filepath.Join(app, "Contents/Resources"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing", filepath.Join(app, "Contents/Resources/payload")); err != nil {
					t.Fatal(err)
				}
			}
			output, err := f.run(t, signingProductionEnvironment(), "package", f.installer, f.destination, "1.2.3")
			if err == nil || !strings.Contains(output, "menu app") || strings.Contains(f.recorded(t), "notarytool|submit") {
				t.Fatalf("missing or linked setup fell back to legacy: %v: %s", err, output)
			}
		})
	}
}

func TestMacOSSigningPackagesInsideOutAndHashesStapledBytes(t *testing.T) {
	f := newSigningFixture(t)
	keychain := filepath.Join(f.root, "signing.keychain-db")
	if err := os.WriteFile(keychain, []byte("private fixture Keychain"), 0600); err != nil {
		t.Fatal(err)
	}
	environment := append(signingProductionEnvironment(), "APPLE_SIGNING_KEYCHAIN="+keychain, "NOTARY_TIMEOUT=7m")
	if output, err := f.run(t, environment, "package", f.installer, f.destination, "1.2.3"); err != nil {
		t.Fatalf("packaging failed: %v: %s", err, output)
	}
	record := f.recorded(t)
	assertNotarySubmissionNames(t, record, "1.2.3")
	ordered := []string{
		"xcrun|notarytool|submit|", "xcrun|notarytool|wait|00000000-0000-0000-0000-000000000001",
		"xcrun|notarytool|log|00000000-0000-0000-0000-000000000001", "xcrun|stapler|staple|" + filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app"),
		"syspolicy_check|distribution|" + filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app"),
		"codesign|--force|--sign|Developer ID Application: Fixture (" + signingTestTeam + ")|--timestamp|--options|runtime|--keychain|" + keychain + "|" + f.installer + "\n",
		"xcrun|notarytool|submit|", "xcrun|notarytool|wait|00000000-0000-0000-0000-000000000002",
		"xcrun|stapler|staple|" + f.installer + "\n", "hdiutil|create|", "xcrun|notarytool|submit|",
		"xcrun|notarytool|wait|00000000-0000-0000-0000-000000000003", "xcrun|stapler|staple|",
		"spctl|--assess|--type|open|--context|context:primary-signature", "shasum|-a|256|Agent-Job-Queue-Installer-1.2.3-universal.zip",
		"shasum|-a|256|Agent-Job-Queue-Installer-1.2.3-universal.dmg",
	}
	remaining := record
	for _, expected := range ordered {
		index := strings.Index(remaining, expected)
		if index < 0 {
			t.Fatalf("missing or out of order %q in:\n%s", expected, record)
		}
		remaining = remaining[index+len(expected):]
	}
	if strings.Contains(record, "--deep|--sign") || strings.Contains(record, "--sign|-|") || !strings.Contains(record, "--timeout|7m") {
		t.Fatalf("unexpected production signing options:\n%s", record)
	}
	for _, line := range strings.Split(record, "\n") {
		if strings.HasPrefix(line, "codesign|--force|--sign|") && (!strings.Contains(line, "|--keychain|"+keychain+"|") || strings.Contains(line, "|--deep|")) {
			t.Fatalf("signing must select the private Keychain without deep signing: %s", line)
		}
	}
	for _, path := range []string{f.installer, filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app"), filepath.Join(f.installer, "Contents/Resources/payload/source/bin/agent-heavy"), filepath.Join(f.installer, "Contents/Resources/payload/source/bin/agent-remote")} {
		for _, arch := range []string{"arm64", "x86_64"} {
			if !strings.Contains(record, "codesign|--display|--verbose=4|--architecture|"+arch+"|"+path+"\n") {
				t.Fatalf("signature metadata was not inspected for %s: %s", arch, path)
			}
		}
	}
	entries, err := os.ReadDir(f.destination)
	if err != nil || len(entries) != 4 {
		t.Fatalf("expected only final ZIP, DMG, and their checksums: %v: %v", entries, err)
	}
	for _, extension := range []string{"zip", "dmg"} {
		name := "Agent-Job-Queue-Installer-1.2.3-universal." + extension
		data, err := os.ReadFile(filepath.Join(f.destination, name))
		if err != nil {
			t.Fatal(err)
		}
		checksum, err := os.ReadFile(filepath.Join(f.destination, name+".sha256"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "stapled") || string(checksum) != fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name) {
			t.Fatalf("checksum does not describe the final stapled %s: %s", extension, checksum)
		}
	}
	logs, err := filepath.Glob(f.destination + ".notarization/notarization.*/*.log.json")
	if err != nil || len(logs) != 3 {
		t.Fatalf("all submission logs must survive outside public artifacts: %v: %v", logs, err)
	}
	image := filepath.Join(f.destination, "Agent-Job-Queue-Installer-1.2.3-universal.dmg")
	if output, err := f.run(t, nil, "verify-dmg", image, signingTestTeam); err != nil {
		t.Fatalf("credential-free disk image verification failed: %v: %s", err, output)
	}
	if output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=dmg-signature"}, "verify-dmg", image, signingTestTeam); err == nil {
		t.Fatalf("disk image verification accepted an invalid signature: %s", output)
	}
}

func TestMacOSSigningAdhocDoesNotUseProductionTools(t *testing.T) {
	f := newSigningFixture(t)
	if output, err := f.run(t, nil, "package", f.installer, f.destination, "1.2.3"); err != nil {
		t.Fatalf("ad hoc packaging failed: %v: %s", err, output)
	}
	record := f.recorded(t)
	for _, forbidden := range []string{"notarytool", "stapler", "syspolicy_check", "spctl", "--timestamp", "--options|runtime", "--test-requirement"} {
		if strings.Contains(record, forbidden) {
			t.Fatalf("ad hoc build used %s:\n%s", forbidden, record)
		}
	}
	if count := strings.Count(record, "codesign|--force|--sign|-|"); count != 4 {
		t.Fatalf("expected four explicit ad hoc signatures, got %d:\n%s", count, record)
	}
	if _, err := os.Stat(f.destination + ".notarization"); !os.IsNotExist(err) {
		t.Fatalf("ad hoc build created a notarization directory: %v", err)
	}
}

func TestMacOSSigningRejectsPrivatePathsInsideInstallerOrArtifacts(t *testing.T) {
	for _, test := range []struct {
		name, variable, location string
		alias, logDirectory      bool
	}{
		{"signing Keychain in payload", "APPLE_SIGNING_KEYCHAIN", "payload", false, false},
		{"notary Keychain in artifacts", "NOTARY_KEYCHAIN", "artifacts", false, false},
		{"API key in payload", "NOTARY_KEY_FILE", "payload", false, false},
		{"API key alias into artifacts", "NOTARY_KEY_FILE", "artifacts", true, false},
		{"signing Keychain alias into payload", "APPLE_SIGNING_KEYCHAIN", "payload", true, false},
		{"missing log directory in payload", "NOTARY_LOG_DIR", "payload", false, true},
		{"missing log directory in artifacts", "NOTARY_LOG_DIR", "artifacts", false, true},
		{"log parent alias into payload", "NOTARY_LOG_DIR", "payload", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSigningFixture(t)
			parent := filepath.Join(f.installer, "Contents/Resources/payload")
			if test.location == "artifacts" {
				parent = f.destination
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			privatePath := filepath.Join(parent, "private-input")
			if test.logDirectory {
				// Missing components, including '..', must be normalized without
				// creating anything within the sealed input bundle.
				privatePath = parent + "/missing/../private-logs"
			} else if err := os.WriteFile(privatePath, []byte("private fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			configuredPath := privatePath
			if test.alias {
				alias := filepath.Join(f.root, "apparently-external")
				target := privatePath
				if test.logDirectory {
					target = parent
				}
				if err := os.Symlink(target, alias); err != nil {
					t.Fatal(err)
				}
				configuredPath = alias
				if test.logDirectory {
					configuredPath += "/private-logs"
				}
			}
			environment := append(signingProductionEnvironment(), test.variable+"="+configuredPath)
			if test.variable == "NOTARY_KEY_FILE" {
				environment = append(environment, "NOTARY_KEYCHAIN_PROFILE=", "NOTARY_KEY_ID=EXAMPLE1234")
			}
			output, err := f.run(t, environment, "package", f.installer, f.destination, "1.2.3")
			if err == nil || !strings.Contains(output, "must be outside") {
				t.Fatalf("private path was not rejected: %v: %s", err, output)
			}
			if events, _ := os.ReadFile(f.events); len(events) != 0 {
				t.Fatalf("private path rejection happened after signing or archiving: %s", events)
			}
			if test.logDirectory {
				if _, err := os.Stat(filepath.Join(parent, "missing")); !os.IsNotExist(err) {
					t.Fatalf("log path validation changed the input bundle: %v", err)
				}
			}
		})
	}
}

func TestMacOSSigningFailurePublishesNothingAndRetainsSubmissionEvidence(t *testing.T) {
	for _, failure := range []string{"payload-rejected", "installer-rejected", "dmg-rejected", "payload-timeout", "payload-malformed-status", "payload-malformed-id", "payload-upload", "payload-log", "payload-staple", "payload-validate", "helper-signature", "intel-team", "missing-runtime", "missing-timestamp", "syspolicy", "gatekeeper", "dmg-signature"} {
		t.Run(failure, func(t *testing.T) {
			f := newSigningFixture(t)
			environment := append(signingProductionEnvironment(), "SIGNING_TEST_FAILURE="+failure)
			output, err := f.run(t, environment, "package", f.installer, f.destination, "1.2.3")
			if err == nil {
				t.Fatalf("%s did not stop packaging: %s", failure, output)
			}
			entries, readErr := os.ReadDir(f.destination)
			if readErr != nil && !os.IsNotExist(readErr) || len(entries) != 0 {
				t.Fatalf("failure published artifacts: %v: %v: %s", entries, readErr, output)
			}
			record := f.recorded(t)
			if strings.Contains(record, "shasum|") {
				t.Fatalf("failure reached artifact checksums:\n%s", record)
			}
			if strings.Contains(failure, "payload") && strings.Contains(record, "hdiutil|create|") {
				t.Fatalf("payload failure proceeded to disk image creation:\n%s", record)
			}
			if failure == "payload-timeout" {
				if !strings.Contains(output, "00000000-0000-0000-0000-000000000001") || !strings.Contains(record, "xcrun|notarytool|log|") {
					t.Fatalf("timeout lost the recovery identifier or did not attempt diagnostics: %s\n%s", output, record)
				}
				paths, _ := filepath.Glob(f.destination + ".notarization/notarization.*/payload.submission.json")
				if len(paths) != 1 {
					t.Fatalf("timeout lost the submission response: %v", paths)
				}
			}
			staging, err := os.ReadDir(filepath.Join(f.root, "temporary"))
			if err != nil || len(staging) != 0 {
				t.Fatalf("failed packaging retained unpublished archives: %v: %v", staging, err)
			}
		})
	}
}

func TestMacOSSigningAPIKeyAndCredentialFreeVerification(t *testing.T) {
	for _, issuer := range []string{"", "00000000-0000-0000-0000-000000000004"} {
		t.Run("issuer="+issuer, func(t *testing.T) {
			f := newSigningFixture(t)
			key := filepath.Join(f.root, "private-api-key.p8")
			const privateMarker = "PRIVATE-FIXTURE-KEY-MUST-NOT-LEAK"
			if err := os.WriteFile(key, []byte(privateMarker), 0600); err != nil {
				t.Fatal(err)
			}
			environment := []string{"SIGNING_MODE=developer-id", "APPLE_SIGNING_IDENTITY=Developer ID Application: Fixture (" + signingTestTeam + ")", "APPLE_TEAM_ID=" + signingTestTeam, "NOTARY_KEY_FILE=" + key, "NOTARY_KEY_ID=EXAMPLE1234", "BUILD_DMG=0"}
			if issuer != "" {
				environment = append(environment, "NOTARY_ISSUER_ID="+issuer)
			}
			output, err := f.run(t, environment, "package", f.installer, f.destination, "1.2.3")
			if err != nil {
				t.Fatalf("API key packaging failed: %v: %s", err, output)
			}
			record := f.recorded(t)
			if !strings.Contains(record, "|--key|"+key+"|--key-id|EXAMPLE1234|") || strings.Contains(record, "--keychain-profile") || (issuer == "") != !strings.Contains(record, "|--issuer|") {
				t.Fatalf("unexpected notarization authentication arguments:\n%s", record)
			}
			if strings.Contains(record, "hdiutil|create|") || strings.Contains(output, privateMarker) {
				t.Fatalf("unexpected disk image or credential disclosure: %s\n%s", output, record)
			}
			if err := filepath.WalkDir(f.destination+".notarization", func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if strings.Contains(string(data), privateMarker) {
					return fmt.Errorf("private key leaked into %s", path)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// This command has no identity, Keychain, or notary credentials.
			if output, err := f.run(t, nil, "verify-installer", f.installer, signingTestTeam); err != nil {
				t.Fatalf("credential-free installer verification failed: %v: %s", err, output)
			}
			if output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=intel-team"}, "verify-installer", f.installer, signingTestTeam); err == nil {
				t.Fatalf("verification accepted a wrong-team Intel signature: %s", output)
			}
			if err := os.Remove(filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app/.stub-staple")); err != nil {
				t.Fatal(err)
			}
			if output, err := f.run(t, nil, "verify-installer", f.installer, signingTestTeam); err == nil {
				t.Fatalf("verification accepted an unstapled installed payload: %s", output)
			}
		})
	}
}

// These tools never inspect the host signing identities, Keychain, or network.
// Their event log preserves argument boundaries, including paths with spaces.
const signingTestTools = `set -euo pipefail
tool=${0##*/}
{
  printf '%s' "$tool"
  printf '|%s' "$@"
  printf '\n'
} >> "$SIGNING_TEST_EVENTS"
failure=${SIGNING_TEST_FAILURE:-}
last=${!#}
case "$tool" in
  codesign)
    if [[ $1 == --force ]]; then
      case "$failure:$last" in
        payload-sign-heavy:*/agent-heavy) printf 'error: The specified item could not be found in the keychain.\n' >&2; exit 41;;
        payload-sign-remote:*/agent-remote) exit 42;;
        payload-sign-app:*/Agent\ Queue.app) exit 43;;
      esac
    fi
    if [[ $1 == --verify && $failure == payload-verify-heavy && $last == */agent-heavy ]]; then exit 44; fi
    # codesign accepts an inline requirement only with the '=' prefix;
    # otherwise this argument must name a readable requirements file.
    requirement_next=0
    for argument in "$@"; do
      if [[ $requirement_next == 1 ]]; then
        [[ $argument == =* || -r $argument ]] || { printf 'invalid requirement specification\n' >&2; exit 24; }
        requirement_next=0
      elif [[ $argument == --test-requirement ]]; then
        requirement_next=1
      fi
    done
    if [[ $failure == dmg-signature && $last == *.dmg ]]; then exit 17; fi
    if [[ $failure == helper-signature && $1 == --verify && $last == */agent-heavy ]]; then exit 16; fi
    if [[ $1 == --display ]]; then
      team=$SIGNING_TEST_TEAM
      if [[ $failure == intel-team && $* == *x86_64* ]]; then team=WRONG12345; fi
      printf 'Authority=Developer ID Application: Fixture\nTeamIdentifier=%s\n' "$team"
      if [[ $failure != missing-timestamp ]]; then printf 'Timestamp=Oct 21, 2026\n'; else printf 'Signed Time=Oct 21, 2026\n'; fi
      if [[ $failure != missing-runtime ]]; then printf 'CodeDirectory v=20500 size=123 flags=0x10000(runtime)\n'; else printf 'CodeDirectory v=20500 size=123 flags=0x0(none)\n'; fi
    fi
    ;;
  xcrun)
    case "$1:$2" in
      notarytool:submit)
        case "$3" in */Agent-Job-Queue-Payload-*-universal.zip) number=1; stage=payload;; */Agent-Job-Queue-Installer-*-universal.zip) number=2; stage=installer;; *.dmg) number=3; stage=dmg;; *) exit 92;; esac
        [[ $failure != "$stage-upload" ]] || exit 18
        if [[ $failure == "$stage-malformed-id" ]]; then printf '{}\n'; else printf '{"id":"00000000-0000-0000-0000-%012d"}\n' "$number"; fi
        ;;
      notarytool:wait)
        case "$3" in *001) stage=payload;; *002) stage=installer;; *003) stage=dmg;; *) exit 92;; esac
        if [[ $failure == "$stage-malformed-status" ]]; then printf '{}\n'; exit 0; fi
        status=Accepted
        [[ $failure != "$stage-rejected" ]] || status=Invalid
        [[ $failure != "$stage-timeout" ]] || status='In Progress'
        printf '{"status":"%s"}\n' "$status"
        [[ $failure != "$stage-timeout" ]] || exit 75
        ;;
      notarytool:log)
        case "$3" in *001) stage=payload;; *002) stage=installer;; *003) stage=dmg;; *) exit 92;; esac
        [[ $failure != "$stage-log" ]] || exit 19
        printf '{"issues":[]}\n' > "$last"
        ;;
      stapler:staple)
        stage=installer
        [[ $last != */payload/* ]] || stage=payload
        [[ $failure != "$stage-staple" ]] || exit 20
        if [[ -d $last ]]; then printf 'stapled' > "$last/.stub-staple"; else printf '\nstapled' >> "$last"; fi
        ;;
      stapler:validate)
        stage=installer
        [[ $last != */payload/* ]] || stage=payload
        [[ $failure != "$stage-validate" ]] || exit 21
        if [[ -d $last ]]; then test -f "$last/.stub-staple"; else grep -q stapled "$last"; fi
        ;;
      *) exit 91;;
    esac
    ;;
  plutil)
    if [[ $1 == -convert || $1 == -type || ( $1 == -extract && $2 == AgentQueueServiceManagement ) ]]; then
      exec /usr/bin/plutil "$@"
    fi
    value=$(sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p" "$last")
    [[ -n $value ]] || exit 1
    printf '%s\n' "$value"
    ;;
  ditto)
    if [[ $1 != -c && $1 != -x ]]; then
      shift $(($# - 2))
      exec /bin/cp -R "$1" "$2"
    fi
    shift $(($# - 2))
    printf 'archive' > "$2"
    if [[ -f $1/.stub-staple ]]; then printf ' stapled' >> "$2"; fi
    if [[ -f "$1/Contents/Resources/payload/Agent Queue.app/.stub-staple" ]]; then printf ' payload-stapled' >> "$2"; fi
    ;;
  hdiutil)
    if [[ $1 == create ]]; then
      source_next=0
      for argument in "$@"; do
        if [[ $source_next == 1 ]]; then image_source=$argument; source_next=0; fi
        if [[ $argument == -srcfolder ]]; then source_next=1; fi
      done
      if [[ -d "$image_source/Agent Queue.app" ]]; then
        [[ -L "$image_source/Applications" && $(readlink "$image_source/Applications") == /Applications ]]
        [[ ! -e "$image_source/Agent Job Queue Installer.app" ]]
        printf 'disk-image-layout|Agent Queue.app|Applications->/Applications\n' >> "$SIGNING_TEST_EVENTS"
      elif [[ -f "$image_source/Contents/MacOS/Installer" ]]; then
        [[ ! -e "$image_source/Applications" && ! -e "$image_source/Agent Queue.app" ]]
        printf 'disk-image-layout|Agent Job Queue Installer.app|automatic-setup\n' >> "$SIGNING_TEST_EVENTS"
      else
        exit 91
      fi
      printf 'disk image' > "$last"
    fi
    ;;
  syspolicy_check) [[ $failure != syspolicy ]] || exit 22;;
  spctl) [[ $failure != gatekeeper ]] || exit 23;;
  shasum) exec /usr/bin/shasum "$@";;
  sync) ;;
  *) exit 90;;
esac
`

func (f signingFixture) embedManagedApp(t *testing.T) string {
	t.Helper()
	source := f.embedAppSupport(t)
	app := filepath.Join(f.installer, "Contents/Resources/payload/Agent Queue.app")
	verificationWrite(t, filepath.Join(app, "Contents/Info.plist"), `<?xml version="1.0"?><plist version="1.0"><dict><key>AgentQueueServiceManagement</key><true/></dict></plist>`, 0600)
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		if err := os.Rename(filepath.Join(source, "bin", name), filepath.Join(app, "Contents/MacOS", name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(source, "bin")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"heavy", "resources", "remote"} {
		filename := "io.went.agent-" + name + ".plist"
		raw, err := os.ReadFile(filepath.Join("../../native/LaunchAgents", filename))
		if err != nil {
			t.Fatal(err)
		}
		verificationWrite(t, filepath.Join(app, "Contents/Library/LaunchAgents", filename), string(raw), 0600)
	}
	return app
}

func TestMacOSSigningManagedHelpersInsideOut(t *testing.T) {
	f := newSigningFixture(t)
	app := f.embedManagedApp(t)
	if output, err := f.run(t, signingProductionEnvironment(), "package", f.installer, f.destination, "1.2.3"); err != nil {
		t.Fatalf("managed signing: %v: %s", err, output)
	}
	record := f.recorded(t)
	seal := strings.Index(record, "codesign|--force|--sign|Developer ID Application: Fixture ("+signingTestTeam+")|--timestamp|--options|runtime|"+app+"\n")
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		path := filepath.Join(app, "Contents/MacOS", name)
		sign := strings.Index(record, "|--identifier|io.went.agent-queue."+name+"|"+path+"\n")
		if sign < 0 || seal < sign {
			t.Fatalf("canonical helper not sealed before app: %s", record)
		}
		for _, arch := range []string{"arm64", "x86_64"} {
			if !strings.Contains(record, "codesign|--display|--verbose=4|--architecture|"+arch+"|"+path+"\n") {
				t.Fatalf("canonical helper not verified: %s", record)
			}
		}
	}
	if !strings.Contains(record, "disk-image-layout|Agent Job Queue Installer.app|automatic-setup\n") {
		t.Fatalf("managed release lost the automatic installer layout: %s", record)
	}
	assertNotarySubmissionNames(t, record, "1.2.3")
}

func TestMacOSSigningRejectsManagedLayoutTampering(t *testing.T) {
	for _, mutation := range []string{"duplicate bin", "missing helper", "missing plist", "modified plist", "extra login plist", "false marker"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSigningFixture(t)
			app := f.embedManagedApp(t)
			path := filepath.Join(app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")
			switch mutation {
			case "duplicate bin":
				verificationWrite(t, filepath.Join(app, "Contents/Resources/payload/source/bin/agent-heavy"), "duplicate", 0700)
			case "missing helper":
				if err := os.Remove(filepath.Join(app, "Contents/MacOS/agent-remote")); err != nil {
					t.Fatal(err)
				}
			case "missing plist":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "modified plist":
				raw, _ := os.ReadFile(path)
				verificationWrite(t, path, strings.Replace(string(raw), "<integer>63</integer>", "<integer>0</integer>", 1), 0600)
			case "extra login plist":
				verificationWrite(t, filepath.Join(filepath.Dir(path), "io.went.agent-queue.plist"), "login", 0600)
			case "false marker":
				verificationWrite(t, filepath.Join(app, "Contents/Info.plist"), `<?xml version="1.0"?><plist version="1.0"><dict><key>AgentQueueServiceManagement</key><false/></dict></plist>`, 0600)
			}
			if output, err := f.run(t, signingProductionEnvironment(), "package", f.installer, f.destination, "1.2.3"); err == nil {
				t.Fatalf("managed tampering accepted: %s", output)
			}
			if strings.Contains(f.recorded(t), "notarytool|submit") {
				t.Fatal("tampered managed app reached Apple submission")
			}
		})
	}
}
