package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Only an older release writes this journal now, so each test seeds it directly.
func liveResumeFixture(t *testing.T) (*managedFixture, *managedLiveUpgrade) {
	t.Helper()
	m := managedOlderRegistration(t)
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
	liveResumeProcessReceipt(t, i, live, phase, daemon)
}

// A PID outside the system's range cannot be running, on this boot.
func liveResumeDeadProcess(t *testing.T) queueHandoverDaemon {
	t.Helper()
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return queueHandoverDaemon{PID: 2_000_000_111, PGID: 2_000_000_111, BootUUID: boot, Birth: "Fixture gone birth"}
}

func liveResumeProcessReceipt(t *testing.T, i *installer, live *managedLiveUpgrade, phase string, process queueHandoverDaemon) {
	t.Helper()
	receipt := queueHandoverReceipt{SchemaVersion: 1, Protocol: queueHandoverProtocol, QueueSchema: queueHandoverSchema, Token: live.Token, TargetSHA256: live.TargetSHA256, Phase: phase, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), DatabaseDevice: 1, DatabaseInode: 1, Daemon: process, Attempts: []queueHandoverAttempt{}, QueueStatus: "Running", ParallelLimit: 1}
	if phase == "consumed" {
		receipt.Consumer = &process
	}
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

func liveResumeSetTarget(t *testing.T, i *installer, live *managedLiveUpgrade, build string) {
	t.Helper()
	live.Target = metadataTestInfo(t, build)
	integrationTestWrite(t, i.liveJournalPath(), mustMetadataJSON(t, live))
}

func requireLiveJournalGone(t *testing.T, i *installer) {
	t.Helper()
	if _, err := os.Lstat(i.liveJournalPath()); !os.IsNotExist(err) {
		t.Fatal("a spent journal was retained", err)
	}
}

func requireLiveJournalKept(t *testing.T, i *installer) {
	t.Helper()
	if _, err := os.Lstat(i.liveJournalPath()); err != nil {
		t.Fatal("custody evidence was removed", err)
	}
}

func TestManagedUpgradeWaitsForActiveJobsWithoutHandover(t *testing.T) {
	for _, kind := range []string{"upgrade", "remove"} {
		t.Run(kind, func(t *testing.T) {
			m := managedOlderRegistration(t)
			i := m.installer
			m.tasks = Object{"7": Object{"id": 7, "token": strings.Repeat("a", 32), "status": Object{"Running": Object{}}}}
			i.queueControl = func(_ context.Context, operation string, _ Object) (Object, error) {
				t.Error("setup offered a live handover:", operation)
				return nil, errors.New("unexpected queue control")
			}
			i.unregisterServices = kind == "remove"
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

// Removal never resumes, even with a journal on disk.
func TestManagedLiveResumeRetainsExplicitRemovalGates(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	liveResumeReceipt(t, i, live, "prepared")
	m.tasks = Object{"7": Object{"id": 7, "token": strings.Repeat("a", 32), "status": Object{"Running": Object{}}}}
	i.unregisterServices = true
	var drain *installDrainPendingError
	if _, err := i.planManaged(context.Background()); !errors.As(err, &drain) {
		t.Fatal("a journal bypassed the removal drain gate", err)
	}
	if _, err := os.Stat(i.liveJournalPath()); err != nil {
		t.Fatal("refusal lost journal evidence", err)
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

// A journal for another unfinished build refuses only while its receipt binds
// live custody. The refusal names that build and offers no action it cannot take.
func TestManagedLiveJournalForAnotherBuildRefusesOnlyWhileBinding(t *testing.T) {
	for _, phase := range []string{"prepared", "committed", "consumed", "released"} {
		t.Run("binding-"+phase, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			i := m.installer
			liveResumeSetTarget(t, i, live, "124")
			liveResumeReceipt(t, i, live, phase)
			before := installTreeWitness(t, i.home)
			managedCandidate(t, m, "125")
			var pending *installPendingError
			if _, err := i.planManaged(context.Background()); !errors.As(err, &pending) || !pending.identity.Equal(live.Target) || !strings.Contains(err.Error(), "build 124") {
				t.Fatal("binding journal was not refused by name", err)
			}
			report := i.preflight(context.Background())
			if len(report.Issues) != 1 || report.Issues[0].ID != "pending-installation" || !strings.Contains(report.Issues[0].Detail, "build 124") {
				t.Fatalf("refusal does not name the build: %+v", report)
			}
			actions := report.Issues[0].Actions
			if len(actions) != 2 || actions[0].ID != "download-release" || actions[0].Title != "Open release history" || actions[1].ID != "reveal" {
				t.Fatalf("refusal offers an action it cannot take: %+v", actions)
			}
			if _, supported := installApprovedIssue(report.Issues[0]); supported {
				t.Fatal("automatic setup claimed it could finish a build it does not hold")
			}
			if !reflect.DeepEqual(before[i.liveJournalPath()], installTreeWitness(t, i.home)[i.liveJournalPath()]) {
				t.Fatal("refusal changed the journal")
			}
		})
	}
	for name, seed := range map[string]func(*testing.T, *installer, *managedLiveUpgrade){
		"no receipt": func(*testing.T, *installer, *managedLiveUpgrade) {},
		"cancelled":  func(t *testing.T, i *installer, l *managedLiveUpgrade) { liveResumeReceipt(t, i, l, "cancelled") },
		"expired":    func(t *testing.T, i *installer, l *managedLiveUpgrade) { liveResumeReceipt(t, i, l, "expired") },
		"prepared with its daemon gone": func(t *testing.T, i *installer, l *managedLiveUpgrade) {
			liveResumeProcessReceipt(t, i, l, "prepared", liveResumeDeadProcess(t))
		},
		"committed with its daemon gone": func(t *testing.T, i *installer, l *managedLiveUpgrade) {
			liveResumeProcessReceipt(t, i, l, "committed", liveResumeDeadProcess(t))
		},
		"consumed with its consumer gone": func(t *testing.T, i *installer, l *managedLiveUpgrade) {
			liveResumeProcessReceipt(t, i, l, "consumed", liveResumeDeadProcess(t))
		},
	} {
		t.Run("spent-"+name, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			i := m.installer
			i.queueControl = nil
			liveResumeSetTarget(t, i, live, "124")
			seed(t, i, live)
			managedCandidate(t, m, "125")
			if err := m.run(t); err != nil {
				t.Fatal("a journal that binds nothing refused another build", err)
			}
			requireLiveJournalGone(t, i)
		})
	}
}

// Once a newer build completed, exact target T is refused as older, so T's journal
// must not refuse the next build either. Only a live process keeps it on disk.
func TestManagedLiveJournalOlderThanCompletedBuildCannotStrandUpgrades(t *testing.T) {
	for _, phase := range []string{"", "released", "consumed"} {
		t.Run("phase-"+phase, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			i := m.installer
			i.queueControl = nil
			liveResumeSetTarget(t, i, live, "122")
			if phase != "" {
				liveResumeReceipt(t, i, live, phase)
			}
			managedCandidate(t, m, "124")
			if err := m.run(t); err != nil {
				t.Fatal("an older target stranded the upgrade", err)
			}
			if phase == "consumed" {
				requireLiveJournalKept(t, i)
				return
			}
			requireLiveJournalGone(t, i)
		})
	}
}

// Removal leaves the journal alone, so the journal is still there for the reinstall.
func TestManagedLiveJournalSurvivingRemovalCannotStrandReinstall(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	i.queueControl = nil
	liveResumeSetTarget(t, i, live, "124")
	managedRemovalTarget(i)
	if err := m.run(t); err != nil {
		t.Fatal("removal", err)
	}
	requireLiveJournalKept(t, i)
	i.unregisterServices = false
	for _, build := range []string{"125", "126"} {
		managedCandidate(t, m, build)
		if err := m.run(t); err != nil {
			t.Fatalf("build %s after removal: %v", build, err)
		}
		requireLiveJournalGone(t, i)
	}
}

// A same-build reinstall interrupted mid-handover has a completed identity equal
// to its target, so completion alone must not read as finished.
func TestManagedRetireSpentLiveKeepsJournalBindingCustody(t *testing.T) {
	for _, phase := range []string{"prepared", "committed", "released", "consumed"} {
		t.Run(phase, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			i := m.installer
			liveResumeReceipt(t, i, live, phase)
			if err := i.retireSpentLive(); err != nil {
				t.Fatal(err)
			}
			requireLiveJournalKept(t, i)
			managedCandidate(t, m, "124")
			identity, err := i.validateBundle()
			if err != nil {
				t.Fatal(err)
			}
			i.identity = identity
			var pending *installPendingError
			_, err = i.planManaged(context.Background())
			if phase == "consumed" {
				// That running daemon belongs to the finished build, and only delays removal.
				if err != nil {
					t.Fatal("a finished build's daemon blocked the next upgrade", err)
				}
				return
			}
			if !errors.As(err, &pending) {
				t.Fatal("another build took over a mid-handover reinstall", err)
			}
		})
	}
}

// Only resume needs the recorded location. A journal that binds nothing must not
// block setup from the other Applications folder.
func TestManagedSpentLiveJournalDoesNotBlockMovedApp(t *testing.T) {
	m, old := managedRelocationFixture(t, false)
	i := m.installer
	completed, err := installReadInfo(filepath.Join(i.root, "build-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	live := &managedLiveUpgrade{SchemaVersion: 1, App: old, Target: completed, Token: token(), TargetSHA256: strings.Repeat("a", 64), Attempts: []runtimeGenerationAttempt{}}
	integrationTestWrite(t, i.liveJournalPath(), mustMetadataJSON(t, live))
	if err := i.repairAndExecute(context.Background()); err != nil {
		t.Fatal("a finished journal blocked the moved app", err)
	}
	requireLiveJournalGone(t, i)
}

func TestManagedLiveResumeRefusesJournalFromAnotherLocation(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	live.App = filepath.Join(i.home, "Applications/Agent Queue elsewhere.app")
	integrationTestWrite(t, i.liveJournalPath(), mustMetadataJSON(t, live))
	liveResumeReceipt(t, i, live, "prepared")
	if _, err := i.planManaged(context.Background()); err == nil || !strings.Contains(err.Error(), "Invalid retained live-upgrade custody") {
		t.Fatal("this build resumed a journal recorded for another location", err)
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
	for _, phase := range []string{"prepared", "committed", "consumed"} {
		t.Run("process-gone-"+phase, func(t *testing.T) {
			m, live := liveResumeFixture(t)
			liveResumeProcessReceipt(t, m.installer, live, phase, liveResumeDeadProcess(t))
			plan, err := m.installer.planManaged(context.Background())
			if err != nil || plan.live != nil {
				t.Fatal("a journal whose process is gone was resumed", plan, err)
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
	t.Run("changed target under the same token fails closed", func(t *testing.T) {
		m, live := liveResumeFixture(t)
		other := *live
		other.TargetSHA256 = strings.Repeat("f", 64)
		liveResumeReceipt(t, m.installer, &other, "prepared")
		if _, err := m.installer.planManaged(context.Background()); err == nil {
			t.Fatal("a receipt for another target authorized this journal")
		}
	})
}

// A journal is always written before its token is prepared, so another token on
// the receipt proves it never took custody, whatever that receipt itself binds.
func TestManagedLiveJournalWithForeignReceiptNeverTookCustody(t *testing.T) {
	foreign := func(t *testing.T, i *installer, live *managedLiveUpgrade) []byte {
		t.Helper()
		other := *live
		other.Token = token()
		liveResumeReceipt(t, i, &other, "prepared")
		raw, err := os.ReadFile(queueHandoverPath(&Gate{State: i.state}))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	requireReceipt := func(t *testing.T, i *installer, raw []byte) {
		t.Helper()
		if current, err := os.ReadFile(queueHandoverPath(&Gate{State: i.state})); err != nil || string(current) != string(raw) {
			t.Fatal("retiring a journal changed another handover's receipt", err)
		}
	}
	t.Run("same build", func(t *testing.T) {
		m, live := liveResumeFixture(t)
		foreign(t, m.installer, live)
		if plan, err := m.installer.planManaged(context.Background()); err != nil || plan.live != nil {
			t.Fatal("a journal that never took custody was resumed", plan, err)
		}
	})
	t.Run("newer build with a finished journal", func(t *testing.T) {
		m, live := liveResumeFixture(t)
		i := m.installer
		i.queueControl = nil
		raw := foreign(t, i, live)
		managedCandidate(t, m, "124")
		if err := m.run(t); err != nil {
			t.Fatal("a foreign receipt refused a newer build", err)
		}
		requireLiveJournalGone(t, i)
		requireReceipt(t, i, raw)
	})
	t.Run("reinstall after removal", func(t *testing.T) {
		m, live := liveResumeFixture(t)
		i := m.installer
		i.queueControl = nil
		liveResumeSetTarget(t, i, live, "124")
		managedRemovalTarget(i)
		if err := m.run(t); err != nil {
			t.Fatal("removal", err)
		}
		raw := foreign(t, i, live)
		i.unregisterServices = false
		managedCandidate(t, m, "125")
		if report := i.preflight(context.Background()); len(report.Issues) != 0 {
			t.Fatalf("a journal that never took custody blocked preflight: %+v", report.Issues)
		}
		if err := m.run(t); err != nil {
			t.Fatal("a foreign receipt failed the reinstall", err)
		}
		requireLiveJournalGone(t, i)
		requireReceipt(t, i, raw)
	})
}

// Preflight does not assess a journal where resume is skipped, so an unreadable
// one must not fail a run that preflight reported as clear.
func TestManagedUnassessableJournalDoesNotFailReinstall(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	i.queueControl = nil
	liveResumeSetTarget(t, i, live, "124")
	managedRemovalTarget(i)
	if err := m.run(t); err != nil {
		t.Fatal("removal", err)
	}
	liveResumeReceipt(t, i, live, "prepared")
	path := queueHandoverPath(&Gate{State: i.state})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	integrationTestWrite(t, path, []byte(strings.Replace(string(raw), `"prepared"`, `"mystery"`, 1)))
	i.unregisterServices = false
	managedCandidate(t, m, "125")
	if report := i.preflight(context.Background()); len(report.Issues) != 0 {
		t.Fatalf("expected no issues for a skipped journal: %+v", report.Issues)
	}
	if err := m.run(t); err != nil {
		t.Fatal("an unassessable journal failed the reinstall", err)
	}
	requireLiveJournalKept(t, i)
}

// lstart follows the system's time zone, so the same running process can present
// another birth. Only a free daemon lock may turn that into proof of PID reuse.
func TestManagedLiveJournalSurvivesTimeZoneChange(t *testing.T) {
	m, live := liveResumeFixture(t)
	i := m.installer
	daemon, err := queueHandoverIdentity()
	if err != nil {
		t.Fatal(err)
	}
	birth := func(zone string) string {
		command := exec.Command("/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(daemon.PID))
		command.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
		if zone != "" {
			command.Env = append(command.Env, "TZ="+zone)
		}
		raw, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	// Whatever the host's zone, one of these renders the birth differently from it.
	daemon.Birth = birth("Asia/Tokyo")
	if daemon.Birth == birth("") {
		daemon.Birth = birth("America/New_York")
	}
	liveResumeProcessReceipt(t, i, live, "prepared", daemon)
	lock, err := installLock(i.state, "queue-daemon.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, spent, err := i.liveJournalState(live); err != nil || spent {
		lock.Close()
		t.Fatal("a zone change made the running daemon read as gone", spent, err)
	}
	if err := i.retireSpentLive(); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	requireLiveJournalKept(t, i)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, spent, err := i.liveJournalState(live); err != nil || !spent {
		t.Fatal("a free lock did not prove the PID was reused", spent, err)
	}
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
// Nothing else removes it once beginning a handover is gone.
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

func TestManagedCleanupRepairAcceptsAnAlreadyCleanQueue(t *testing.T) {
	m := managedTestFixture(t)
	if err := m.run(t); err != nil {
		t.Fatal(err)
	}
	if err := m.installer.recoverManagedCleanup(context.Background(), 7); err != nil {
		t.Fatal("a caller finishing its own cleanup first failed setup", err)
	}
}
