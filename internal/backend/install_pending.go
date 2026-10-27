package backend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

const installFinishPending = "finish-pending-installation"

type installPendingError struct{ identity BuildInfo }

func (err *installPendingError) Error() string {
	build := "untracked"
	if err.identity.Build != nil {
		build = fmt.Sprint(*err.identity.Build)
	}
	commit := "untracked"
	if err.identity.Commit != nil {
		commit = *err.identity.Commit
	}
	return fmt.Sprintf("Another candidate installation is pending: build %s, version %s, commit %s. Finish that exact build before replacing it. If its app is no longer available, retrieve that build from the release history. Preserve the pending installation records and any retained application backup.", build, err.identity.Version, commit)
}

// Recovery completes the exact retained app using the current controller. It
// never clears or replaces an unfinished identity to let a different build in.
func (install *installer) pendingInstallation(ctx context.Context) (*installer, error) {
	var pending *installPendingError
	if err := install.validatePayload(); !errors.As(err, &pending) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("The pending installation changed; check setup again")
	}
	source := install.app
	if !install.appBundle {
		source = filepath.Join(install.payload, "Agent Queue.app")
	}
	if err := install.managedVerify(ctx, source); err != nil {
		return nil, err
	}
	retained := *install
	retained.appBundle = true
	retained.payload = filepath.Join(install.app, "Contents/Resources/payload")
	retained.prepareReplacement = false
	retained.unregisterServices = false
	retained.pendingIdentity = &pending.identity
	if err := retained.validatePayload(); err != nil {
		return nil, fmt.Errorf("The exact pending app cannot be recovered: %w", err)
	}
	if !retained.identity.Equal(pending.identity) {
		return nil, pending
	}
	if retained.managed {
		if err := retained.managedVerify(ctx, retained.app); err != nil {
			return nil, err
		}
	} else {
		for _, target := range []string{filepath.Join(retained.payload, "source/bin/agent-heavy"), filepath.Join(retained.payload, "source/bin/agent-remote"), retained.app} {
			if _, err := retained.run(ctx, "/usr/bin/codesign", "--verify", "--strict", "--verbose=2", target); err != nil {
				return nil, fmt.Errorf("Retained app signature verification failed: %w", err)
			}
		}
	}
	return &retained, nil
}

func (install *installer) finishPendingInstallation(ctx context.Context) error {
	retained, err := install.pendingInstallation(ctx)
	if err != nil {
		return err
	}
	// execute revalidates the exact identity under its normal locks and retains
	// all drain, signature, ownership, custody, approval and readiness gates.
	if err := retained.execute(ctx); err != nil {
		return fmt.Errorf("The retained pending installation could not complete; its build identity and application custody were preserved: %w", err)
	}
	return nil
}
