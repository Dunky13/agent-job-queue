package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// A Finder copy can take over an existing managed location without deleting or
// moving either app. Relocation uses the fully drained lifecycle: outstanding
// callers and workers keep their original executable until they finish.
type managedRelocation struct {
	previous  appservices.Installation
	old, next managedBundleIdentity
}

var errInstallRelocationWaiting = errors.New("Waiting for original job callers and retained runtimes before changing application locations; their outcomes and executable custody are preserved")

func (install *installer) planManagedRelocation(ctx context.Context, previous *appservices.Installation) (*managedRelocation, error) {
	if !install.appBundle || install.prepareReplacement || install.unregisterServices {
		return nil, errors.New("Open the new Agent Queue app from Applications to continue setup at its new location")
	}
	if err := appservices.ValidateApp(install.home, previous.App); err != nil {
		return nil, err
	}
	old := *install
	old.app, old.payload = previous.App, filepath.Join(previous.App, "Contents/Resources/payload")
	// A retained publication must be completed by its exact candidate first.
	if err := old.managedBackupAccess(); err != nil {
		return nil, err
	}
	if err := old.managedVerify(ctx, previous.App); err != nil {
		return nil, err
	}
	source, err := managedCaptureBundle(previous.App)
	if err != nil {
		return nil, err
	}
	completed, err := installReadInfo(filepath.Join(install.root, "build-info.json"))
	if err != nil || !source.Build.Equal(completed) {
		return nil, errors.New("The recorded app differs from the completed installation; finish its exact pending setup before changing locations")
	}
	target, err := managedCaptureBundle(install.app)
	if err != nil {
		return nil, err
	}
	if !target.Build.Equal(install.identity) {
		return nil, errors.New("The new app changed from the selected installation build")
	}
	return &managedRelocation{previous: *previous, old: source, next: target}, nil
}

func (install *installer) requireManagedRelocation(ctx context.Context, relocation *managedRelocation) error {
	if relocation == nil {
		return nil
	}
	previous, err := appservices.Load(install.home)
	if err != nil {
		return err
	}
	if previous == nil || *previous != relocation.previous {
		return errors.New("The recorded application changed during relocation; no further service was changed")
	}
	for app, expected := range map[string]managedBundleIdentity{previous.App: relocation.old, install.app: relocation.next} {
		if err := managedRequireBundle(app, expected); err != nil {
			return err
		}
		if err := install.managedVerify(ctx, app); err != nil {
			return err
		}
	}
	return nil
}

func (p *managedPlan) serviceApp(target string) string {
	if p.relocation != nil {
		return p.relocation.previous.App
	}
	return target
}

// Reclaim only installer-owned generations, using their original app binding
// and the normal durable-attempt/liveness checks. This runs under all lifecycle
// locks before publishing a new pending identity or stopping any service.
func (install *installer) cleanupRelocationRuntimes(ctx context.Context, relocation *managedRelocation) error {
	if relocation == nil {
		return nil
	}
	if err := install.requireManagedRelocation(ctx, relocation); err != nil {
		return err
	}
	old := *install
	old.app = relocation.previous.App
	old.payload = filepath.Join(old.app, "Contents/Resources/payload")
	if err := old.cleanupManagedRuntimes(ctx); err != nil {
		return err
	}
	parent, err := integrationParent(filepath.Join(old.runtimeGenerationRecords(), ".inspect"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errInstallRelocationWaiting
	}
	return nil
}
