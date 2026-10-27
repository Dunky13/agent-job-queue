package remote

import (
	"errors"
	"fmt"

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
			return errors.New("The installed phone service definition is unavailable")
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
