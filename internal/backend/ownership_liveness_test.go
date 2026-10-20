package backend

import (
	"errors"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

type groupProbeFixture struct {
	probes                    []error
	snapshots                 [][]groupState
	snapshotErrors            []error
	probesRead, snapshotsRead int
}

func (f *groupProbeFixture) run(t *testing.T) (bool, error) {
	t.Helper()
	return groupExistsWith(42, func(pgid int, signal syscall.Signal) error {
		t.Helper()
		if pgid != -42 || signal != 0 {
			t.Fatal("liveness check attempted a signal or wrong process group")
		}
		if f.probesRead >= len(f.probes) {
			t.Fatal("unexpected additional kernel probe")
		}
		e := f.probes[f.probesRead]
		f.probesRead++
		return e
	}, func(pgid int) ([]groupState, error) {
		t.Helper()
		if pgid != 42 || f.snapshotsRead >= len(f.snapshots) {
			t.Fatal("unexpected process snapshot")
		}
		i := f.snapshotsRead
		f.snapshotsRead++
		var e error
		if i < len(f.snapshotErrors) {
			e = f.snapshotErrors[i]
		}
		return f.snapshots[i], e
	})
}
func TestGroupLivenessPreservesUncertainOwnership(t *testing.T) {
	zombie := groupState{101, "same birth", "Z"}
	for _, test := range []struct {
		name    string
		fixture groupProbeFixture
	}{
		{"empty-first-snapshot-still-eperm", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{}}}},
		{"permission-error-with-live-member", groupProbeFixture{probes: []error{syscall.EPERM}, snapshots: [][]groupState{{{101, "same birth", "S"}}}}},
		{"new-pid", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {{102, "other birth", "Z"}}}}},
		{"changed-birth", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {{101, "new birth", "Z"}}}}},
		{"live-second-snapshot", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {{101, "same birth", "S"}}}}},
		{"empty-second-snapshot-still-eperm", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {}}}},
		{"incomplete-first-snapshot", groupProbeFixture{probes: []error{syscall.EPERM}, snapshots: [][]groupState{{}}, snapshotErrors: []error{errors.New("incomplete snapshot")}}},
		{"incomplete-second-snapshot", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {}}, snapshotErrors: []error{nil, errors.New("incomplete snapshot")}}},
		{"missing-birth", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {{101, "", "Z"}}}}},
		{"invalid-pid", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {{1, "same birth", "Z"}}}}},
		{"duplicate-identity", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie, {102, "other birth", "Z"}}, {zombie, zombie}}}},
		{"unexpected-kernel-error", groupProbeFixture{probes: []error{syscall.EIO}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			exists, e := test.fixture.run(t)
			if e == nil || exists {
				t.Fatalf("uncertain ownership was accepted: exists=%v error=%v", exists, e)
			}
		})
	}
}

// Reap the sole owned zombie exactly between the real kernel's EPERM and a
// complete process listing. This reproduces disappearance across observations
// without a scheduler race or a probability-dependent process farm.
func TestGroupLivenessReapedAfterDeniedProbe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin zombie group errno")
	}
	t.Run("empty-first-snapshot", reapedBeforeFirstSnapshot)
	t.Run("shrinking-second-snapshot", reapedBetweenSnapshots)
}
func killedChild(t *testing.T, pgid int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	return cmd
}
func awaitZombieGroup(t *testing.T, pgid int) {
	t.Helper()
	if e := syscall.Kill(-pgid, syscall.SIGKILL); e != nil {
		t.Fatal(e)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		e := syscall.Kill(-pgid, 0)
		if errors.Is(e, syscall.EPERM) {
			return
		}
		if e != nil {
			t.Fatalf("unexpected owned group errno before reaping: %v", e)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("owned killed child did not become a zombie")
}
func reapedBeforeFirstSnapshot(t *testing.T) {
	cmd := killedChild(t, 0)
	pgid := cmd.Process.Pid
	reaped := false
	defer func() {
		if !reaped {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()
	awaitZombieGroup(t, pgid)
	probes := []error{}
	var observed []groupState
	exists, e := groupExistsWith(pgid, func(group int, sig syscall.Signal) error {
		e := syscall.Kill(group, sig)
		probes = append(probes, e)
		return e
	}, func(group int) ([]groupState, error) {
		reaped = true
		_ = cmd.Wait()
		members, e := processGroupStates(group)
		observed = members
		return members, e
	})
	if e != nil || exists {
		t.Fatalf("reaped owned group was not reconciled: exists=%v error=%v kernelProbes=%v completeSnapshot=%+v", exists, e, probes, observed)
	}
	if len(probes) != 2 || !errors.Is(probes[0], syscall.EPERM) || !errors.Is(probes[1], syscall.ESRCH) || len(observed) != 0 {
		t.Fatalf("test did not exercise EPERM -> complete empty snapshot -> ESRCH: probes=%v snapshot=%v", probes, observed)
	}
}
func reapedBetweenSnapshots(t *testing.T) {
	leader := killedChild(t, 0)
	pgid := leader.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL); _ = leader.Wait() }()
	child := killedChild(t, pgid)
	childReaped := false
	defer func() {
		if !childReaped {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	awaitZombieGroup(t, pgid)
	probes := []error{}
	snapshots := [][]groupState{}
	exists, e := groupExistsWith(pgid, func(group int, sig syscall.Signal) error {
		e := syscall.Kill(group, sig)
		probes = append(probes, e)
		return e
	}, func(group int) ([]groupState, error) {
		members, e := processGroupStates(group)
		snapshots = append(snapshots, members)
		if len(snapshots) == 1 {
			childReaped = true
			_ = child.Wait()
		}
		return members, e
	})
	if e != nil || exists {
		t.Fatalf("shrinking owned zombie group was not reconciled: exists=%v error=%v kernelProbes=%v completeSnapshots=%+v", exists, e, probes, snapshots)
	}
	if len(snapshots) != 2 || len(snapshots[0]) != 2 || len(snapshots[1]) != 1 || snapshots[1][0].PID != pgid || len(probes) < 2 || !errors.Is(probes[0], syscall.EPERM) || !errors.Is(probes[1], syscall.EPERM) {
		t.Fatalf("test did not exercise a strict owned zombie subset: probes=%v snapshots=%+v", probes, snapshots)
	}
}

func TestGroupLivenessAuthoritativeKernelResults(t *testing.T) {
	zombie := groupState{101, "same birth", "Z"}
	for _, test := range []struct {
		name    string
		fixture groupProbeFixture
		exists  bool
	}{
		{"live", groupProbeFixture{probes: []error{nil}}, true},
		{"absent", groupProbeFixture{probes: []error{syscall.ESRCH}}, false},
		{"gone-after-zombie-snapshot", groupProbeFixture{probes: []error{syscall.EPERM, syscall.ESRCH}, snapshots: [][]groupState{{zombie}}}, false},
		{"live-after-zombie-snapshot", groupProbeFixture{probes: []error{syscall.EPERM, nil}, snapshots: [][]groupState{{zombie}}}, true},
		{"stable-zombies", groupProbeFixture{probes: []error{syscall.EPERM, syscall.EPERM}, snapshots: [][]groupState{{zombie}, {zombie}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			exists, e := test.fixture.run(t)
			if e != nil || exists != test.exists {
				t.Fatalf("kernel liveness result lost: exists=%v error=%v", exists, e)
			}
		})
	}
}
