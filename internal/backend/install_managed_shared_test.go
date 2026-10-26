package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// This low-level FD copier does not confer publication or execution custody.
// The production wrapper below must refuse a foreign-writable parent even when
// this primitive can copy bytes without following a substituted symlink.
func TestManagedSharedCopyPreservesPrivateBoundary(t *testing.T) {
	f := installTestFixture(t)
	shared, parent := managedSharedFixture(t)
	source := filepath.Join(f.installer.payload, "Agent Queue.app")
	if err := installCopyTree(source, filepath.Join(shared, "legacy.app")); err == nil || !strings.Contains(err.Error(), "group/world-writable parent") {
		t.Fatalf("legacy copier did not reproduce private-parent refusal: %v", err)
	}
	if err := managedCopyTreeAt(context.Background(), source, parent, "candidate.app"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(shared, "candidate.app/Contents/MacOS/AgentQueue"))
	if err != nil || string(raw) != "prebuilt app" {
		t.Fatalf("copied bytes: %q %v", raw, err)
	}
	info, err := os.Stat(filepath.Join(shared, "candidate.app/Contents/MacOS/AgentQueue"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("executable mode: %v %v", info, err)
	}
	if info, err := parent.Stat(); err != nil || info.Mode().Perm() != 0775 {
		t.Fatalf("shared parent permissions changed: %v %v", info, err)
	}
	if private, err := integrationParent(filepath.Join(shared, "private-state/lock"), true); err == nil {
		private.Close()
		t.Fatal("private state checker accepted shared ancestor")
	}
	if _, err := os.Stat(filepath.Join(shared, "private-state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private state was created: %v", err)
	}
}

func TestManagedSharedCopyRefusesUnsafeEntries(t *testing.T) {
	for _, kind := range []string{"source-link", "child-link", "hardlink", "world-writable", "setuid", "fifo", "destination-link", "destination-existing", "escape", "oversize", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := installTestFixture(t)
			shared, parent := managedSharedFixture(t)
			source := filepath.Join(f.installer.payload, "Agent Queue.app")
			file := filepath.Join(source, "Contents/MacOS/AgentQueue")
			outside := filepath.Join(f.installer.home, "outside")
			integrationTestWrite(t, outside, []byte("preserved"))
			name := "candidate.app"
			ctx := context.Background()
			var err error
			switch kind {
			case "source-link":
				link := filepath.Join(f.installer.payload, "source-link.app")
				err = os.Symlink(source, link)
				source = link
			case "child-link":
				err = os.Symlink(outside, filepath.Join(source, "link"))
			case "hardlink":
				err = os.Link(file, filepath.Join(source, "hardlink"))
			case "world-writable":
				err = os.Chmod(file, 0722)
			case "setuid":
				err = os.Chmod(file, 0700|os.ModeSetuid)
			case "fifo":
				err = unix.Mkfifo(filepath.Join(source, "fifo"), 0600)
			case "destination-link":
				err = os.Symlink(outside, filepath.Join(shared, name))
			case "destination-existing":
				err = os.Mkdir(filepath.Join(shared, name), 0700)
			case "escape":
				name = "../escape.app"
			case "oversize":
				err = os.Truncate(file, updateMaxArchive+1)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := managedCopyTreeAt(ctx, source, parent, name); err == nil {
				t.Fatal("unsafe copy succeeded")
			}
			if slices.Contains([]string{"source-link", "child-link", "hardlink", "world-writable", "setuid", "fifo", "oversize"}, kind) {
				if err := managedCopySourceAccess(source); err == nil {
					t.Fatal("writer refusal was not detected by read-only preflight")
				}
			}
			if raw, err := os.ReadFile(outside); err != nil || string(raw) != "preserved" {
				t.Fatalf("outside destination changed: %q %v", raw, err)
			}
		})
	}
}

func TestManagedSharedCopyPinsParentDescriptor(t *testing.T) {
	f := installTestFixture(t)
	shared, parent := managedSharedFixture(t)
	retained := shared + "-retained"
	if err := os.Rename(shared, retained); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.installer.home, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, shared); err != nil {
		t.Fatal(err)
	}
	if err := managedCopyTreeAt(context.Background(), filepath.Join(f.installer.payload, "Agent Queue.app"), parent, "candidate.app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(retained, "candidate.app/Contents/MacOS/AgentQueue")); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("copy escaped held directory: %v %v", entries, err)
	}
}

func TestManagedSharedCopyBoundsAndOwnership(t *testing.T) {
	for _, name := range []string{"", ".", "..", "/", "/absolute.app", "../escape.app", "nested/child.app"} {
		if managedCopyName(name) {
			t.Fatalf("non-child staging name accepted: %q", name)
		}
	}
	for _, kind := range []string{"foreign", "unsafe-directory", "hardlink", "special", "total-bytes", "entries", "depth"} {
		t.Run(kind, func(t *testing.T) {
			info := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uint32(os.Getuid()), Nlink: 1}
			directory := false
			switch kind {
			case "foreign":
				info.Uid = uint32(os.Getuid()) + 1
			case "unsafe-directory":
				info.Mode = unix.S_IFDIR | 0775
				directory = true
			case "hardlink":
				info.Nlink = 2
			case "special":
				info.Mode = unix.S_IFSOCK | 0600
			default:
				f := installTestFixture(t)
				_, parent := managedSharedFixture(t)
				input, err := os.Open(filepath.Join(f.installer.payload, "Agent Queue.app"))
				if err != nil {
					t.Fatal(err)
				}
				defer input.Close()
				budget := &managedCopyBudget{}
				depth := 0
				switch kind {
				case "total-bytes":
					budget.bytes = updateMaxExpanded
				case "entries":
					budget.entries = 10000
				case "depth":
					depth = 129
				}
				if err := budget.directory(context.Background(), input, parent, depth); err == nil {
					t.Fatal("copy limit was ignored")
				}
				return
			}
			if err := managedCopyInfo(&info, directory); err == nil {
				t.Fatal("unsafe ownership/type/permission accepted")
			}
		})
	}
}

func TestManagedSharedAppStagesThenMovesThroughFD(t *testing.T) {
	f := managedTestFixture(t)
	shared, parent := managedSharedFixture(t)
	if err := f.installer.managedCopyAppAt(context.Background(), filepath.Join(f.installer.payload, "Agent Queue.app"), parent, "candidate.app"); err == nil {
		t.Fatal("group-writable publication parent accepted")
	}
	if err := os.Chmod(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.installer.managedCopyAppAt(context.Background(), filepath.Join(f.installer.payload, "Agent Queue.app"), parent, "candidate.app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, "candidate.app/Contents/MacOS/AgentQueue")); err != nil {
		t.Fatal(err)
	}
	workspaces, err := filepath.Glob(filepath.Join(f.installer.home, ".AgentQueue-staging-*"))
	if err != nil || len(workspaces) != 0 {
		t.Fatalf("private staging leaked: %v %v", workspaces, err)
	}
	if info, err := parent.Stat(); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("shared parent permissions changed: %v %v", info, err)
	}
}

func TestManagedSharedCopyDetectsReboundChild(t *testing.T) {
	f := installTestFixture(t)
	shared, parent := managedSharedFixture(t)
	if err := os.Mkdir(filepath.Join(shared, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	child, err := os.Open(filepath.Join(shared, "child"))
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if err := os.Rename(filepath.Join(shared, "child"), filepath.Join(shared, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.installer.payload, filepath.Join(shared, "child")); err != nil {
		t.Fatal(err)
	}
	if err := managedCopyNamed(parent, "child", child); err == nil {
		t.Fatal("rebound directory was accepted")
	}
	input, err := os.Open(filepath.Join(f.installer.payload, "Agent Queue.app"))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := (&managedCopyBudget{}).entry(context.Background(), input, parent, "child", 0); err == nil {
		t.Fatal("destination child symlink was followed")
	}
}

func TestManagedPublicationRefusesReadonlyCodeBeforeStopping(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses owner write permissions")
	}
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(m.installer.app, "Contents/MacOS/agent-heavy")
	if err := os.Chmod(file, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0700) })
	m.calls = nil
	if err := m.run(t); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("read-only code was accepted: %v", err)
	}
	managedRequireNoServiceMutation(t, m.calls)
}

func TestManagedPublicationRechecksAccessAfterStaging(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses owner write permissions")
	}
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	before, err := managedBundleDigest(i.app)
	if err != nil {
		t.Fatal(err)
	}
	managedCandidate(t, m, "124")
	file := filepath.Join(i.app, "Contents/MacOS/agent-heavy")
	t.Cleanup(func() { _ = os.Chmod(file, 0700) })
	original := i.command
	i.command = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "/usr/bin/codesign" && strings.Contains(args[len(args)-1], ".Agent Queue.installing-") {
			if err := os.Chmod(file, 0500); err != nil {
				t.Fatal(err)
			}
		}
		return original(ctx, args)
	}
	if err := i.publishManaged(context.Background()); err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("publication did not recheck access: %v", err)
	}
	if after, err := managedBundleDigest(i.app); err != nil || before != after {
		t.Fatalf("old app changed after publication refusal: %v", err)
	}
}

func TestManagedPublicationKeepsExplicitImmutableSetup(t *testing.T) {
	m := managedTestFixture(t)
	i := m.installer
	if err := os.MkdirAll(filepath.Dir(i.app), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(i.app), 0700) })
	i.appBundle = true
	if err := i.managedPublicationAccess(); err != nil {
		t.Fatalf("explicit immutable setup required publication permission: %v", err)
	}
	if info, err := os.Stat(filepath.Dir(i.app)); err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("in-place setup changed publication permissions: %v %v", info, err)
	}
}

func TestManagedSharedNativeCopyRetainsMetadataAndVerifiesBeforeMove(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native ditto metadata requires macOS")
	}
	for _, kind := range []string{"metadata", "signature-refusal", "unsafe-parent-at-publication"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			i := m.installer
			i.testing = false
			shared, parent := managedSharedFixture(t)
			if err := os.Chmod(shared, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(i.payload, "Agent Queue.app")
			if err := unix.Setxattr(source, "com.apple.quarantine", []byte("0081;6f35b100;AgentQueueInstaller;"), 0); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(source, "io.went.agent-queue.fixture", []byte("preserved"), 0); err != nil {
				t.Fatal(err)
			}
			original := i.command
			native, verified := false, false
			i.command = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "/usr/bin/ditto" {
					native = true
					if len(args) != 7 || !slices.Equal(args[1:5], []string{"--rsrc", "--extattr", "--qtn", "--acl"}) || args[5] != source || !strings.HasPrefix(args[6], i.home+"/.AgentQueue-staging-") {
						t.Fatalf("unexpected native copy contract: %v", args)
					}
					return exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
				}
				if args[0] == "/usr/bin/codesign" {
					verified = true
					if _, err := os.Lstat(filepath.Join(shared, "candidate.app")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("published before signature verification: %v", err)
					}
					if kind == "signature-refusal" {
						return nil, errors.New("simulated signature failure")
					}
					if kind == "unsafe-parent-at-publication" {
						if err := os.Chmod(shared, 0777); err != nil {
							t.Fatal(err)
						}
					}
				}
				return original(ctx, args)
			}
			err := i.managedCopyAppAt(context.Background(), source, parent, "candidate.app")
			if !native || !verified {
				t.Fatalf("native/signature gates not reached: %v %v: %v", native, verified, err)
			}
			if kind != "metadata" {
				want := "signature"
				if kind == "unsafe-parent-at-publication" {
					want = "unsafe parent"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("publication failure ignored: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(shared, "candidate.app")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe candidate was published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for attribute, prefix := range map[string]string{"com.apple.quarantine": "0081;", "io.went.agent-queue.fixture": "preserved"} {
				data := make([]byte, 1024)
				n, err := unix.Getxattr(filepath.Join(shared, "candidate.app"), attribute, data)
				if err != nil || !strings.HasPrefix(string(data[:n]), prefix) {
					t.Fatalf("lost %s: %q %v", attribute, data[:n], err)
				}
			}
		})
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
