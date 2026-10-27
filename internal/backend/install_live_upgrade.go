package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Installer custody is separate from the daemon's one-use leadership receipt.
// Retain the original active attempts even after the new daemon adopts them, so
// a retry can finish publication without stopping that daemon a second time.
type managedLiveUpgrade struct {
	SchemaVersion int                        `json:"schemaVersion"`
	App           string                     `json:"app"`
	Target        BuildInfo                  `json:"target"`
	Token         string                     `json:"token"`
	TargetSHA256  string                     `json:"targetSHA256"`
	Attempts      []runtimeGenerationAttempt `json:"attempts"`
	prepared      bool
	released      bool
	adopted       bool
}

func (install *installer) liveJournalPath() string {
	return filepath.Join(install.root, "live-upgrade.json")
}

func (install *installer) liveGate(ctx context.Context) *Gate {
	deadline := time.Now().Add(15 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	return &Gate{State: install.state, ControlDeadline: deadline, controlContext: ctx}
}

func (install *installer) liveControl(ctx context.Context, operation string, payload Object) (Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if install.queueControl != nil {
		return install.queueControl(ctx, operation, payload)
	}
	return install.liveGate(ctx).QueueControl(operation, payload)
}

func (install *installer) liveTarget(ctx context.Context) (string, bool) {
	app := install.app
	if !install.appBundle {
		app = filepath.Join(install.payload, "Agent Queue.app")
	}
	helper := filepath.Join(app, "Contents/MacOS/agent-heavy")
	output, err := install.run(ctx, helper, "handover-capabilities", "--json")
	if err != nil {
		return "", false
	}
	value, err := decodeObject(output)
	if err != nil || len(value) != 3 {
		return "", false
	}
	schema, _ := integer(value["schemaVersion"])
	protocol, _ := integer(value["protocol"])
	database, _ := integer(value["queueSchema"])
	if schema != 1 || protocol != 1 || database != 2 {
		return "", false
	}
	raw, err := installReadPayload(app, helper, updateMaxArchive, true)
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), true
}

func liveAllowed(install *installer, plan *managedPlan) bool {
	return !install.prepareReplacement && !install.unregisterServices && plan.previous != nil &&
		!plan.python && !plan.pueue && len(plan.legacy) == 0
}

func liveAttempts(value Object) ([]runtimeGenerationAttempt, error) {
	values, ok := value["attempts"].([]any)
	if !ok || len(values) > 4096 {
		return nil, errors.New("Invalid daemon handover attempt inventory")
	}
	attempts := []runtimeGenerationAttempt{}
	seen := map[int]bool{}
	for _, raw := range values {
		entry := obj(raw)
		id, ok := integer(entry["id"])
		if !ok || id < 0 || seen[id] || !validToken(text(entry["token"])) {
			return nil, errors.New("Invalid daemon handover job identity")
		}
		seen[id] = true
		phase := text(entry["phase"])
		if phase != "queued" && phase != "running" && phase != "outcome" && phase != "done" {
			return nil, errors.New("An uncertain job attempt prevents live replacement")
		}
		worker := obj(entry["worker"])
		attemptID := text(entry["attempt"])
		if phase == "running" && worker == nil || phase == "queued" && (worker != nil || attemptID != "") || attemptID != "" && !validToken(attemptID) {
			return nil, errors.New("Invalid live worker custody")
		}
		caller, err := decodeQueueHandoverIdentity(entry["caller"])
		if err != nil {
			return nil, err
		}
		attempt := runtimeGenerationAttempt{ID: id, Token: text(entry["token"]), Attempt: attemptID, Worker: worker, Caller: caller}
		if err := validateRuntimeGenerationAttempt(attempt); err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, nil
}

func (install *installer) readLiveJournal() (*managedLiveUpgrade, []byte, error) {
	raw, err := updateReadOwned(install.liveJournalPath(), 4*1024*1024)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	fields, err := metadataFields(raw, "schemaVersion", "app", "target", "token", "targetSHA256", "attempts")
	if err != nil {
		return nil, nil, err
	}
	if _, err := ValidateBuildInfo(fields["target"]); err != nil {
		return nil, nil, err
	}
	var attempts []json.RawMessage
	if err := json.Unmarshal(fields["attempts"], &attempts); err != nil {
		return nil, nil, err
	}
	for _, attempt := range attempts {
		if err := validateRuntimeGenerationAttemptFields(attempt); err != nil {
			return nil, nil, err
		}
	}
	var live managedLiveUpgrade
	if err := json.Unmarshal(raw, &live); err != nil {
		return nil, nil, err
	}
	digest, err := hex.DecodeString(live.TargetSHA256)
	if err != nil || len(digest) != 32 || live.SchemaVersion != 1 || live.App != install.app || !validToken(live.Token) || live.Attempts == nil || len(live.Attempts) > 4096 {
		return nil, nil, errors.New("Invalid retained live-upgrade custody; evidence preserved")
	}
	seen := map[int]bool{}
	for _, attempt := range live.Attempts {
		if err := validateRuntimeGenerationAttempt(attempt); err != nil {
			return nil, nil, err
		}
		if seen[attempt.ID] {
			return nil, nil, errors.New("Duplicate live-upgrade job custody")
		}
		seen[attempt.ID] = true
	}
	return &live, raw, nil
}

func (install *installer) liveReceipt(live *managedLiveUpgrade) (Object, error) {
	receipt, err := readQueueHandoverReceipt(&Gate{State: install.state})
	if err != nil {
		return nil, err
	}
	if receipt.Token != live.Token || receipt.TargetSHA256 != live.TargetSHA256 {
		return nil, errors.New("Daemon handover differs from the retained installer custody")
	}
	return queueHandoverObject(receipt)
}

func (install *installer) resumeManagedLive(ctx context.Context, plan *managedPlan) (*managedLiveUpgrade, error) {
	if !liveAllowed(install, plan) {
		return nil, nil
	}
	live, _, err := install.readLiveJournal()
	if err != nil || live == nil {
		return nil, err
	}
	if !live.Target.Equal(install.identity) {
		completed, err := installReadInfo(filepath.Join(install.root, "build-info.json"))
		if err == nil && completed.Equal(live.Target) {
			return nil, nil
		}
		return nil, errors.New("Another live candidate is pending; preserve its journal and finish that exact installation")
	}
	target, supported := install.liveTarget(ctx)
	if !supported || target != live.TargetSHA256 {
		return nil, errors.New("The retained live-upgrade target changed")
	}
	receipt, err := install.liveReceipt(live)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch text(receipt["phase"]) {
	case "prepared", "committed":
		live.prepared = true
	case "released":
		if err := install.validateManagedLive(ctx, live); err != nil {
			return nil, err
		}
		live.prepared, live.released = true, true
	case "consumed":
		if err := validateConsumedQueueHandover(install.liveGate(ctx), live.Token, live.TargetSHA256); err != nil {
			return nil, err
		}
		if !plan.loaded["io.went.agent-heavy"] {
			return nil, errors.New("An adopted queue is no longer running; crash recovery remains authoritative")
		}
		if err := install.managedVerify(ctx, install.app); err != nil {
			return nil, err
		}
		current, err := managedCaptureBundle(install.app)
		if err != nil || !current.Build.Equal(live.Target) {
			return nil, errors.New("Adopted runtime differs from the pending candidate")
		}
		source := install.app
		if !install.appBundle {
			source = filepath.Join(install.payload, "Agent Queue.app")
		}
		selected, err := managedCaptureBundle(source)
		if err != nil || selected.Digest != current.Digest {
			return nil, errors.New("The adopted application is not the exact retained installer candidate")
		}
		if _, err := install.managedSnapshot(ctx); err != nil {
			return nil, err
		}
		live.prepared, live.released, live.adopted = true, true, true
	case "cancelled", "expired":
		return nil, nil
	default:
		return nil, errors.New("Unrecognized daemon handover phase; live replacement refused")
	}
	return live, nil
}

func (install *installer) planManagedLive(ctx context.Context, plan *managedPlan) (*managedLiveUpgrade, error) {
	if live, err := install.resumeManagedLive(ctx, plan); err != nil || live != nil {
		return live, err
	}
	if !liveAllowed(install, plan) || install.appBundle || !plan.loaded["io.went.agent-heavy"] {
		return nil, nil
	}
	target, supported := install.liveTarget(ctx)
	if !supported {
		return nil, nil
	}
	status, err := install.liveControl(ctx, "handover-status", nil)
	if err != nil {
		return nil, nil
	} // Older daemons retain the fully drained path.
	schema, _ := integer(status["schemaVersion"])
	protocol, _ := integer(status["protocol"])
	database, _ := integer(status["queueSchema"])
	if schema != 1 || protocol != 1 || database != 2 || !boolean(status["eligible"]) {
		return nil, nil
	}
	attempts, err := liveAttempts(status)
	if err != nil {
		return nil, err
	}
	if len(attempts) == 0 {
		return nil, nil
	}
	return &managedLiveUpgrade{SchemaVersion: 1, App: install.app, Target: install.identity, Token: token(), TargetSHA256: target, Attempts: attempts}, nil
}

func (install *installer) beginManagedLive(ctx context.Context, live *managedLiveUpgrade) error {
	if live.released || live.adopted {
		return nil
	}
	if previous, raw, err := install.readLiveJournal(); err != nil {
		return err
	} else if previous != nil && previous.Token != live.Token {
		completed, err := installReadInfo(filepath.Join(install.root, "build-info.json"))
		finished := err == nil && completed.Equal(previous.Target)
		if previous.Target.Equal(live.Target) && !finished {
			receipt, receiptErr := install.liveReceipt(previous)
			if !errors.Is(receiptErr, os.ErrNotExist) && (receiptErr != nil || receipt["phase"] != "cancelled" && receipt["phase"] != "expired") {
				return errors.New("The previous live handover must be cancelled or expired before a new token is prepared")
			}
		} else if !finished {
			return errors.New("Another live-upgrade journal must be reconciled before replacement")
		}
		if err := install.managedRemoveFile(install.liveJournalPath(), raw); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(live)
	if err != nil {
		return err
	}
	if err := installAtomic(install.liveJournalPath(), append(raw, '\n'), 0600); err != nil {
		return err
	}
	if live.prepared {
		receipt, err := install.liveReceipt(live)
		if err != nil {
			return err
		}
		if text(receipt["phase"]) == "committed" || text(receipt["phase"]) == "released" {
			return nil
		}
	}
	status, err := install.liveControl(ctx, "prepare-handover", Object{"token": live.Token, "targetSHA256": live.TargetSHA256})
	if err != nil {
		return err
	}
	if status["token"] != live.Token || status["targetSHA256"] != live.TargetSHA256 || status["state"] != "prepared" {
		return errors.New("Daemon did not acknowledge the exact live-upgrade target")
	}
	attempts, err := liveAttempts(status)
	if err != nil {
		return err
	}
	live.Attempts, live.prepared = attempts, true
	raw, err = json.Marshal(live)
	if err != nil {
		return err
	}
	return installAtomic(install.liveJournalPath(), append(raw, '\n'), 0600)
}

func (install *installer) commitManagedLive(ctx context.Context, live *managedLiveUpgrade) error {
	if live.released || live.adopted {
		return nil
	}
	receipt, err := install.liveReceipt(live)
	if err != nil {
		return err
	}
	if text(receipt["phase"]) != "committed" && text(receipt["phase"]) != "released" {
		response, err := install.liveControl(ctx, "commit-handover", Object{"token": live.Token})
		if err != nil {
			// A socket can close after the daemon durably commits but before its
			// acknowledgement arrives. Reconcile that exact receipt instead of
			// issuing another commit or abandoning an accepted handover.
			current, readErr := install.liveReceipt(live)
			if readErr != nil || current["phase"] != "committed" && current["phase"] != "released" {
				return errors.Join(err, readErr)
			}
		} else if response["token"] != live.Token || response["state"] != "committed" {
			return errors.New("Daemon did not acknowledge the retained handover token")
		}
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := install.validateManagedLive(deadline, live); err == nil {
			live.released = true
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("The old daemon did not release its verified active attempts: %w", deadline.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (install *installer) validateManagedLive(ctx context.Context, live *managedLiveUpgrade) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := validateReleasedQueueHandover(install.liveGate(ctx), live.Token, live.TargetSHA256)
	return err
}

func (install *installer) cancelManagedLive(live *managedLiveUpgrade) {
	if live.released || live.adopted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := install.liveReceipt(live)
	if err != nil || text(receipt["phase"]) != "prepared" {
		return
	}
	if _, err := install.liveControl(ctx, "cancel-handover", Object{"token": live.Token}); err != nil {
		fmt.Fprintf(os.Stderr, "Agent Queue live-upgrade cancellation could not be confirmed: %v\n", err)
	}
}

func (install *installer) finishManagedLive(live *managedLiveUpgrade) error {
	current, raw, err := install.readLiveJournal()
	if err != nil {
		return err
	}
	if current == nil || current.Token != live.Token {
		return errors.New("Live-upgrade journal changed before completion")
	}
	return install.managedRemoveFile(install.liveJournalPath(), raw)
}
