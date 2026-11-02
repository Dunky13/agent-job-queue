package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Only an older release writes this journal now, so each test seeds it directly.
func liveResumeFixture(t *testing.T) (*managedFixture, *managedLiveUpgrade) {
	t.Helper()
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	i := m.installer
	digest, err := i.liveTargetDigest()
	if err != nil {
		t.Fatal(err)
	}
	live := &managedLiveUpgrade{SchemaVersion: 1, App: i.app, Target: i.identity, Token: token(), TargetSHA256: digest, Attempts: []runtimeGenerationAttempt{runtimeGenerationAttemptFixture(t)}}
	integrationTestWrite(t, i.liveJournalPath(), mustMetadataJSON(t, live))
	i.queueControl = func(_ context.Context, operation string, _ Object) (Object, error) {
		return nil, errors.New("unexpected queue control: " + operation)
	}
	return m, live
}

func liveResumeReceipt(t *testing.T, i *installer, live *managedLiveUpgrade, phase string) {
	t.Helper()
	daemon, err := queueHandoverIdentity()
	if err != nil {
		t.Fatal(err)
	}
	receipt := queueHandoverReceipt{SchemaVersion: 1, Protocol: queueHandoverProtocol, QueueSchema: queueHandoverSchema, Token: live.Token, TargetSHA256: live.TargetSHA256, Phase: phase, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), DatabaseDevice: 1, DatabaseInode: 1, Daemon: daemon, Attempts: []queueHandoverAttempt{}, QueueStatus: "Running", ParallelLimit: 1}
	value, err := queueHandoverObject(receipt)
	if err != nil {
		t.Fatal(err)
	}
	path := queueHandoverPath(&Gate{State: i.state})
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(path, value); err != nil {
		t.Fatal(err)
	}
}

func TestManagedUpgradeWaitsForActiveJobsWithoutHandover(t *testing.T) {
	for _, kind := range []string{"upgrade", "remove", "external-replacement"} {
		t.Run(kind, func(t *testing.T) {
			m := managedTestFixture(t)
			if err := m.run(t); err != nil {
				t.Fatal(err)
			}
			i := m.installer
			m.tasks = Object{"7": Object{"id": 7, "token": strings.Repeat("a", 32), "status": Object{"Running": Object{}}}}
			i.queueControl = func(_ context.Context, operation string, _ Object) (Object, error) {
				t.Error("setup offered a live handover:", operation)
				return nil, errors.New("unexpected queue control")
			}
			switch kind {
			case "remove":
				i.unregisterServices = true
			case "external-replacement":
				i.prepareReplacement = true
			}
			before := installTreeWitness(t, i.home)
			m.calls = nil
			var drain *installDrainPendingError
			if _, err := i.planManaged(context.Background()); !errors.As(err, &drain) {
				t.Fatal("active work bypassed its drain gate", err)
			}
			managedRequireNoServiceMutation(t, m.calls)
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("drain wait mutated state")
			}
		})
	}
}

// Removal and external replacement never resume, even with a journal on disk.
func TestManagedLiveResumeRetainsExplicitRemovalGates(t *testing.T) {
	for _, kind := range []string{"remove", "external-replacement"} {
		t.Run(kind, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			i := m.installer
			liveResumeReceipt(t, i, live, "prepared")
			m.tasks = Object{"7": Object{"id": 7, "token": strings.Repeat("a", 32), "status": Object{"Running": Object{}}}}
			if kind == "remove" {
				i.unregisterServices = true
			} else {
				i.prepareReplacement = true
			}
			var drain *installDrainPendingError
			if _, err := i.planManaged(context.Background()); !errors.As(err, &drain) {
				t.Fatal("a journal bypassed the removal drain gate", err)
			}
			if _, err := os.Stat(i.liveJournalPath()); err != nil {
				t.Fatal("refusal lost journal evidence", err)
			}
		})
	}
}

func TestManagedLiveResumeRefusesChangedPendingTarget(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	liveResumeReceipt(t, i, live, "prepared")
	// A journal with a substituted target hash cannot authorize active service
	// shutdown, even if its source build metadata still appears current.
	live.TargetSHA256 = strings.Repeat("f", 64)
	integrationTestWrite(t, i.liveJournalPath(), mustMetadataJSON(t, live))
	if _, err := i.planManaged(context.Background()); err == nil {
		t.Fatal("changed live target was accepted")
	}
	if _, err := os.Stat(filepath.Join(i.root, "install-in-progress.json")); !os.IsNotExist(err) {
		t.Fatal("invalid target created pending installation state", err)
	}
}

func TestManagedLiveResumeRefusesAnotherUnfinishedCandidate(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	live.Target = metadataTestInfo(t, "122")
	integrationTestWrite(t, i.liveJournalPath(), mustMetadataJSON(t, live))
	if _, err := i.planManaged(context.Background()); err == nil || !strings.Contains(err.Error(), "Another live candidate is pending") {
		t.Fatal("another build's journal was not refused", err)
	}
	if _, err := os.Stat(i.liveJournalPath()); err != nil {
		t.Fatal("refusal lost journal evidence", err)
	}
}

func TestManagedLiveResumeSelectsOnlyActivePhases(t *testing.T) {
	for _, phase := range []string{"", "cancelled", "expired"} {
		t.Run("inactive-"+phase, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			if phase != "" {
				liveResumeReceipt(t, m.installer, live, phase)
			}
			plan, err := m.installer.planManaged(context.Background())
			if err != nil || plan.live != nil {
				t.Fatal("a journal that binds nothing was resumed", plan, err)
			}
		})
	}
	for _, phase := range []string{"prepared", "committed"} {
		t.Run("active-"+phase, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			liveResumeReceipt(t, m.installer, live, phase)
			plan, err := m.installer.planManaged(context.Background())
			if err != nil || plan.live == nil || plan.live.Token != live.Token || plan.live.released {
				t.Fatal("an active journal was not resumed", plan, err)
			}
		})
	}
	t.Run("unknown phase fails closed", func(t *testing.T) {
		m, live := liveResumeFixture(t)
		liveResumeReceipt(t, m.installer, live, "prepared")
		path := queueHandoverPath(&Gate{State: m.installer.state})
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		integrationTestWrite(t, path, []byte(strings.Replace(string(raw), `"prepared"`, `"mystery"`, 1)))
		if _, err := m.installer.planManaged(context.Background()); err == nil {
			t.Fatal("an unknown handover phase was accepted")
		}
	})
	t.Run("different token fails closed", func(t *testing.T) {
		m, live := liveResumeFixture(t)
		other := *live
		other.Token = token()
		liveResumeReceipt(t, m.installer, &other, "prepared")
		if _, err := m.installer.planManaged(context.Background()); err == nil {
			t.Fatal("another daemon receipt authorized this journal")
		}
	})
}

func TestManagedLiveResumeTakesDaemonAttemptsWhenLastCallerFinished(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	liveResumeReceipt(t, i, live, "prepared")
	plan, err := i.planManaged(context.Background())
	if err != nil || plan.live == nil || len(plan.live.Attempts) != 1 {
		t.Fatal("initial live plan", plan, err)
	}
	// The daemon's preparation is authoritative: the previously active job and
	// its caller have both finished since the older release wrote the journal.
	i.queueControl = func(_ context.Context, operation string, payload Object) (Object, error) {
		if operation != "prepare-handover" {
			return nil, errors.New("unexpected mutation")
		}
		return Object{"schemaVersion": 1, "protocol": 1, "queueSchema": 2,
			"state": "prepared", "token": payload["token"], "targetSHA256": payload["targetSHA256"], "attempts": []any{}}, nil
	}
	if err := i.confirmManagedLive(context.Background(), plan.live); err != nil {
		t.Fatal("empty authoritative preparation", err)
	}
	journal, _, err := i.readLiveJournal()
	if err != nil || journal == nil || journal.Attempts == nil || len(journal.Attempts) != 0 {
		t.Fatal("valid drained handover journal was rejected", journal, err)
	}
	if err := i.finishManagedLive(plan.live); err != nil {
		t.Fatal("completed drained handover could not retire its journal", err)
	}
	if _, err := os.Lstat(i.liveJournalPath()); !os.IsNotExist(err) {
		t.Fatal("completed handover retained an invalid journal", err)
	}
}

// An older release can finish through its drained path and leave the journal.
// Once a later build completes, that journal would read as a pending candidate
// and refuse every further upgrade.
func TestManagedFinishedLiveJournalCannotBlockLaterUpgrades(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	i.queueControl = nil
	for _, build := range []string{"124", "125"} {
		managedCandidate(t, m, build)
		if err := m.run(t); err != nil {
			t.Fatalf("upgrade to %s was blocked by a finished journal: %v", build, err)
		}
		if _, err := os.Lstat(i.liveJournalPath()); !os.IsNotExist(err) {
			t.Fatal("finished journal was retained", live.Target, err)
		}
	}
}
