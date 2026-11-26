package tooling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyAppAcceptsOnlyTheSparkleLayout(t *testing.T) {
	f := newVerificationFixture(t)
	if err := VerifyApp(f.app); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(f.events)
	if err != nil {
		t.Fatal(err)
	}
	framework := filepath.Join(f.app, "Contents/Frameworks/Sparkle.framework")
	for _, path := range []string{filepath.Join(framework, "Versions/B/Autoupdate"), filepath.Join(framework, "Versions/B/Updater.app"), framework} {
		if !strings.Contains(string(events), "codesign|--verify --strict --deep "+path+"\n") {
			t.Fatalf("Sparkle code was not verified: %s", path)
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, verificationFixture, string)
	}{
		{"extra nested app", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(framework, "Versions/B/Other.app/Contents/Info.plist"), "x", 0600)
		}},
		{"nested app outside the framework", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(f.app, "Contents/Library/Other.app/Contents/Info.plist"), "x", 0600)
		}},
		{"XPC services directory", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(framework, "Versions/B/XPCServices/Installer.xpc/Contents/Info.plist"), "x", 0600)
		}},
		{"XPC service link", func(t *testing.T, f verificationFixture, framework string) {
			if err := os.Symlink("Versions/Current/XPCServices", filepath.Join(framework, "XPCServices")); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra framework", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(f.app, "Contents/Frameworks/Other.framework/Other"), "x", 0700)
		}},
		{"nested framework", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(framework, "Versions/B/Resources/Other.framework/Other"), "x", 0700)
		}},
		{"dylib in the app", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(f.app, "Contents/Resources/libhook.dylib"), "x", 0600)
		}},
		{"dylib in the framework", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(framework, "Versions/B/Resources/libhook.dylib"), "x", 0600)
		}},
		{"bundle directory", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(f.app, "Contents/Resources/Hook.bundle/Contents/Info.plist"), "x", 0600)
		}},
		{"plugin directory", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(f.app, "Contents/PlugIns/Hook.plugin/Contents/Info.plist"), "x", 0600)
		}},
		{"Mach-O file in the app", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(f.app, "Contents/Resources/tool"), "\xcf\xfa\xed\xfe rest", 0700)
		}},
		{"universal Mach-O file in the framework", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(framework, "Versions/B/Resources/helper"), "\xca\xfe\xba\xbe rest", 0700)
		}},
		{"Mach-O file in Updater.app", func(t *testing.T, f verificationFixture, framework string) {
			verificationWrite(t, filepath.Join(framework, "Versions/B/Updater.app/Contents/Resources/extra"), "\xce\xfa\xed\xfe rest", 0700)
		}},
		{"unexpected link", func(t *testing.T, f verificationFixture, framework string) {
			if err := os.Symlink("/usr/bin/true", filepath.Join(framework, "Versions/B/Resources/escape")); err != nil {
				t.Fatal(err)
			}
		}},
		{"retargeted link", func(t *testing.T, f verificationFixture, framework string) {
			link := filepath.Join(framework, "Autoupdate")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/usr/bin/true", link); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing updater", func(t *testing.T, f verificationFixture, framework string) {
			if err := os.RemoveAll(filepath.Join(framework, "Versions/B/Updater.app")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing framework", func(t *testing.T, f verificationFixture, framework string) {
			if err := os.RemoveAll(filepath.Join(f.app, "Contents/Frameworks")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing license", func(t *testing.T, f verificationFixture, framework string) {
			if err := os.Remove(filepath.Join(f.source, "licenses/Sparkle-LICENSE")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newVerificationFixture(t)
			test.mutate(t, f, filepath.Join(f.app, "Contents/Frameworks/Sparkle.framework"))
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("unexpected Sparkle layout accepted")
			}
		})
	}
}

func TestVerifyAppRequiresStrictUniversalSparkleCodeFromTheSameTeam(t *testing.T) {
	version := "Contents/Frameworks/Sparkle.framework/Versions/B/"
	for _, relative := range []string{version + "Autoupdate", version + "Updater.app", "Contents/Frameworks/Sparkle.framework"} {
		t.Run("signature "+filepath.Base(relative), func(t *testing.T) {
			f := newVerificationFixture(t)
			t.Setenv("VERIFY_TEST_BAD_SIGNATURE", filepath.Join(f.app, relative))
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("invalid Sparkle signature accepted")
			}
		})
		t.Run("team "+filepath.Base(relative), func(t *testing.T) {
			f := newVerificationFixture(t)
			t.Setenv("VERIFY_TEST_BAD_TEAM", filepath.Join(f.app, relative))
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("Sparkle code from another team accepted")
			}
		})
	}
	for _, relative := range []string{version + "Sparkle", version + "Autoupdate", version + "Updater.app/Contents/MacOS/Updater"} {
		t.Run("architectures "+filepath.Base(relative), func(t *testing.T) {
			f := newVerificationFixture(t)
			t.Setenv("VERIFY_TEST_BAD_ARCH", filepath.Join(f.app, relative))
			if err := VerifyApp(f.app); err == nil {
				t.Fatal("non-universal Sparkle code accepted")
			}
		})
	}
}

func TestVerifySparkleConfiguration(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{"SUFeedURL": sparkleFeedURL, "SURequireSignedFeed": true, "SUVerifyUpdateBeforeExtraction": true, "SUPublicEDKey": verificationSparkleKey}
	}
	for _, test := range []struct {
		name    string
		mutate  func(map[string]any)
		tracked bool
		ok      bool
	}{
		{"published build", func(map[string]any) {}, true, true},
		{"untracked build without a key", func(info map[string]any) { delete(info, "SUPublicEDKey") }, false, true},
		{"published build without a key", func(info map[string]any) { delete(info, "SUPublicEDKey") }, true, false},
		{"malformed key", func(info map[string]any) { info["SUPublicEDKey"] = "not-a-key" }, false, false},
		{"short key", func(info map[string]any) { info["SUPublicEDKey"] = "AAAA" }, true, false},
		{"other feed", func(info map[string]any) { info["SUFeedURL"] = "https://example.invalid/appcast.xml" }, true, false},
		{"unsigned feed", func(info map[string]any) { info["SURequireSignedFeed"] = false }, true, false},
		{"verification after extraction", func(info map[string]any) { delete(info, "SUVerifyUpdateBeforeExtraction") }, true, false},
		{"preset automatic checks", func(info map[string]any) { info["SUEnableAutomaticChecks"] = true }, true, false},
		{"downloads description", func(info map[string]any) { info["NSDownloadsFolderUsageDescription"] = "x" }, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := valid()
			test.mutate(info)
			if err := verifySparkleConfiguration(info, test.tracked); (err == nil) != test.ok {
				t.Fatalf("unexpected result for %s: %v", test.name, err)
			}
		})
	}
}
