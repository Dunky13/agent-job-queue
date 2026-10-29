package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"golang.org/x/sys/unix"
)

// Publication names require a protected parent. Holding a parent descriptor
// does not prevent other writers from rebinding its children. Private state
// continues to use integrationParent with its existing stricter rules.
func (install *installer) managedPublicationParent(require bool) (*os.File, error) {
	if !integrationValidPath(install.app) || filepath.Clean(install.app) != install.app {
		return nil, errors.New("Managed app publication requires a canonical absolute path")
	}
	if filepath.Dir(install.app) == "/Applications" {
		root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(root)
		var rootInfo unix.Stat_t
		if err := unix.Fstat(root, &rootInfo); err != nil {
			return nil, err
		}
		if err := managedSystemPublicationParentInfo(&rootInfo); err != nil {
			return nil, err
		}
		fd, err := unix.Openat(root, "Applications", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		parent := os.NewFile(uintptr(fd), "/Applications")
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			parent.Close()
			return nil, err
		}
		if err := managedSystemPublicationParentInfo(&info); err != nil {
			parent.Close()
			return nil, err
		}
		return parent, nil
	}
	path := install.app
	for {
		parent, err := integrationParent(path, false)
		if !errors.Is(err, os.ErrNotExist) || require {
			if err != nil {
				return nil, fmt.Errorf("Managed application publication requires a protected parent; use the explicit installer with per-user Applications, or an approved protected location for shared deployments: %w", err)
			}
			return parent, nil
		}
		// A first install may not yet have its private Applications directory.
		// Validate the nearest existing ancestor without creating or chmodding it.
		path = filepath.Dir(path)
	}
}

func managedSystemPublicationParentInfo(info *unix.Stat_t) error {
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFDIR || info.Uid != 0 || uint32(info.Mode)&0022 != 0 && !appservices.StockApplications(info) {
		return errors.New("Managed application publication requires a protected root-owned Applications directory without group/world write permission; use your per-user Applications directory with the explicit installer, or an approved protected location for shared deployments")
	}
	return nil
}

func (install *installer) managedStagingAccess(publication *os.File) error {
	private, err := integrationParent(filepath.Join(install.home, ".managed-app-probe"), false)
	if err != nil {
		return err
	}
	defer private.Close()
	var left, right unix.Stat_t
	if err := unix.Fstat(int(private.Fd()), &left); err != nil {
		return err
	}
	if err := unix.Fstat(int(publication.Fd()), &right); err != nil {
		return err
	}
	if uint32(right.Mode)&unix.S_IFMT != unix.S_IFDIR || (right.Uid != 0 && right.Uid != uint32(os.Getuid())) || uint32(right.Mode)&0022 != 0 {
		return errors.New("Managed app publication has an unsafe parent")
	}
	if err := unix.Faccessat(int(publication.Fd()), ".", unix.W_OK|unix.X_OK, 0); err != nil {
		return errors.New("Application directory is not writable by this account")
	}
	if left.Dev != right.Dev {
		return errors.New("Private app staging and Applications must share a filesystem for atomic publication; no services were changed")
	}
	if err := unix.Access(install.home, unix.W_OK|unix.X_OK); err != nil {
		return errors.New("Private app staging is not writable by this account; no services were changed")
	}
	return nil
}

func managedCopyName(name string) bool {
	return name != "" && name != "." && name != ".." && !filepath.IsAbs(name) && filepath.Base(name) == name
}

func managedCopyInfo(info *unix.Stat_t, directory bool) error {
	want := uint32(unix.S_IFREG)
	if directory {
		want = unix.S_IFDIR
	}
	if uint32(info.Mode)&unix.S_IFMT != want || (info.Uid != uint32(os.Getuid()) && info.Uid != 0) || uint32(info.Mode)&007022 != 0 || (!directory && info.Nlink != 1) {
		return errors.New("Managed app has unsafe ownership, permissions, links, or file type")
	}
	return nil
}

// Check the name as well as the held descriptor. This detects removal/rebinding
// of an entry in a shared parent without following the replacement.
func managedCopyNamed(parent *os.File, name string, file *os.File) error {
	var named, held unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstat(int(file.Fd()), &held); err != nil {
		return err
	}
	if named.Dev != held.Dev || named.Ino != held.Ino || named.Uid != uint32(os.Getuid()) || uint32(named.Mode)&unix.S_IFMT != unix.S_IFDIR || uint32(named.Mode)&0022 != 0 {
		return errors.New("Managed app staging directory changed during publication")
	}
	return nil
}

type managedCopyBudget struct {
	entries int
	bytes   int64
}

// Copy only through already-open directory descriptors. Every destination entry
// is new; no source or destination symlink is followed, even after a rename of
// the containing path. The caller decides which parent is an approved boundary.
func managedCopyTreeAt(ctx context.Context, source string, parent *os.File, name string) error {
	if !managedCopyName(name) {
		return errors.New("Managed app staging must be a direct child of its checked parent")
	}
	input, err := installPayloadParent(source, filepath.Join(source, ".managed-copy-probe"), true)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	output := os.NewFile(uintptr(fd), name)
	defer output.Close()
	if err := managedCopyNamed(parent, name, output); err != nil {
		return err
	}
	budget := &managedCopyBudget{}
	if err := budget.directory(ctx, input, output, 0); err != nil {
		return err
	}
	return managedCopyNamed(parent, name, output)
}

// Exercise the writer's ownership, type and size policy before quiescence,
// without creating a directory or reading file contents.
func managedCopySourceAccess(source string) error {
	input, err := installPayloadParent(source, filepath.Join(source, ".managed-copy-probe"), true)
	if err != nil {
		return err
	}
	defer input.Close()
	return (&managedCopyBudget{}).directory(context.Background(), input, nil, 0)
}

func (budget *managedCopyBudget) directory(ctx context.Context, input, output *os.File, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	budget.entries++
	if budget.entries > 10000 || depth > 128 {
		return errors.New("Managed app exceeds its entry or nesting limit")
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(input.Fd()), &before); err != nil {
		return err
	}
	if err := managedCopyInfo(&before, true); err != nil {
		return err
	}
	for {
		entries, readErr := input.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			if !managedCopyName(name) {
				return errors.New("Invalid managed app entry name")
			}
			fd, err := unix.Openat(int(input.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			child := os.NewFile(uintptr(fd), name)
			err = budget.entry(ctx, child, output, name, depth+1)
			child.Close()
			if err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(input.Fd()), &after); err != nil {
		return err
	}
	if integrationStatIdentity(&before) != integrationStatIdentity(&after) {
		return errors.New("Managed app source directory changed during staging")
	}
	if output != nil {
		return output.Sync()
	}
	return nil
}

func (budget *managedCopyBudget) entry(ctx context.Context, input, parent *os.File, name string, depth int) error {
	var before unix.Stat_t
	if err := unix.Fstat(int(input.Fd()), &before); err != nil {
		return err
	}
	directory := uint32(before.Mode)&unix.S_IFMT == unix.S_IFDIR
	if err := managedCopyInfo(&before, directory); err != nil {
		return err
	}
	if directory {
		if parent == nil {
			return budget.directory(ctx, input, nil, depth)
		}
		if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
			return err
		}
		fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		output := os.NewFile(uintptr(fd), name)
		defer output.Close()
		if err := managedCopyNamed(parent, name, output); err != nil {
			return err
		}
		return budget.directory(ctx, input, output, depth)
	}
	budget.entries++
	if budget.entries > 10000 || before.Size < 0 || before.Size > updateMaxArchive || budget.bytes > updateMaxExpanded-before.Size {
		return errors.New("Managed app exceeds its entry or content size limit")
	}
	budget.bytes += before.Size
	if parent == nil {
		return nil
	}
	mode := uint32(0600)
	if uint32(before.Mode)&0111 != 0 {
		mode = 0700
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	output := os.NewFile(uintptr(fd), name)
	defer output.Close()
	n, err := io.Copy(output, io.LimitReader(input, before.Size+1))
	if err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(input.Fd()), &after); err != nil {
		return err
	}
	if n != before.Size || integrationStatIdentity(&before) != integrationStatIdentity(&after) {
		return errors.New("Managed app source file changed during staging")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return output.Sync()
}

func (install *installer) managedCopyApp(ctx context.Context, source, destination string) error {
	if filepath.Dir(install.app) != filepath.Dir(destination) {
		return errors.New("Managed app staging escapes its publication parent")
	}
	parent, err := install.managedPublicationParent(true)
	if err != nil {
		return err
	}
	defer parent.Close()
	return install.managedCopyAppAt(ctx, source, parent, filepath.Base(destination))
}

// ditto needs pathname arguments on macOS. Run it only inside a checked private
// home workspace, never against a pathname below the group-writable parent.
// After native metadata and signature verification, renameat publishes the
// complete bundle into a protected parent on the same filesystem.
func (install *installer) managedCopyAppAt(ctx context.Context, source string, publication *os.File, name string) (result error) {
	if !managedCopyName(name) {
		return errors.New("Invalid managed app staging name")
	}
	if err := install.managedStagingAccess(publication); err != nil {
		return err
	}
	expected, err := managedCaptureBundle(source)
	if err != nil {
		return err
	}
	private, err := integrationParent(filepath.Join(install.home, ".managed-app-probe"), false)
	if err != nil {
		return err
	}
	defer private.Close()
	workspaceName := ".AgentQueue-staging-" + token()
	if err := unix.Mkdirat(int(private.Fd()), workspaceName, 0700); err != nil {
		return err
	}
	fd, err := unix.Openat(int(private.Fd()), workspaceName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	workspace := os.NewFile(uintptr(fd), workspaceName)
	defer workspace.Close()
	path := filepath.Join(install.home, workspaceName)
	var candidateRoot *os.File
	var ownedCandidate string
	exported := false
	defer func() {
		if candidateRoot != nil {
			defer candidateRoot.Close()
		}

		if err := managedCopyNamed(private, workspaceName, workspace); err != nil {
			result = errors.Join(result, fmt.Errorf("Private staging custody changed; evidence preserved: %w", err))
			return
		}
		if candidateRoot != nil && !exported {
			if err := managedCopyNamed(workspace, ownedCandidate, candidateRoot); err != nil {
				result = errors.Join(result, fmt.Errorf("Private candidate custody changed; evidence preserved: %w", err))
				return
			}
		}
		result = errors.Join(result, os.RemoveAll(path))
	}()
	if err := managedCopyNamed(private, workspaceName, workspace); err != nil {
		return err
	}
	if err := managedCopyTreeAt(ctx, source, workspace, "validated.app"); err != nil {
		return err
	}
	candidate := "validated.app"
	if !install.testing {
		if err := os.RemoveAll(filepath.Join(path, candidate)); err != nil {
			return err
		}
		candidate = ".Agent Queue.installing-" + token() + ".app"
		if _, err := install.run(ctx, "/usr/bin/ditto", "--rsrc", "--extattr", "--qtn", "--acl", source, filepath.Join(path, candidate)); err != nil {
			return err
		}
	}
	// The test copier follows the same verification sequence as native ditto.
	// Keep the diagnostic candidate basename in the private workspace as well.
	if install.testing {
		verifiedName := ".Agent Queue.installing-" + token() + ".app"
		if err := unix.Renameat(int(workspace.Fd()), candidate, int(workspace.Fd()), verifiedName); err != nil {
			return err
		}
		candidate = verifiedName
	}
	candidatePath := filepath.Join(path, candidate)
	candidateRoot, err = installPayloadParent(candidatePath, filepath.Join(candidatePath, ".custody-probe"), true)
	if err != nil {
		return err
	}
	ownedCandidate = candidate
	before, err := managedCaptureBundle(candidatePath)
	if err != nil {
		return err
	}
	if before.Digest != expected.Digest || !before.Build.Equal(expected.Build) {
		return errors.New("Managed staged candidate differs from the expected source identity")
	}
	if err := install.managedVerify(ctx, candidatePath); err != nil {
		return err
	}
	if err := managedRequireBundle(candidatePath, before); err != nil {
		return err
	}
	if err := managedRequireBundle(source, expected); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := managedCopyNamed(private, workspaceName, workspace); err != nil {
		return err
	}
	if err := install.managedStagingAccess(publication); err != nil {
		return err
	}
	var info unix.Stat_t
	if err := unix.Fstatat(int(publication.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		if err != nil {
			return err
		}
		return errors.New("Managed app staging destination already exists")
	}
	if err := unix.Renameat(int(workspace.Fd()), candidate, int(publication.Fd()), name); err != nil {
		return fmt.Errorf("Publish managed app staging: %w", err)
	}
	exported = true
	if err := publication.Sync(); err != nil {
		// The caller has not received the stage yet. Retire only this exact
		// verified root if a durability failure prevents that handoff.
		if custodyErr := managedCopyNamed(publication, name, candidateRoot); custodyErr != nil {
			return errors.Join(err, custodyErr)
		}
		root, openErr := os.OpenRoot(publication.Name())
		if openErr != nil {
			return errors.Join(err, openErr)
		}
		cleanupErr := root.RemoveAll(name)
		root.Close()
		return errors.Join(err, cleanupErr)
	}
	return nil
}
