package tooling

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyNoLooseCodeNamesTheOffender(t *testing.T) {
	f := newVerificationFixture(t)
	if err := verifyNoLooseCode(f.app); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"Contents/Resources/libhook.dylib": "x", "Contents/Resources/tool": "\xcf\xfa\xed\xfe rest", "Contents/PlugIns/Hook.plugin/Info.plist": "x"} {
		verificationWrite(t, filepath.Join(f.app, path), content, 0700)
		if err := verifyNoLooseCode(f.app); err == nil || !strings.Contains(err.Error(), filepath.Base(strings.SplitN(path, "/Info", 2)[0])) {
			t.Fatalf("%s: %v", path, err)
		}
		if err := os.RemoveAll(filepath.Join(f.app, strings.SplitN(path, "/Info", 2)[0])); err != nil {
			t.Fatal(err)
		}
	}
}

// scripts/build-app.sh sources this function, so this is its key check.
func TestBuildAppPublicKeyCheck(t *testing.T) {
	script, err := filepath.Abs("../../packaging/sparkle/public-key.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	if err := os.WriteFile(good, []byte(verificationSparkleKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	entry := "<key>SUPublicEDKey</key><string>" + verificationSparkleKey + "</string>"
	for _, test := range []struct {
		name, file, build string
		ok                bool
		want              string
	}{
		{"published build without a key", filepath.Join(dir, "missing.txt"), "42", false, ""},
		{"untracked build without a key", filepath.Join(dir, "missing.txt"), "", true, ""},
		{"published build with a key", good, "42", true, entry},
		{"untracked build with a key", good, "", true, entry},
		{"malformed key", bad, "", false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("/bin/bash", "-c", `source "$1" && sparkle_public_key_plist "$2"`, "bash", script, test.file)
			cmd.Env = append(os.Environ(), "BUILD_NUMBER="+test.build)
			output, err := cmd.Output()
			if (err == nil) != test.ok || string(output) != test.want {
				t.Fatalf("ok=%v output=%q err=%v", test.ok, output, err)
			}
		})
	}
}
