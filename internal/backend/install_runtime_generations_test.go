package backend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func runtimeGenerationAttemptFixture(t *testing.T) runtimeGenerationAttempt {
	t.Helper()
	boot := "00000000-0000-0000-0000-000000000001"
	if runtime.GOOS == "darwin" {
		var err error
		boot, err = BootIdentity()
		if err != nil {
			t.Fatal(err)
		}
	}
	pid := 2_000_000_111
	return runtimeGenerationAttempt{ID: 7, Token: strings.Repeat("a", 32), Attempt: strings.Repeat("b", 32),
		Worker: Object{"pid": pid, "pgid": pid, "boot_uuid": boot, "members": []Object{{"pid": pid, "birth": "Fixture worker birth"}}},
		Caller: &queueHandoverDaemon{PID: pid + 1, PGID: pid + 2, BootUUID: boot, Birth: "Fixture caller birth"}}
}

func retainedRuntimeFixture(t *testing.T) (*managedFixture, runtimeGenerationAttempt, managedBundleIdentity) {
	t.Helper()
	m := managedRetainedBackupFixture(t)
	i := m.installer
	before, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	attempt := runtimeGenerationAttemptFixture(t)
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	return m, attempt, before
}

func TestRuntimeGenerationTransfersExactSignedBundleAndSurvivesPublicationCleanup(t *testing.T) {
	m, attempt, expected := retainedRuntimeFixture(t)
	i := m.installer
	if _, err := os.Lstat(i.managedPreviousPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old publication backup was not transferred", err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(expected), expected); err != nil {
		t.Fatal("old runtime lost its exact filesystem identity", err)
	}
	record, _, err := i.readRuntimeGeneration(i.runtimeGenerationRecordPath(expected))
	if err != nil || record.Phase != "retained" || !slicesContainRuntimeAttempt(record.Attempts, attempt) {
		t.Fatal("missing retained worker custody", record, err)
	}
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal("exact retained transfer was not idempotent", err)
	}
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(i.managedReceiptPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publication custody did not retire", err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(expected), expected); err != nil {
		t.Fatal("publication cleanup deleted a live worker runtime", err)
	}
}

func TestRuntimeGenerationRecoversBothTransferCrashBoundaries(t *testing.T) {
	for _, beforeMove := range []bool{false, true} {
		t.Run(map[bool]string{false: "after rename", true: "before rename"}[beforeMove], func(t *testing.T) {
			m, attempt, expected := retainedRuntimeFixture(t)
			i := m.installer
			record, raw, err := i.readRuntimeGeneration(i.runtimeGenerationRecordPath(expected))
			if err != nil {
				t.Fatal(err)
			}
			record.Phase = "transfer"
			if err := i.replaceRuntimeGeneration(record, raw); err != nil {
				t.Fatal(err)
			}
			if beforeMove {
				if err := os.Rename(record.Path, i.managedPreviousPath()); err != nil {
					t.Fatal(err)
				}
			}
			if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
				t.Fatal("exact crash transfer did not recover", err)
			}
			after, _, err := i.readRuntimeGeneration(i.runtimeGenerationRecordPath(expected))
			if err != nil || after.Phase != "retained" {
				t.Fatal("transfer did not finish", after, err)
			}
			if err := managedRequireBundle(after.Path, expected); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeGenerationRefusesUnownedTargetAndInvalidAttemptBeforeMutation(t *testing.T) {
	for _, variant := range []string{"unowned target", "invalid worker", "missing caller", "invalid caller boot", "invalid caller pid", "invalid caller pgid", "missing caller birth", "invalid outcome attempt", "duplicate attempt"} {
		t.Run(variant, func(t *testing.T) {
			m := managedRetainedBackupFixture(t)
			i := m.installer
			identity, err := managedCaptureBundle(i.managedPreviousPath())
			if err != nil {
				t.Fatal(err)
			}
			attempt := runtimeGenerationAttemptFixture(t)
			attempts := []runtimeGenerationAttempt{attempt}
			switch variant {
			case "unowned target":
				if err := os.MkdirAll(i.runtimeGenerationPath(identity), 0700); err != nil {
					t.Fatal(err)
				}
			case "invalid worker":
				attempts[0].Worker = Object{}
			case "missing caller":
				attempts[0].Caller = nil
			case "invalid caller boot":
				attempts[0].Caller.BootUUID = "unknown"
			case "invalid caller pid":
				attempts[0].Caller.PID = 1
			case "invalid caller pgid":
				attempts[0].Caller.PGID = 1
			case "missing caller birth":
				attempts[0].Caller.Birth = " "
			case "invalid outcome attempt":
				attempts[0].Worker = nil
				attempts[0].Attempt = "invalid"
			case "duplicate attempt":
				attempts = append(attempts, attempt)
			}
			before := installTreeWitness(t, i.home)
			if err := i.retainManagedRuntime(context.Background(), attempts); err == nil {
				t.Fatal("uncertain generation accepted")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("rejected generation changed executable or receipt custody")
			}
		})
	}
}

func TestRuntimeGenerationRejectsSubstitutedBundleAndChangedAttempt(t *testing.T) {
	for _, variant := range []string{"inode", "bytes", "symlink", "attempt", "caller"} {
		t.Run(variant, func(t *testing.T) {
			m, attempt, expected := retainedRuntimeFixture(t)
			i := m.installer
			path := i.runtimeGenerationPath(expected)
			switch variant {
			case "inode", "symlink":
				original := path + ".original"
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				if variant == "inode" {
					if err := installCopyTree(original, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(original, path); err != nil {
					t.Fatal(err)
				}
			case "bytes":
				integrationTestWrite(t, filepath.Join(path, "Contents/Resources/changed"), []byte("substituted runtime"))
			case "attempt":
				attempt.Attempt = strings.Repeat("c", 32)
			case "caller":
				attempt.Caller.Birth = "Substituted original caller birth"
			}
			before := installTreeWitness(t, i.home)
			if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err == nil {
				t.Fatal("substituted retained custody accepted")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("substitution refusal changed retained evidence")
			}
		})
	}
}

func TestRuntimeGenerationIdenticalBytesWithDifferentInodesRemainIndependent(t *testing.T) {
	m, attempt, first := retainedRuntimeFixture(t)
	i := m.installer
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	if err := installCopyTree(i.runtimeGenerationPath(first), i.managedPreviousPath()); err != nil {
		t.Fatal(err)
	}
	second, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || first.Inode == second.Inode || i.runtimeGenerationPath(first) == i.runtimeGenerationPath(second) {
		t.Fatal("fixture did not establish equal bytes with distinct inode custody")
	}
	candidate, err := managedCaptureBundle(i.app)
	if err != nil {
		t.Fatal(err)
	}
	receipt := managedPublicationReceipt{SchemaVersion: 1, App: i.app, Pending: candidate.Build, Candidate: candidate, Backup: &second}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := installAtomic(i.managedReceiptPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	attempt.ID++
	attempt.Token = strings.Repeat("d", 32)
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	for _, identity := range []managedBundleIdentity{first, second} {
		if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
			t.Fatal("one generation displaced another", err)
		}
		if _, _, err := i.readRuntimeGeneration(i.runtimeGenerationRecordPath(identity)); err != nil {
			t.Fatal(err)
		}
	}
}

func runtimeGenerationJob(t *testing.T, install *installer, attempt runtimeGenerationAttempt, phase string) *queueStore {
	t.Helper()
	store, err := openQueueStore(install.state, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.close)
	task := Object{"status": Object{"Done": Object{"result": "Success"}}}
	if phase != "done" {
		task["status"] = Object{"Running": Object{}}
	}
	taskJSON, err := queueJSON(task)
	if err != nil {
		t.Fatal(err)
	}
	var workerJSON any
	if attempt.Worker != nil {
		workerJSON, err = queueJSON(attempt.Worker)
		if err != nil {
			t.Fatal(err)
		}
	}
	var outcome any
	if phase == "done" {
		outcome = 0
	}
	if _, err := store.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase,worker,outcome) VALUES(?,?,?,?,?,?,?)", attempt.ID, attempt.Token, taskJSON, attempt.Attempt, phase, workerJSON, outcome); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE queue_settings SET next_id=? WHERE id=1", attempt.ID+1); err != nil {
		t.Fatal(err)
	}
	return store
}

func runtimeGenerationCallerFixture(t *testing.T) (*exec.Cmd, *queueHandoverDaemon) {
	t.Helper()
	command := exec.Command("/bin/sleep", "30")
	// The original CLI may share its process group with its shell and siblings.
	// Retention must observe its exact PID without signalling that group.
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	caller := &queueHandoverDaemon{PID: command.Process.Pid, PGID: syscall.Getpgrp()}
	var err error
	caller.BootUUID, err = BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	members, err := ProcessGroupMembers(caller.PGID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if pid, _ := integer(member["pid"]); pid == caller.PID {
			caller.Birth = text(member["birth"])
		}
	}
	if caller.PID == caller.PGID || caller.Birth == "" {
		t.Fatal("fixture did not establish a caller in a shared process group", caller)
	}
	return command, caller
}

func TestRuntimeGenerationRetainsQueuedCallerWithoutWorker(t *testing.T) {
	m := managedRetainedBackupFixture(t)
	i := m.installer
	attempt := runtimeGenerationAttemptFixture(t)
	attempt.Attempt, attempt.Worker = "", nil
	identity, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal("queued caller did not retain its old signed executable bundle", err)
	}
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	record, _, err := i.readRuntimeGeneration(i.runtimeGenerationRecordPath(identity))
	if err != nil || len(record.Attempts) != 1 || !slicesContainRuntimeAttempt(record.Attempts, attempt) {
		t.Fatal("queued caller custody did not survive publication cleanup", record, err)
	}
	if err := managedRequireBundle(record.Path, identity); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeGenerationCleanupWaitsForOriginalCallerAfterTerminalOutcome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	for _, variant := range []string{"worker", "queued caller", "outcome without worker"} {
		t.Run(variant, func(t *testing.T) {
			m := managedRetainedBackupFixture(t)
			i := m.installer
			attempt := runtimeGenerationAttemptFixture(t)
			command, caller := runtimeGenerationCallerFixture(t)
			attempt.Caller = caller
			if variant != "worker" {
				attempt.Worker = nil
			}
			if variant == "queued caller" {
				attempt.Attempt = ""
			}
			identity, err := managedCaptureBundle(i.managedPreviousPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
				t.Fatal(err)
			}
			if err := i.managedCleanupBackup(); err != nil {
				t.Fatal(err)
			}
			terminal := attempt
			if variant == "queued caller" {
				// A queued job may start on the new runtime after the handover.
				terminal.Attempt = strings.Repeat("c", 32)
				terminal.Worker = runtimeGenerationAttemptFixture(t).Worker
			}
			runtimeGenerationJob(t, i, terminal, "done")
			if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
				t.Fatal("terminal outcome deleted the original caller's code", err)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = command.Wait()
			if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(i.runtimeGenerationPath(identity)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("finished generation survived after its exact caller exited", err)
			}
			if _, err := os.Lstat(i.runtimeGenerationRecordPath(identity)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("reclaimed generation left its receipt", err)
			}
		})
	}
}

func TestRuntimeGenerationCleanupPreservesUncertainCallerBirth(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	m := managedRetainedBackupFixture(t)
	i := m.installer
	attempt := runtimeGenerationAttemptFixture(t)
	_, attempt.Caller = runtimeGenerationCallerFixture(t)
	attempt.Caller.Birth = "Previously recorded original caller birth"
	attempt.Attempt, attempt.Worker = "", nil
	identity, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	runtimeGenerationJob(t, i, attempt, "done")
	if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
		t.Fatal("optional reclamation blocked upgrade on uncertain caller birth", err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
		t.Fatal("uncertain caller birth allowed executable deletion", err)
	}
}

func TestRuntimeGenerationCleanupRequiresOriginalOutcomeAttemptWithoutWorker(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	m := managedRetainedBackupFixture(t)
	i := m.installer
	attempt := runtimeGenerationAttemptFixture(t)
	attempt.Worker = nil
	identity, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	changed := attempt
	changed.Attempt = strings.Repeat("c", 32)
	runtimeGenerationJob(t, i, changed, "done")
	if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
		t.Fatal("changed outcome attempt authorized executable deletion", err)
	}
}

func TestRuntimeGenerationCleanupRequiresTerminalAttemptAndDeadWorker(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	for _, variant := range []string{"active", "terminal", "changed attempt", "pending publication", "missing history"} {
		t.Run(variant, func(t *testing.T) {
			m, attempt, identity := retainedRuntimeFixture(t)
			i := m.installer
			phase := "done"
			if variant == "active" {
				phase = "running"
			}
			store := runtimeGenerationJob(t, i, attempt, phase)
			if variant != "pending publication" {
				if err := i.managedCleanupBackup(); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "changed attempt" {
				if _, err := store.db.Exec("UPDATE queue_jobs SET attempt=?", strings.Repeat("c", 32)); err != nil {
					t.Fatal(err)
				}
			} else if variant == "missing history" {
				if _, err := store.db.Exec("DELETE FROM queue_jobs"); err != nil {
					t.Fatal(err)
				}
			}
			if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err := os.Lstat(i.runtimeGenerationPath(identity))
			if (variant == "terminal") != errors.Is(err, os.ErrNotExist) {
				t.Fatalf("incorrect cleanup for %s: %v", variant, err)
			}
			if variant != "terminal" {
				if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRuntimeGenerationCleanupRejectsMalformedTerminalOutcome(t *testing.T) {
	for _, variant := range []string{"missing result", "invalid result", "missing outcome", "mismatched outcome"} {
		t.Run(variant, func(t *testing.T) {
			m, attempt, identity := retainedRuntimeFixture(t)
			i := m.installer
			if err := i.managedCleanupBackup(); err != nil {
				t.Fatal(err)
			}
			store := runtimeGenerationJob(t, i, attempt, "done")
			var err error
			switch variant {
			case "missing result", "invalid result":
				details := Object{}
				if variant == "invalid result" {
					details["result"] = Object{"Failed": 0}
				}
				task, encodeErr := queueJSON(Object{"status": Object{"Done": details}})
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				_, err = store.db.Exec("UPDATE queue_jobs SET task=? WHERE id=?", task, attempt.ID)
			case "missing outcome":
				_, err = store.db.Exec("UPDATE queue_jobs SET outcome=NULL WHERE id=?", attempt.ID)
			case "mismatched outcome":
				_, err = store.db.Exec("UPDATE queue_jobs SET outcome=1 WHERE id=?", attempt.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := installTreeWitness(t, i.home)
			if err := i.cleanupManagedRuntimes(context.Background()); err == nil {
				t.Fatal("malformed terminal outcome authorized runtime cleanup")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("malformed terminal outcome changed retained evidence")
			}
			if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
				t.Fatal("malformed terminal outcome lost the signed runtime", err)
			}
		})
	}
}

func TestRuntimeGenerationCleanupPreservesStillLiveOwnedWorker(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	m := managedRetainedBackupFixture(t)
	i := m.installer
	attempt := runtimeGenerationAttemptFixture(t)
	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	pid := command.Process.Pid
	members, err := ProcessGroupMembers(pid)
	if err != nil {
		t.Fatal(err)
	}
	attempt.Worker = Object{"pid": pid, "pgid": pid, "boot_uuid": attempt.Worker["boot_uuid"], "members": members}
	identity, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	runtimeGenerationJob(t, i, attempt, "done")
	if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
		t.Fatal("terminal state alone deleted a live runtime", err)
	}
}

func TestRuntimeGenerationCleanupRetainsAmbiguousProcessBirthWithoutBlockingUpgrade(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("runtime process birth checks require macOS")
	}
	m := managedRetainedBackupFixture(t)
	i := m.installer
	attempt := runtimeGenerationAttemptFixture(t)
	command := exec.Command("/bin/sleep", "30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	pid := command.Process.Pid
	attempt.Worker = Object{"pid": pid, "pgid": pid, "boot_uuid": attempt.Worker["boot_uuid"], "members": []Object{{"pid": pid, "birth": "Previously recorded worker birth"}}}
	identity, err := managedCaptureBundle(i.managedPreviousPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := i.retainManagedRuntime(context.Background(), []runtimeGenerationAttempt{attempt}); err != nil {
		t.Fatal(err)
	}
	if err := i.managedCleanupBackup(); err != nil {
		t.Fatal(err)
	}
	runtimeGenerationJob(t, i, attempt, "done")
	if err := i.cleanupManagedRuntimes(context.Background()); err != nil {
		t.Fatal("optional code cleanup blocked upgrade on ambiguous process birth", err)
	}
	if err := managedRequireBundle(i.runtimeGenerationPath(identity), identity); err != nil {
		t.Fatal("ambiguous process birth allowed code deletion", err)
	}
}

func TestRuntimeGenerationCleanupRecoversDeletionCrashAndRejectsUnknownRecords(t *testing.T) {
	for _, variant := range []string{"deleted retiring runtime", "unknown receipt", "changed path", "changed bundle"} {
		t.Run(variant, func(t *testing.T) {
			m, _, identity := retainedRuntimeFixture(t)
			i := m.installer
			if err := i.managedCleanupBackup(); err != nil {
				t.Fatal(err)
			}
			record, raw, err := i.readRuntimeGeneration(i.runtimeGenerationRecordPath(identity))
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "deleted retiring runtime":
				record.Phase = "retiring"
				if err := i.replaceRuntimeGeneration(record, raw); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(record.Path); err != nil {
					t.Fatal(err)
				}
			case "unknown receipt":
				integrationTestWrite(t, filepath.Join(i.runtimeGenerationRecords(), "unknown.json"), []byte("{}"))
			case "changed path":
				record.Path = filepath.Join(i.home, "unrelated.app")
				if err := i.replaceRuntimeGeneration(record, raw); err != nil {
					t.Fatal(err)
				}
			case "changed bundle":
				integrationTestWrite(t, filepath.Join(record.Path, "Contents/Resources/unrelated"), []byte("changed"))
			}
			before := installTreeWitness(t, i.home)
			err = i.cleanupManagedRuntimes(context.Background())
			if variant == "deleted retiring runtime" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(i.runtimeGenerationRecordPath(identity)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("cleanup crash receipt survived", err)
				}
			} else if err == nil || !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("unknown custody was not preserved", err)
			}
		})
	}
}

func TestRuntimeGenerationRenameCannotReplaceUnexpectedDirectory(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"source", "target"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := runtimeGenerationRenameNoReplace(int(parent.Fd()), "source", int(parent.Fd()), "target"); !errors.Is(err, unix.EEXIST) {
		t.Fatal("exclusive generation move could replace an unknown target", err)
	}
	for _, name := range []string{"source", "target"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal("exclusive move damaged original entries", err)
		}
	}
}

func TestRuntimeGenerationReaderRejectsAmbiguousAttemptAndBirthFields(t *testing.T) {
	for _, variant := range []string{"attempt", "worker", "member", "caller", "missing caller", "null caller", "unknown caller field"} {
		t.Run(variant, func(t *testing.T) {
			m, _, identity := retainedRuntimeFixture(t)
			i := m.installer
			filename := i.runtimeGenerationRecordPath(identity)
			raw := string(integrationTestRead(t, filename))
			switch variant {
			case "attempt":
				raw = strings.Replace(raw, `"attempts":[{`, `"attempts":[{"id":7,`, 1)
			case "worker":
				raw = strings.Replace(raw, `"worker":{`, `"worker":{"pid":2000000111,`, 1)
			case "member":
				raw = strings.Replace(raw, `"members":[{`, `"members":[{"pid":2000000111,`, 1)
			case "caller":
				raw = strings.Replace(raw, `"caller":{`, `"caller":{"pid":2000000112,`, 1)
			case "missing caller":
				var record map[string]json.RawMessage
				if err := json.Unmarshal([]byte(raw), &record); err != nil {
					t.Fatal(err)
				}
				var attempts []map[string]json.RawMessage
				if err := json.Unmarshal(record["attempts"], &attempts); err != nil {
					t.Fatal(err)
				}
				delete(attempts[0], "caller")
				encoded, err := json.Marshal(attempts)
				if err != nil {
					t.Fatal(err)
				}
				record["attempts"] = encoded
				encoded, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				raw = string(encoded)
			case "null caller":
				caller, err := json.Marshal(runtimeGenerationAttemptFixture(t).Caller)
				if err != nil {
					t.Fatal(err)
				}
				raw = strings.Replace(raw, `"caller":`+string(caller), `"caller":null`, 1)
			case "unknown caller field":
				raw = strings.Replace(raw, `"caller":{`, `"caller":{"unknown":true,`, 1)
			}
			integrationTestWrite(t, filename, []byte(raw))
			before := installTreeWitness(t, i.home)
			if _, _, err := i.readRuntimeGeneration(filename); err == nil {
				t.Fatal("ambiguous process custody was accepted")
			}
			if err := i.cleanupManagedRuntimes(context.Background()); err == nil {
				t.Fatal("ambiguous process custody allowed cleanup")
			}
			if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
				t.Fatal("ambiguous record rejection mutated retained evidence")
			}
		})
	}
}

func TestRuntimeGenerationCancelledOperationsPreserveCustody(t *testing.T) {
	m, attempt, _ := retainedRuntimeFixture(t)
	i := m.installer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := installTreeWitness(t, i.home)
	if err := i.retainManagedRuntime(ctx, []runtimeGenerationAttempt{attempt}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled transfer was accepted", err)
	}
	if err := i.cleanupManagedRuntimes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled cleanup was accepted", err)
	}
	if !reflect.DeepEqual(before, installTreeWitness(t, i.home)) {
		t.Fatal("cancelled runtime custody changed")
	}
}
