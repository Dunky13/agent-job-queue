package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallerHomebrewMovedAppAvoidsSharedStagingParents(t *testing.T) {
	for _, prefix := range []string{"opt/homebrew", "usr/local", "custom prefix/Homebrew"} {
		t.Run(prefix, func(t *testing.T) {
			f := installTestFixture(t)
			install := f.installer
			originalPayload := install.payload
			f.appBundle(t)
			base := filepath.Dir(originalPayload)
			brewPrefix := filepath.Join(base, prefix)
			caskroom := filepath.Join(brewPrefix, "Caskroom")
			stagedPayload := filepath.Join(caskroom, "agent-job-queue/0.1.0,123/Agent Job Queue Installer.app/Contents/Resources/payload")
			if err := os.MkdirAll(filepath.Dir(stagedPayload), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(originalPayload, stagedPayload); err != nil {
				t.Fatal(err)
			}
			stagedApp := filepath.Join(stagedPayload, "Agent Queue.app")
			if err := os.RemoveAll(stagedApp); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(install.app, stagedApp); err != nil {
				t.Fatal(err)
			}
			for _, directory := range []string{brewPrefix, caskroom} {
				if err := os.Chmod(directory, 0775); err != nil {
					t.Fatal(err)
				}
			}
			// Keep the rejection that exposed the incident: shared Homebrew
			// ancestors must not become trusted private payload parents.
			_, err := installReadPayload(stagedPayload, filepath.Join(stagedPayload, "source/build-info.json"), 16384, false)
			if err == nil || !strings.Contains(err.Error(), "unsafe parent ownership or permissions") {
				t.Fatalf("shared staging read did not fail closed: %v", err)
			}
			// Homebrew moves its App artifact before native first launch runs setup
			// with the installed app's real path. A staging symlink is never
			// the --app-bundle argument.
			if err := os.Rename(stagedApp, install.app); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(install.app, stagedApp); err != nil {
				t.Fatal(err)
			}
			before := installBundleSnapshot(t, install.app)
			for attempt := range 2 {
				if err := f.run(t); err != nil {
					t.Fatalf("installed app setup attempt %d: %v", attempt+1, err)
				}
			}
			if after := installBundleSnapshot(t, install.app); !reflect.DeepEqual(before, after) {
				t.Fatal("app setup changed Homebrew's managed application")
			}
			assertInstallerNoAppCommands(t, f)
			for _, directory := range []string{brewPrefix, caskroom} {
				info, err := os.Stat(directory)
				if err != nil || info.Mode().Perm() != 0775 {
					t.Fatalf("Homebrew ancestor permissions changed: %s: %v %v", directory, info, err)
				}
			}
			for _, directory := range []string{install.root, install.state, install.config} {
				info, err := os.Stat(directory)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("installed state is not private: %s: %v %v", directory, info, err)
				}
			}
		})
	}
}
