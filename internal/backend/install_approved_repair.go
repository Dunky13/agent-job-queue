package backend

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"
)

type installApprovedStep struct {
	action   string
	jobID    *int
	revision string
}

// The native automatic flow delegates this fixed issue/action contract here.
// An action advertised by an unrelated blocker never grants repair authority.
func installApprovedIssue(issue installPreflightIssue) (installApprovedStep, bool) {
	has := func(id string) bool {
		return slices.ContainsFunc(issue.Actions, func(action installRecoveryAction) bool { return action.ID == id })
	}
	wait := func() (installApprovedStep, bool) { return installApprovedStep{}, has("wait") }
	switch issue.ID {
	case "pending-installation":
		return installApprovedStep{action: installFinishPending}, has(installFinishPending)
	case "service-definitions-missing":
		return installApprovedStep{action: installRestoreServices}, has(installRestoreServices)
	case "legacy-phone-access", "remote-migration":
		return installApprovedStep{action: "disable-phone-access"}, has("disable-phone-access")
	case "custom-callback":
		return installApprovedStep{action: "reset-callback"}, has("reset-callback")
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
	case "legacy-jobs", "managed-jobs":
		if len(issue.Jobs) == 0 {
			return installApprovedStep{}, false
		}
		for _, job := range issue.Jobs {
			if job.ID < 0 || !slices.Contains([]string{"Running", "Queued"}, job.Status) {
				return installApprovedStep{}, false
			}
		}
		return wait()
	case "installer-busy", "scheduling-busy", "remote-busy", "controller-stopping", "service-approval":
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
	draining := false
	for _, issue := range report.Issues {
		step, supported := installApprovedIssue(issue)
		if !supported {
			return installApprovedStep{}, fmt.Errorf("Automatic setup requires review: %s. %s", issue.Title, issue.Detail)
		}
		if step.action == "reset-callback" {
			if decoded, err := hex.DecodeString(report.Revision); err != nil || len(decoded) != 32 {
				return installApprovedStep{}, errors.New("Automatic callback replacement requires the exact configuration revision")
			}
			step.revision = report.Revision
		}
		steps = append(steps, step)
		busy = busy || slices.Contains([]string{"installer-busy", "scheduling-busy", "remote-busy", "controller-stopping"}, issue.ID)
		draining = draining || (step.action == "" && slices.Contains([]string{"legacy-jobs", "managed-jobs", "pending-cleanup", "service-approval"}, issue.ID))
	}
	if !busy {
		for _, action := range []string{installFinishPending, installRestoreServices, "repair-service", "cleanup-job", "disable-phone-access", "reset-callback"} {
			// Restoring a verified service and cleaning a completed job can help
			// the queue drain. Configuration and phone changes require that drain
			// to finish first, just as their authoritative recovery paths do.
			if draining && !slices.Contains([]string{installRestoreServices, "repair-service", "cleanup-job"}, action) {
				continue
			}
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
	var pendingApproval error
	approvalObserved := false
	approvalResumed := map[string]bool{}
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
			if pendingApproval != nil && !approvalObserved {
				var approval *installApprovalError
				if !errors.As(pendingApproval, &approval) || approvalResumed[approval.service] {
					return pendingApproval
				}
				// Consent can be granted before the first status poll. Permit
				// one custody-checked resume, but fence repeated no-progress exits.
				approvalResumed[approval.service] = true
			}
			reportProgress("AGENT_QUEUE_SETUP_STATE:installing\nSetup requirements are clear. Continuing installation…")
			if err := ctx.Err(); err != nil {
				return err
			}
			err := execute(ctx)
			if errors.Is(err, errInstallRelocationWaiting) {
				reportProgress("AGENT_QUEUE_SETUP_STATE:waiting\n" + err.Error())
				if err := pause(ctx); err != nil {
					return err
				}
				continue
			}
			var approval *installApprovalError
			if errors.As(err, &approval) {
				// Registration can legitimately pause the exact pending candidate
				// for macOS approval. Reinspect it without retrying registration
				// while consent is denied; resume only after read-only status clears.
				pendingApproval, approvalObserved = err, false
				continue
			}
			return err
		}
		if step.action == "" {
			if slices.ContainsFunc(report.Issues, func(issue installPreflightIssue) bool { return issue.ID == "service-approval" }) {
				approvalObserved = true
				reportProgress("AGENT_QUEUE_SETUP_STATE:requires-approval\nWaiting for macOS background service approval. Allow Agent Queue in System Settings > General > Login Items & Extensions. Installation will continue automatically.")
			} else {
				reportProgress("AGENT_QUEUE_SETUP_STATE:waiting\nWaiting for queue work and setup requirements. Installation will continue automatically; active jobs are preserved.")
			}
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
		reportProgress("AGENT_QUEUE_SETUP_STATE:installing\nRunning verified setup repair: " + step.action)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := recover(ctx, step.action, step.jobID, step.revision); err != nil {
			return err
		}
	}
}

func (install *installer) repairAndExecute(ctx context.Context) error {
	previous := install.automaticSetup
	install.automaticSetup = true
	defer func() { install.automaticSetup = previous }()
	// Stop waiting cancels inspections and waits only. An accepted mutation
	// retains its original context until its response, even if progress lags.
	run, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if install.setupStop != nil {
		go func() {
			select {
			case <-install.setupStop:
				cancelRun()
			case <-run.Done():
			}
		}()
	}
	// Queue drain can take as long as the running workload. Each individual
	// inspection, repair and installation still has a finite execution budget.
	return installRunApprovedRepair(run, func(ctx context.Context) installPreflightReport {
		operation, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		report := install.preflight(operation)
		if err := operation.Err(); err != nil {
			report.add("inspection-failed", "Installation inspection did not complete", err.Error(), "")
		}
		return report
	}, func(_ context.Context, action string, jobID *int, revision string) error {
		if action == installFinishPending {
			// Its retained installer owns bounded individual operations and
			// may need an arbitrarily long drain or OS approval wait.
			return install.recover(ctx, action, jobID, revision)
		}
		operation, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return install.recover(operation, action, jobID, revision)
	}, func(_ context.Context) error {
		operation, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return install.execute(operation)
	}, func(ctx context.Context) error {
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
