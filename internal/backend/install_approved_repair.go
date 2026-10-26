package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"
)

type installApprovedStep struct {
	action string
	jobID  *int
}

// Keep this fixed issue/action contract aligned with the native setup model.
// An action advertised by an unrelated blocker never grants repair authority.
func installApprovedIssue(issue installPreflightIssue) (installApprovedStep, bool) {
	has := func(id string) bool {
		return slices.ContainsFunc(issue.Actions, func(action installRecoveryAction) bool { return action.ID == id })
	}
	wait := func() (installApprovedStep, bool) { return installApprovedStep{}, has("wait") }
	switch issue.ID {
	case "service-definitions-missing":
		return installApprovedStep{action: installRestoreServices}, has(installRestoreServices)
	case "legacy-phone-access", "remote-migration":
		return installApprovedStep{action: "disable-phone-access"}, has("disable-phone-access")
	case "service-not-ready":
		if has("repair-service") {
			return installApprovedStep{action: "repair-service"}, true
		}
		return wait()
	case "pending-cleanup":
		if has("cleanup-job") && len(issue.Jobs) > 0 && slices.Contains([]string{"Cleanup pending", "Done"}, issue.Jobs[0].Status) {
			id := issue.Jobs[0].ID
			if id < 0 {
				return installApprovedStep{}, false
			}
			return installApprovedStep{action: "cleanup-job", jobID: &id}, true
		}
		return wait()
	case "legacy-jobs":
		if len(issue.Jobs) == 0 {
			return installApprovedStep{}, false
		}
		for _, job := range issue.Jobs {
			if job.ID < 0 || !slices.Contains([]string{"Running", "Queued"}, job.Status) {
				return installApprovedStep{}, false
			}
		}
		return wait()
	case "installer-busy", "scheduling-busy", "remote-busy", "controller-stopping":
		return wait()
	default:
		return installApprovedStep{}, false
	}
}

func installApprovedNext(report installPreflightReport) (installApprovedStep, error) {
	if report.SchemaVersion != 1 {
		return installApprovedStep{}, errors.New("Unsupported setup report; automatic repair stopped")
	}
	steps := make([]installApprovedStep, 0, len(report.Issues))
	busy := false
	for _, issue := range report.Issues {
		step, supported := installApprovedIssue(issue)
		if !supported {
			return installApprovedStep{}, fmt.Errorf("Automatic setup requires review: %s. %s", issue.Title, issue.Detail)
		}
		steps = append(steps, step)
		busy = busy || slices.Contains([]string{"installer-busy", "scheduling-busy", "remote-busy", "controller-stopping"}, issue.ID)
	}
	if !busy {
		for _, action := range []string{installRestoreServices, "repair-service", "cleanup-job", "disable-phone-access"} {
			for _, step := range steps {
				if step.action == action {
					return step, nil
				}
			}
		}
	}
	return installApprovedStep{}, nil
}

func installRunApprovedRepair(ctx context.Context, inspect func(context.Context) installPreflightReport, recover func(context.Context, string, *int, string) error, execute func(context.Context) error, pause func(context.Context) error, progress func(string)) error {
	performed := map[string]bool{}
	lastMessage := ""
	reportProgress := func(message string) {
		if message != lastMessage {
			progress(message)
			lastMessage = message
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		report := inspect(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		step, err := installApprovedNext(report)
		if err != nil {
			return err
		}
		if len(report.Issues) == 0 {
			reportProgress("Setup requirements are clear. Continuing installation…")
			if err := ctx.Err(); err != nil {
				return err
			}
			return execute(ctx)
		}
		if step.action == "" {
			reportProgress("Waiting for queue work and setup requirements. Installation will continue automatically; active jobs are preserved.")
			if err := pause(ctx); err != nil {
				return err
			}
			continue
		}
		key := step.action
		if step.jobID != nil {
			key += ":" + strconv.Itoa(*step.jobID)
		}
		if performed[key] {
			return errors.New("The approved repair did not clear its setup requirement. No repair was repeated; review the current requirements before approving another attempt")
		}
		if len(performed) >= 128 {
			return errors.New("Setup reached its verified repair limit; review the remaining requirements before approving another attempt")
		}
		performed[key] = true
		reportProgress("Running verified setup repair: " + step.action)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := recover(ctx, step.action, step.jobID, ""); err != nil {
			return err
		}
	}
}

func (install *installer) repairAndExecute(ctx context.Context) error {
	return installRunApprovedRepair(ctx, install.preflight, install.recover, install.execute, func(ctx context.Context) error {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}, func(message string) { fmt.Fprintln(os.Stdout, message) })
}
