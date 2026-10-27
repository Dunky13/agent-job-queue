package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
)

// Lifecycle calls execute only the installed owning app. Missing or malformed
// ownership metadata never falls back to writing an unmanaged LaunchAgent.
func (c *controller) managedServiceState(action string) (string, error) {
	if err := appservices.ValidateApp(c.p.home, c.p.managedApp); err != nil {
		return "", fmt.Errorf("Cannot verify the installed phone service owner: %w", err)
	}
	arguments, err := appservices.Arguments(c.p.managedApp, action, "remote")
	if err != nil {
		return "", err
	}
	var output []byte
	if c.managedCommand != nil {
		output, err = c.managedCommand(arguments)
	} else {
		// A pending-approval mutation returns JSON and exit 2. Decode its state
		// even on that exit; only the requested observed end state can succeed.
		timeout := appservices.NativeCommandTimeout
		if action == "status" {
			timeout = appservices.NativeStatusTimeout
		}
		output, err = c.command(arguments, nil, timeout, false)
	}
	if err != nil {
		return "", err
	}
	status, err := appservices.DecodeStatus(output, "remote")
	if err != nil {
		return "", fmt.Errorf("Invalid native phone service response: %w", err)
	}
	return status.Status, nil
}

func (c *controller) managedServiceAction(action string) error {
	if action != "start" && action != "stop" {
		return errors.New("Unsupported managed phone service action")
	}
	state, err := c.managedServiceState("status")
	if err != nil {
		return err
	}
	if action == "stop" {
		if state == "notRegistered" {
			return nil
		}
		if state == "notFound" {
			return c.managedServiceAbsent()
		}
		state, err = c.managedServiceState("unregister")
		if err != nil {
			return err
		}
		if state != "notRegistered" {
			return errors.New("Phone service removal did not finish; review Login Items in System Settings")
		}
		return nil
	}
	if state == "requiresApproval" {
		return errors.New("Allow Agent Queue's background services in Login Items settings before enabling phone access")
	}
	if state == "enabled" {
		return nil
	}
	if state != "notRegistered" && state != "notFound" {
		return errors.New("The installed phone service definition is unavailable")
	}
	state, err = c.managedServiceState("register")
	if err != nil {
		return err
	}
	if state == "requiresApproval" {
		return errors.New("Allow Agent Queue's background services in Login Items settings before enabling phone access")
	}
	if state != "enabled" {
		return errors.New("Phone service registration did not finish")
	}
	return nil
}

// A framework lookup miss alone cannot prove shutdown. stopOwnedService still
// stops/reconciles the owned supervisor and children after this launchd check.
func (c *controller) managedServiceAbsent() error {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	newCommand := c.newCommand
	if newCommand == nil {
		newCommand = exec.CommandContext
	}
	cmd := newCommand(ctx, "/bin/launchctl", "print", fmt.Sprintf("gui/%d/io.went.agent-remote", os.Getuid()))
	cmd.Env = controlEnvironment(false)
	cmd.WaitDelay = time.Second
	output := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	var exited *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exited) && appservices.LaunchServiceAbsent(output.Bytes(), exited.ExitCode(), "io.went.agent-remote") {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("Cannot verify phone service absence: %w", ctx.Err())
	}
	if err == nil {
		return errors.New("The phone service is still loaded despite its lookup miss; ownership checks remain required")
	}
	return fmt.Errorf("Cannot prove the phone service is absent after its lookup failed: %w", err)
}
