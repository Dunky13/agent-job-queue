package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// installServices is the installer's port to the services registered with the
// user's login session. Print returns errInstallServiceNotLoaded for an absent
// service.
type installServices interface {
	Print(ctx context.Context, label string) ([]byte, error)
	Bootout(ctx context.Context, label string) error
	Bootstrap(ctx context.Context, definition string) error
}

func (install *installer) loginServices() installServices {
	if install.services == nil {
		return installLaunchctl{install}
	}
	return install.services
}

type installLaunchctl struct{ install *installer }

// domain also heads the print output that service definition recovery parses.
func (install *installer) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (adapter installLaunchctl) Print(ctx context.Context, label string) ([]byte, error) {
	output, err := adapter.install.run(ctx, "launchctl", "print", adapter.install.domain()+"/"+label)
	var exited *exec.ExitError
	if errors.As(err, &exited) && installServiceAbsent(output, exited.ExitCode(), label) {
		return nil, errInstallServiceNotLoaded
	}
	if err != nil {
		return nil, err
	}
	return output, nil
}

func (adapter installLaunchctl) Bootout(ctx context.Context, label string) error {
	_, err := adapter.install.run(ctx, "launchctl", "bootout", adapter.install.domain()+"/"+label)
	return err
}

func (adapter installLaunchctl) Bootstrap(ctx context.Context, definition string) error {
	_, err := adapter.install.run(ctx, "launchctl", "bootstrap", adapter.install.domain(), definition)
	return err
}
