package tooling

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This is a native lookup probe, not a Developer ID or notarization test. A new
// synthetic identity prevents an operator's existing certificate from satisfying
// the lookup, and requires neither production credentials nor trust overrides.
func TestMacOSCodesignUsesUniquePrivateKeychainIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires native macOS Keychain and codesign")
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run := func(label, command string, arguments ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, command, arguments...)
		cmd.Env = signingTestEnvironment("PATH=/usr/bin:/bin:/usr/sbin:/sbin")
		output, err := cmd.CombinedOutput()
		if err != nil {
			// Never include argv: import and Keychain setup carry an ephemeral
			// password. Tool status and this fixed label locate the failed step.
			t.Fatalf("native private-Keychain probe: %s failed: %v", label, err)
		}
		return output
	}
	beforeList := string(run("read search list", "/usr/bin/security", "list-keychains", "-d", "user"))
	beforeDefault := string(run("read default Keychain", "/usr/bin/security", "default-keychain", "-d", "user"))
	keychain := filepath.Join(root, "synthetic.keychain-db")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		check := func(label string, arguments ...string) []byte {
			t.Helper()
			cmd := exec.CommandContext(cleanupCtx, "/usr/bin/security", arguments...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("native private-Keychain probe cleanup: %s failed: %v", label, err)
			}
			return output
		}
		if _, err := os.Lstat(keychain); err == nil {
			check("delete owned Keychain", "delete-keychain", keychain)
		} else if !os.IsNotExist(err) {
			t.Errorf("check owned Keychain: %v", err)
		}
		if after := string(check("read search list", "list-keychains", "-d", "user")); after != beforeList {
			t.Error("native probe changed the operator's Keychain search list")
		}
		if after := string(check("read default Keychain", "default-keychain", "-d", "user")); after != beforeDefault {
			t.Error("native probe changed the operator's default Keychain")
		}
	})
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(random[:])
	name := "Agent Queue Synthetic Signing " + hex.EncodeToString(random[:8])
	configuration := "[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=" + name + "\nOU=TESTTEAM01\n[ext]\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\n"
	config := filepath.Join(root, "certificate.cnf")
	if err := os.WriteFile(config, []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	key, certificate, bundle := filepath.Join(root, "key.pem"), filepath.Join(root, "certificate.pem"), filepath.Join(root, "identity.p12")
	run("generate synthetic identity", "/usr/bin/openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-config", config, "-keyout", key, "-out", certificate)
	run("package synthetic identity", "/usr/bin/openssl", "pkcs12", "-export", "-inkey", key, "-in", certificate, "-out", bundle, "-passout", "pass:"+password)
	run("create owned Keychain", "/usr/bin/security", "create-keychain", "-p", password, keychain)
	if current := string(run("check search list", "/usr/bin/security", "list-keychains", "-d", "user")); current != beforeList {
		t.Fatal("creating an owned Keychain changed the operator search list")
	}
	run("unlock owned Keychain", "/usr/bin/security", "unlock-keychain", "-p", password, keychain)
	run("configure owned Keychain", "/usr/bin/security", "set-keychain-settings", "-lut", "21600", keychain)
	run("import synthetic identity", "/usr/bin/security", "import", bundle, "-k", keychain, "-P", password, "-T", "/usr/bin/codesign")
	run("authorize codesign", "/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:,codesign:", "-s", "-k", password, keychain)
	inventory := run("read explicit identity inventory", "/usr/bin/security", "find-identity", "-p", "codesigning", keychain)
	// The unrestricted command can repeat the same identity in its valid-only
	// section. Count only the full matching section, as the real wrapper does.
	matching, _, _ := strings.Cut(string(inventory), "\n  Valid identities only")
	pattern := regexp.MustCompile(`(?m)^\s*\d+\)\s+([A-Fa-f0-9]{40})\s+"` + regexp.QuoteMeta(name) + `"`)
	rows := pattern.FindAllStringSubmatch(matching, -1)
	if len(rows) != 1 {
		t.Fatal("synthetic identity is absent or ambiguous in the explicit Keychain")
	}
	fingerprint := rows[0][1]
	global := run("read default identity inventory", "/usr/bin/security", "find-identity", "-p", "codesigning")
	if strings.Contains(strings.ToUpper(string(global)), strings.ToUpper(fingerprint)) {
		t.Fatal("synthetic identity is already visible through the default search list")
	}
	input, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "probe")
	if err := os.WriteFile(target, input, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/codesign", "--force", "--sign", fingerprint, "--keychain", keychain, "--timestamp=none", "--options", "runtime", "--identifier", "io.went.agent-queue.synthetic-probe", target)
	cmd.Env = signingTestEnvironment("PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unique explicit-Keychain codesign failed: %v; diagnostic: %s", err, output)
	}
	run("verify synthetic signature", "/usr/bin/codesign", "--verify", "--strict", "--all-architectures", target)
	prefix := filepath.Join(root, "signed-certificate-")
	run("extract signed certificate", "/usr/bin/codesign", "--display", "--extract-certificates="+prefix, target)
	signed := run("check signed identity fingerprint", "/usr/bin/openssl", "x509", "-inform", "der", "-in", prefix+"0", "-noout", "-fingerprint", "-sha1")
	if !strings.Contains(strings.ReplaceAll(strings.ToUpper(string(signed)), ":", ""), strings.ToUpper(fingerprint)) {
		t.Fatal("codesign did not use the unique synthetic identity")
	}
	// The probe signs but never executes its copied target or submits to Apple.
}
