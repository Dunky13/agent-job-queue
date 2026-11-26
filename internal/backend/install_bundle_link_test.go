package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sparkleBundle builds an app with the trimmed Sparkle.framework layout the release ships.
func sparkleBundle(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(base, "Agent Queue.app")
	framework := filepath.Join(app, SparkleFramework)
	for _, name := range []string{"Contents/MacOS/Agent Queue", SparkleFramework + "/Versions/B/Sparkle", SparkleFramework + "/Versions/B/Autoupdate", SparkleFramework + "/Versions/B/Resources/Info.plist", SparkleFramework + "/Versions/B/Updater.app/Contents/MacOS/Updater"} {
		filename := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range sparkleLinks {
		if err := os.Symlink(target, filepath.Join(framework, link)); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func TestSetupAcceptsOnlySparkleVersionLinks(t *testing.T) {
	app := sparkleBundle(t)
	if err := installValidatePayloadTree(app); err != nil {
		t.Fatalf("shipped Sparkle layout refused: %v", err)
	}
	first, err := managedBundleDigest(app)
	if err != nil {
		t.Fatal(err)
	}
	framework := filepath.Join(app, SparkleFramework)
	if err := os.Remove(filepath.Join(framework, "Versions/Current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../..", filepath.Join(framework, "Versions/Current")); err != nil {
		t.Fatal(err)
	}
	if err := installValidatePayloadTree(app); err == nil || !strings.Contains(err.Error(), "unexpected symlink") {
		t.Fatalf("retargeted Versions/Current accepted or refused for another reason: %v", err)
	}
	if _, err := managedBundleDigest(app); err == nil {
		t.Fatal("digest accepted a retargeted link")
	}
	if err := os.Remove(filepath.Join(framework, "Versions/Current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("B", filepath.Join(framework, "Versions/Current")); err != nil {
		t.Fatal(err)
	}
	second, err := managedBundleDigest(app)
	if err != nil || second != first {
		t.Fatalf("digest changed for the same bundle: %v", err)
	}
	// A regular file holding the link's target text is a different bundle.
	resources := filepath.Join(framework, "Resources")
	if err := os.Remove(resources); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resources, []byte(sparkleLinks["Resources"]), 0o644); err != nil {
		t.Fatal(err)
	}
	if replaced, err := managedBundleDigest(app); err != nil || replaced == first {
		t.Fatalf("digest did not distinguish a file from a link: %v", err)
	}
	if err := os.Remove(resources); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sparkleLinks["Resources"], resources); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"Contents/MacOS/helper":                   "Agent Queue",
		SparkleFramework + "/Headers":             "Versions/Current/Headers",
		SparkleFramework + "/Versions/B/Sparkle2": "Sparkle",
	} {
		t.Run(name, func(t *testing.T) {
			extra := filepath.Join(app, name)
			if err := os.Symlink(target, extra); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(extra)
			if err := installValidatePayloadTree(app); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("unexpected symlink accepted or refused for another reason: %v", err)
			}
			if _, err := managedBundleDigest(app); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("digest accepted an unexpected symlink or refused it for another reason: %v", err)
			}
		})
	}
}
