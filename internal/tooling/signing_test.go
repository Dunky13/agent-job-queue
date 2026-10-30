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

func TestMacOSSignerDetailsConsumeCompleteMetadata(t *testing.T) {
	script, err := filepath.Abs("../../scripts/macos-signing.sh")
	if err != nil {
		t.Fatal(err)
	}
	fields := []string{
		"TeamIdentifier=" + signingTestTeam,
		"Authority=Developer ID Application: Fixture",
		"Timestamp=Oct 21, 2026",
		"CodeDirectory v=20500 size=123 flags=0x10000(runtime)",
	}
	// A match near the start must not close a pipe while Bash is still writing
	// metadata. Keep enough trailing data to exceed the operating system buffer.
	padding := strings.Repeat("Unrelated signing metadata\n", 40000)
	for _, test := range []struct {
		name    string
		omit    int
		runtime string
		wantOK  bool
	}{
		{"universal code", -1, "1", true},
		{"disk image", 3, "0", true},
		{"missing team", 0, "1", false},
		{"missing authority", 1, "1", false},
		{"missing timestamp", 2, "1", false},
		{"missing runtime", 3, "1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var details strings.Builder
			for index, field := range fields {
				if index != test.omit {
					details.WriteString(field + "\n")
				}
			}
			details.WriteString(padding)
			path := filepath.Join(t.TempDir(), "details")
			if err := os.WriteFile(path, []byte(details.String()), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/bash", "-c", `set -euo pipefail
source "$1"
details=$(cat "$2")
macos_verify_signer_details fixture "$details" "$3"`, "signer-details-test", script, path, test.runtime)
			cmd.Env = signingTestEnvironment("APPLE_TEAM_ID=" + signingTestTeam)
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantOK {
				t.Fatalf("signer verification success=%t, want %t: %v: %s", err == nil, test.wantOK, err, output)
			}
			if strings.Contains(string(output), "Broken pipe") {
				t.Fatalf("verification closed its metadata input early: %s", output)
			}
		})
	}
}

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
	script, err := filepath.Abs("../../scripts/build-app.sh")
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
	script, root, app, destination, events string
	environment                            []string
}

const signingTestInfoPlist = `<?xml version="1.0"?><plist version="1.0"><dict><key>AgentQueueEmbeddedPayload</key><true/><key>AgentQueueServiceManagement</key><true/></dict></plist>`

// signingTestApp writes the self-contained layout build-app.sh produces:
// helpers beside the executable, bundle-owned agents and a support payload.
func signingTestApp(t *testing.T, app string) {
	t.Helper()
	for _, path := range []string{"Contents/MacOS/AgentQueue", "Contents/MacOS/agent-heavy", "Contents/MacOS/agent-remote"} {
		verificationWrite(t, filepath.Join(app, filepath.FromSlash(path)), "fixture executable", 0700)
	}
	verificationWrite(t, filepath.Join(app, "Contents/Info.plist"), signingTestInfoPlist, 0600)
	verificationWrite(t, filepath.Join(app, "Contents/Resources/payload/source/build-info.json"), "{}", 0600)
	for _, name := range []string{"heavy", "resources", "remote"} {
		filename := "io.went.agent-" + name + ".plist"
		raw, err := os.ReadFile(filepath.Join("../../native/LaunchAgents", filename))
		if err != nil {
			t.Fatal(err)
		}
		verificationWrite(t, filepath.Join(app, "Contents/Library/LaunchAgents", filename), string(raw), 0600)
	}
}

func newSigningFixture(t *testing.T) signingFixture {
	t.Helper()
	root := t.TempDir()
	script, err := filepath.Abs("../../scripts/macos-signing.sh")
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(root, "prepared Agent Queue.app")
	signingTestApp(t, app)
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
		script: script, root: root, app: app, destination: filepath.Join(root, "public assets"), events: events,
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
		"Agent-Queue-" + version + "-universal.zip",
		"Agent-Queue-" + version + "-universal.dmg",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected Apple submission names: %v, want %v", names, want)
	}
}

func TestMacOSSigningSignsBundledHelpersBeforeApp(t *testing.T) {
	f := newSigningFixture(t)
	if output, err := f.run(t, signingProductionEnvironment(), "package", f.app, f.destination, "1.2.3"); err != nil {
		t.Fatalf("app packaging failed: %v: %s", err, output)
	}
	record := f.recorded(t)
	seal := strings.Index(record, "codesign|--force|--sign|Developer ID Application: Fixture ("+signingTestTeam+")|--timestamp|--options|runtime|"+f.app+"\n")
	for _, name := range []string{"agent-heavy", "agent-remote"} {
		path := filepath.Join(f.app, "Contents/MacOS", name)
		sign := strings.Index(record, "|--identifier|io.went.agent-queue."+name+"|"+path+"\n")
		if sign < 0 || seal < sign {
			t.Fatalf("bundled helper was not signed before sealing the app: %s", record)
		}
		for _, arch := range []string{"arm64", "x86_64"} {
			if !strings.Contains(record, "codesign|--display|--verbose=4|--architecture|"+arch+"|"+path+"\n") {
				t.Fatalf("bundled helper signature was not inspected for %s: %s", arch, path)
			}
		}
	}
	if !strings.Contains(record, "disk-image-layout|Agent Queue.app|Applications->/Applications\n") {
		t.Fatalf("disk image did not contain the app and the Applications link: %s", record)
	}
	assertNotarySubmissionNames(t, record, "1.2.3")
}

func TestMacOSSigningRejectsPartialAppWithoutPublishing(t *testing.T) {
	for _, test := range []struct{ missing, message string }{
		{"Contents/MacOS/agent-remote", "bundled helpers are required"},
		{"Contents/Resources/payload/source/build-info.json", "setup payload is incomplete"},
	} {
		t.Run(test.missing, func(t *testing.T) {
			f := newSigningFixture(t)
			if err := os.Remove(filepath.Join(f.app, test.missing)); err != nil {
				t.Fatal(err)
			}
			if output, err := f.run(t, signingProductionEnvironment(), "package", f.app, f.destination, "1.2.3"); err == nil || !strings.Contains(output, test.message) {
				t.Fatalf("partial app was packaged: %v: %s", err, output)
			}
			if entries, err := os.ReadDir(f.destination); !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
				t.Fatalf("failed packaging published artifacts: %v: %v", entries, err)
			}
		})
	}
}

func TestMacOSSigningRejectsDeclaredMissingAndLinkedPayload(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint("linked=", linked), func(t *testing.T) {
			f := newSigningFixture(t)
			payload := filepath.Join(f.app, "Contents/Resources/payload")
			if err := os.RemoveAll(payload); err != nil {
				t.Fatal(err)
			}
			if linked {
				if err := os.Symlink("missing", payload); err != nil {
					t.Fatal(err)
				}
			}
			output, err := f.run(t, signingProductionEnvironment(), "package", f.app, f.destination, "1.2.3")
			if err == nil || !strings.Contains(output, "menu app") || strings.Contains(f.recorded(t), "notarytool|submit") {
				t.Fatalf("missing or linked setup payload was accepted: %v: %s", err, output)
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
	if output, err := f.run(t, environment, "package", f.app, f.destination, "1.2.3"); err != nil {
		t.Fatalf("packaging failed: %v: %s", err, output)
	}
	record := f.recorded(t)
	assertNotarySubmissionNames(t, record, "1.2.3")
	ordered := []string{
		"codesign|--force|--sign|Developer ID Application: Fixture (" + signingTestTeam + ")|--timestamp|--options|runtime|--keychain|" + keychain + "|" + f.app + "\n",
		"xcrun|notarytool|submit|", "xcrun|notarytool|wait|00000000-0000-0000-0000-000000000001",
		"xcrun|notarytool|log|00000000-0000-0000-0000-000000000001", "xcrun|stapler|staple|" + f.app + "\n",
		"syspolicy_check|distribution|" + f.app + "|--verbose\n",
		"hdiutil|create|", "xcrun|notarytool|submit|",
		"xcrun|notarytool|wait|00000000-0000-0000-0000-000000000002", "xcrun|stapler|staple|",
		"spctl|--assess|--type|open|--context|context:primary-signature", "shasum|-a|256|Agent-Queue-1.2.3-universal.zip",
		"shasum|-a|256|Agent-Queue-1.2.3-universal.dmg",
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
	for _, path := range []string{f.app, filepath.Join(f.app, "Contents/MacOS/agent-heavy"), filepath.Join(f.app, "Contents/MacOS/agent-remote")} {
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
		name := "Agent-Queue-1.2.3-universal." + extension
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
	if err != nil || len(logs) != 2 {
		t.Fatalf("all submission logs must survive outside public artifacts: %v: %v", logs, err)
	}
	image := filepath.Join(f.destination, "Agent-Queue-1.2.3-universal.dmg")
	if output, err := f.run(t, nil, "verify-dmg", image, signingTestTeam); err != nil {
		t.Fatalf("credential-free disk image verification failed: %v: %s", err, output)
	}
	if output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=dmg-signature"}, "verify-dmg", image, signingTestTeam); err == nil {
		t.Fatalf("disk image verification accepted an invalid signature: %s", output)
	}
}

func TestMacOSSigningAdhocDoesNotUseProductionTools(t *testing.T) {
	f := newSigningFixture(t)
	if output, err := f.run(t, nil, "package", f.app, f.destination, "1.2.3"); err != nil {
		t.Fatalf("ad hoc packaging failed: %v: %s", err, output)
	}
	record := f.recorded(t)
	for _, forbidden := range []string{"notarytool", "stapler", "syspolicy_check", "spctl", "--timestamp", "--options|runtime", "--test-requirement"} {
		if strings.Contains(record, forbidden) {
			t.Fatalf("ad hoc build used %s:\n%s", forbidden, record)
		}
	}
	if count := strings.Count(record, "codesign|--force|--sign|-|"); count != 3 {
		t.Fatalf("expected three explicit ad hoc signatures, got %d:\n%s", count, record)
	}
	if _, err := os.Stat(f.destination + ".notarization"); !os.IsNotExist(err) {
		t.Fatalf("ad hoc build created a notarization directory: %v", err)
	}
}

func TestMacOSSigningRejectsPrivatePathsInsideAppOrArtifacts(t *testing.T) {
	for _, test := range []struct {
		name, variable, location string
		alias, logDirectory      bool
	}{
		{"signing Keychain in app", "APPLE_SIGNING_KEYCHAIN", "app", false, false},
		{"notary Keychain in artifacts", "NOTARY_KEYCHAIN", "artifacts", false, false},
		{"API key in app", "NOTARY_KEY_FILE", "app", false, false},
		{"API key alias into artifacts", "NOTARY_KEY_FILE", "artifacts", true, false},
		{"signing Keychain alias into app", "APPLE_SIGNING_KEYCHAIN", "app", true, false},
		{"missing log directory in app", "NOTARY_LOG_DIR", "app", false, true},
		{"missing log directory in artifacts", "NOTARY_LOG_DIR", "artifacts", false, true},
		{"log parent alias into app", "NOTARY_LOG_DIR", "app", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSigningFixture(t)
			parent := filepath.Join(f.app, "Contents/Resources")
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
			output, err := f.run(t, environment, "package", f.app, f.destination, "1.2.3")
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
	for _, failure := range []string{"app-rejected", "dmg-rejected", "app-timeout", "app-malformed-status", "app-malformed-id", "app-upload", "app-log", "app-staple", "app-validate", "helper-signature", "intel-team", "missing-runtime", "missing-timestamp", "syspolicy", "gatekeeper", "dmg-signature"} {
		t.Run(failure, func(t *testing.T) {
			f := newSigningFixture(t)
			environment := append(signingProductionEnvironment(), "SIGNING_TEST_FAILURE="+failure)
			output, err := f.run(t, environment, "package", f.app, f.destination, "1.2.3")
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
			if strings.HasPrefix(failure, "app-") && strings.Contains(record, "hdiutil|create|") {
				t.Fatalf("app failure proceeded to disk image creation:\n%s", record)
			}
			if failure == "app-timeout" {
				if !strings.Contains(output, "00000000-0000-0000-0000-000000000001") || !strings.Contains(record, "xcrun|notarytool|log|") {
					t.Fatalf("timeout lost the recovery identifier or did not attempt diagnostics: %s\n%s", output, record)
				}
				paths, _ := filepath.Glob(f.destination + ".notarization/notarization.*/app.submission.json")
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
			output, err := f.run(t, environment, "package", f.app, f.destination, "1.2.3")
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
			if output, err := f.run(t, nil, "verify-app", f.app, signingTestTeam); err != nil {
				t.Fatalf("credential-free app verification failed: %v: %s", err, output)
			}
			if output, err := f.run(t, []string{"SIGNING_TEST_FAILURE=intel-team"}, "verify-app", f.app, signingTestTeam); err == nil {
				t.Fatalf("verification accepted a wrong-team Intel signature: %s", output)
			}
			if err := os.Remove(filepath.Join(f.app, ".stub-staple")); err != nil {
				t.Fatal(err)
			}
			if output, err := f.run(t, nil, "verify-app", f.app, signingTestTeam); err == nil {
				t.Fatalf("verification accepted an unstapled app: %s", output)
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
        app-sign-heavy:*/agent-heavy) printf 'error: The specified item could not be found in the keychain.\n' >&2; exit 41;;
        app-sign-remote:*/agent-remote) exit 42;;
        app-sign-app:*/Agent\ Queue.app) exit 43;;
      esac
    fi
    if [[ $1 == --verify && $failure == app-verify-heavy && $last == */agent-heavy ]]; then exit 44; fi
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
        case "$3" in */Agent-Queue-*-universal.zip) number=1; stage=app;; *.dmg) number=2; stage=dmg;; *) exit 92;; esac
        [[ $failure != "$stage-upload" ]] || exit 18
        if [[ $failure == "$stage-malformed-id" ]]; then printf '{}\n'; else printf '{"id":"00000000-0000-0000-0000-%012d"}\n' "$number"; fi
        ;;
      notarytool:wait)
        case "$3" in *001) stage=app;; *002) stage=dmg;; *) exit 92;; esac
        if [[ $failure == "$stage-malformed-status" ]]; then printf '{}\n'; exit 0; fi
        status=Accepted
        [[ $failure != "$stage-rejected" ]] || status=Invalid
        [[ $failure != "$stage-timeout" ]] || status='In Progress'
        printf '{"status":"%s"}\n' "$status"
        [[ $failure != "$stage-timeout" ]] || exit 75
        ;;
      notarytool:log)
        case "$3" in *001) stage=app;; *002) stage=dmg;; *) exit 92;; esac
        [[ $failure != "$stage-log" ]] || exit 19
        printf '{"issues":[]}\n' > "$last"
        ;;
      stapler:staple)
        stage=dmg
        [[ ! -d $last ]] || stage=app
        [[ $failure != "$stage-staple" ]] || exit 20
        if [[ -d $last ]]; then printf 'stapled' > "$last/.stub-staple"; else printf '\nstapled' >> "$last"; fi
        ;;
      stapler:validate)
        stage=dmg
        [[ ! -d $last ]] || stage=app
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
    ;;
  hdiutil)
    if [[ $1 == create ]]; then
      source_next=0
      for argument in "$@"; do
        if [[ $source_next == 1 ]]; then image_source=$argument; source_next=0; fi
        if [[ $argument == -srcfolder ]]; then source_next=1; fi
      done
      [[ -d "$image_source/Agent Queue.app" && -L "$image_source/Applications" &&
        $(readlink "$image_source/Applications") == /Applications ]] || exit 91
      [[ $(find "$image_source" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ') == 2 ]] || exit 91
      printf 'disk-image-layout|Agent Queue.app|Applications->/Applications\n' >> "$SIGNING_TEST_EVENTS"
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

func TestMacOSSigningRejectsAppLayoutTampering(t *testing.T) {
	for _, mutation := range []string{"embedded bin", "missing helper", "missing plist", "modified plist", "extra login plist", "false marker", "missing marker"} {
		t.Run(mutation, func(t *testing.T) {
			f := newSigningFixture(t)
			path := filepath.Join(f.app, "Contents/Library/LaunchAgents/io.went.agent-heavy.plist")
			info := filepath.Join(f.app, "Contents/Info.plist")
			switch mutation {
			case "embedded bin":
				verificationWrite(t, filepath.Join(f.app, "Contents/Resources/payload/source/bin/agent-heavy"), "duplicate", 0700)
			case "missing helper":
				if err := os.Remove(filepath.Join(f.app, "Contents/MacOS/agent-remote")); err != nil {
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
				verificationWrite(t, info, strings.Replace(signingTestInfoPlist, "<key>AgentQueueServiceManagement</key><true/>", "<key>AgentQueueServiceManagement</key><false/>", 1), 0600)
			case "missing marker":
				verificationWrite(t, info, strings.Replace(signingTestInfoPlist, "<key>AgentQueueServiceManagement</key><true/>", "", 1), 0600)
			}
			if output, err := f.run(t, signingProductionEnvironment(), "package", f.app, f.destination, "1.2.3"); err == nil {
				t.Fatalf("app tampering accepted: %s", output)
			}
			if events, _ := os.ReadFile(f.events); strings.Contains(string(events), "notarytool|submit") {
				t.Fatal("tampered app reached Apple submission")
			}
		})
	}
}
