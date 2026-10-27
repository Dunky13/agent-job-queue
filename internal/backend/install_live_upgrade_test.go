package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func liveManagedFixture(t *testing.T) *managedFixture {
	t.Helper()
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	m.tasks = Object{"7": Object{"id": 7, "token": strings.Repeat("a", 32), "status": Object{"Running": Object{}}}}
	active := runtimeGenerationAttemptFixture(t)
	original := m.installer.command
	m.installer.command = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) == 3 && args[1] == "handover-capabilities" && args[2] == "--json" {
			return []byte(`{"schemaVersion":1,"protocol":1,"queueSchema":2}`), nil
		}
		return original(ctx, args)
	}
	m.installer.queueControl = func(_ context.Context, operation string, _ Object) (Object, error) {
		if operation != "handover-status" {
			return nil, errors.New("unexpected mutation")
		}
		return Object{"schemaVersion": 1, "protocol": 1, "queueSchema": 2, "eligible": true, "state": "idle", "attempts": []any{
			Object{"id": 7, "token": strings.Repeat("a", 32), "attempt": strings.Repeat("b", 32), "phase": "running", "worker": active.Worker, "caller": active.Caller},
		}}, nil
	}
	return m
}

func TestManagedLiveUpgradePreflightKeepsActiveJobsAndIsReadOnly(t *testing.T) {
	m := liveManagedFixture(t)
	before := installTreeWitness(t, m.installer.home)
	m.calls = nil
	report := m.installer.preflight(context.Background())
	if len(report.Issues) != 0 {
		t.Fatalf("compatible live upgrade still requires queue drain: %+v", report.Issues)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, m.installer.home)) {
		t.Fatal("live preflight changed installation state")
	}
	managedRequireNoServiceMutation(t, m.calls)
	plan, err := m.installer.planManaged(context.Background())
	if err != nil || plan.live == nil || len(plan.live.Attempts) != 1 || plan.live.Attempts[0].ID != 7 {
		t.Fatal("live plan lost active worker custody", plan, err)
	}
	if _, err := os.Stat(m.installer.liveJournalPath()); !os.IsNotExist(err) {
		t.Fatal("read-only preflight wrote a handover journal", err)
	}
}

func TestManagedLiveUpgradeRetainsLegacyDrainAndExplicitRemovalGates(t *testing.T) {
	for _, kind := range []string{"legacy-daemon", "legacy-client", "incompatible-target", "remove", "external-replacement"} {
		t.Run(kind, func(t *testing.T) {
			m := liveManagedFixture(t)
			i := m.installer
			switch kind {
			case "legacy-daemon":
				i.queueControl = func(context.Context, string, Object) (Object, error) {
					return nil, errors.New("Unknown queue operation")
				}
			case "legacy-client":
				original := i.queueControl
				i.queueControl = func(ctx context.Context, op string, payload Object) (Object, error) {
					value, err := original(ctx, op, payload)
					if value != nil {
						value["eligible"] = false
					}
					return value, err
				}
			case "incompatible-target":
				original := i.command
				i.command = func(ctx context.Context, args []string) ([]byte, error) {
					if len(args) == 3 && args[1] == "handover-capabilities" {
						return []byte(`{"schemaVersion":1,"protocol":1,"queueSchema":3}`), nil
					}
					return original(ctx, args)
				}
			case "remove":
				i.unregisterServices = true
			case "external-replacement":
				i.prepareReplacement = true
			}
			before := installTreeWitness(t, i.home)
			if _, err := i.planManaged(context.Background()); err == nil {
				t.Fatal("unsupported active upgrade bypassed its drain gate")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("drain refusal mutated state")
			}
		})
	}
}

func TestManagedLiveUpgradeRefusesChangedPendingTarget(t *testing.T) {
	m := liveManagedFixture(t)
	i := m.installer
	plan, err := i.planManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	live := plan.live
	raw := mustMetadataJSON(t, live)
	integrationTestWrite(t, i.liveJournalPath(), raw)
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

func TestManagedLiveUpgradeCompletesWhenLastCallerFinishesBeforePreparation(t *testing.T) {
	m := liveManagedFixture(t)
	i := m.installer
	plan, err := i.planManaged(context.Background())
	if err != nil || plan.live == nil || len(plan.live.Attempts) != 1 {
		t.Fatal("initial live plan", plan, err)
	}
	// The daemon's preparation is authoritative: the previously active job and
	// its caller have both finished before dispatch is frozen.
	i.queueControl = func(_ context.Context, operation string, payload Object) (Object, error) {
		if operation != "prepare-handover" {
			return nil, errors.New("unexpected mutation")
		}
		return Object{"schemaVersion": 1, "protocol": 1, "queueSchema": 2,
			"state": "prepared", "token": payload["token"], "targetSHA256": payload["targetSHA256"], "attempts": []any{}}, nil
	}
	if err := i.beginManagedLive(context.Background(), plan.live); err != nil {
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
