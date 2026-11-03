package backend

import (
	"errors"
	"fmt"
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
		if err := managedSystemPublicationParentInfo(&rootInfo, false); err != nil {
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
		if err := managedSystemPublicationParentInfo(&info, true); err != nil {
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
				return nil, fmt.Errorf("Managed application publication requires a protected parent; use per-user Applications, or an approved protected location for shared deployments: %w", err)
			}
			return parent, nil
		}
		// A first install may not yet have its private Applications directory.
		// Validate the nearest existing ancestor without creating or chmodding it.
		path = filepath.Dir(path)
	}
}

func managedSystemPublicationParentInfo(info *unix.Stat_t, applications bool) error {
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFDIR || info.Uid != 0 || uint32(info.Mode)&0022 != 0 && !(applications && appservices.StockApplications(info)) {
		return errors.New("Managed application publication requires a protected root-owned Applications directory without group/world write permission; use your per-user Applications directory, or an approved protected location for shared deployments")
	}
	return nil
}
