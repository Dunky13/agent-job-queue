package backend

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func managedSharedFixture(t *testing.T) (string, *os.File) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "shared")
	if err := os.Mkdir(path, 0775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0775); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(fd), path)
	t.Cleanup(func() { parent.Close() })
	return path, parent
}

func TestManagedPublicationKeepsExplicitImmutableSetup(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := os.Chmod(filepath.Dir(i.app), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(i.app), 0700) })
	if err := i.managedPublicationAccess(); err != nil {
		t.Fatalf("explicit immutable setup required publication permission: %v", err)
	}
	if info, err := os.Stat(filepath.Dir(i.app)); err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("in-place setup changed publication permissions: %v %v", info, err)
	}
}

func TestManagedPublicationRejectsUnsafeParentBeforeServiceChanges(t *testing.T) {
	m := managedTestFixture(t)
	shared, _ := managedSharedFixture(t)
	m.installer.app = filepath.Join(shared, "Agent Queue.app")
	if _, err := m.installer.planManaged(context.Background()); err == nil {
		t.Fatal("arbitrary shared ancestor accepted")
	}
	for _, args := range m.calls {
		if slices.Contains(args, "unregister") || slices.Contains(args, "register") || slices.Contains(args, "--prepare-app-replacement") {
			t.Fatalf("preflight changed a service: %v", args)
		}
	}
}
