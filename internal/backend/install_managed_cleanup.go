package backend

import (
	"context"
	"errors"
	"strconv"
)

// Called under installation, scheduling and validated private-file locks. A
// managed plan verifies bundle/registration/process ownership before returning
// its cleanup blocker; legacy LaunchAgent files are not its ownership evidence.
func (install *installer) recoverManagedCleanup(ctx context.Context, id int) error {
	if _, err := install.planManaged(ctx); !errors.Is(err, errInstallPendingCleanup) {
		if err != nil {
			return err
		}
		return errors.New("The managed queue no longer requires cleanup")
	}
	snapshot, err := install.managedSnapshot(ctx)
	if err != nil {
		return err
	}
	task := obj(obj(snapshot["tasks"])[strconv.Itoa(id)])
	if task == nil {
		return errors.New("The selected managed job no longer exists")
	}
	status, _, err := TaskStatus(task)
	if err != nil {
		return err
	}
	if status != "Done" {
		return errors.New("Only a completed managed job can be cleaned during installation")
	}
	gate := install.recoveryGate(nil)
	gate.controlContext = ctx
	gate.queueControl = func(operation string, _ Object) (Object, error) {
		if operation != "snapshot" {
			return nil, errors.New("Unsupported managed cleanup operation")
		}
		return install.managedSnapshot(ctx)
	}
	if err := installValidateRecoveryFiles(gate, id); err != nil {
		return err
	}
	return Cleanup(gate, id)
}
