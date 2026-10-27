package backend

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func handoverTarget(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func prepareQueueHandover(t *testing.T, f *daemonFixture, target string) string {
	t.Helper()
	value := token()
	if _, err := f.gate.QueueControl("prepare-handover", Object{"token": value, "targetSHA256": target}); err != nil {
		t.Fatal("prepare handover", err)
	}
	return value
}

func commitQueueHandover(t *testing.T, f *daemonFixture, value string) {
	t.Helper()
	if _, err := f.gate.QueueControl("commit-handover", Object{"token": value}); err != nil {
		t.Fatal("commit handover", err)
	}
	ended := make(chan error, 1)
	go func() { ended <- f.daemon.Wait() }()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal("committed daemon did not exit cleanly", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("committed daemon did not release leadership")
	}
}

func handoverFileClient(t *testing.T, f *daemonFixture, output string, args ...string) *queueClient {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	client := &queueClient{done: make(chan error, 1)}
	client.command = exec.Command(executable, append([]string{"run", "--"}, args...)...)
	client.command.Env = f.environment
	client.command.Dir = f.root
	client.command.Stdout = file
	client.command.Stderr = &client.errors
	client.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := client.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { client.done <- client.command.Wait(); close(client.done) }()
	f.clients = append(f.clients, client)
	return client
}

func TestRealDaemonLiveHandoverRetainsWorkerCallerAndQueuedJobs(t *testing.T) {
	f := daemonAcceptance(t)
	started, one, two, release := filepath.Join(f.root, "started"), filepath.Join(f.root, "one"), filepath.Join(f.root, "two"), filepath.Join(f.root, "release")
	attempts, output := filepath.Join(f.root, "attempts"), filepath.Join(f.root, "caller-output")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	client := handoverFileClient(t, f, output, "/bin/sh", "-c", `printf attempt >> "$1"; touch "$2"; while [ ! -f "$3" ]; do sleep .04; done; printf 'outage-one\n'; while [ ! -f "$4" ]; do sleep .04; done; printf 'outage-two\n'; while [ ! -f "$5" ]; do sleep .04; done; printf 'finished\n'; exit 23`, "fixture", attempts, started, one, two, release)
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
	queued := f.launch("/bin/sh", "-c", `printf new-runtime > "$1"`, "fixture", filepath.Join(f.root, "queued-ran"))
	f.waitJobs(2)
	value := prepareQueueHandover(t, f, handoverTarget(t))
	commitQueueHandover(t, f, value)
	retained, err := readQueueHandoverReceipt(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	retained.ExpiresAt = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := writeQueueHandoverReceipt(f.gate, retained); err != nil {
		t.Fatal(err)
	}
	// Released custody survives arbitrary operating-system approval delays;
	// only a still-prepared handover has an expiring dispatch-freeze lease.
	// Two command/output barriers force the original caller through at least
	// one failed socket read while the old worker continues, without a sleep.
	for _, step := range []struct{ file, text string }{{one, "outage-one"}, {two, "outage-two"}} {
		if err := os.WriteFile(step.file, nil, 0600); err != nil {
			t.Fatal(err)
		}
		fixtureUntil(t, 8*time.Second, func() bool {
			raw, err := os.ReadFile(output)
			return err == nil && strings.Contains(string(raw), step.text)
		})
	}
	if _, err := os.Stat(filepath.Join(f.root, "queued-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("queued work ran without a daemon", err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		store, err := queueReadHandoverStore(f.gate)
		if err != nil {
			return false
		}
		defer store.close()
		job, err := store.job(0)
		return err == nil && job.phase == "outcome" && job.outcome.Valid && job.outcome.Int64 == 23
	})
	queueRestartFixture(t, f)
	client.wait(t, 23)
	queued.wait(t, 0)
	raw, err := os.ReadFile(attempts)
	if err != nil || string(raw) != "attempt" {
		t.Fatal("active command was replayed", string(raw), err)
	}
	caller, err := os.ReadFile(output)
	if err != nil || string(caller) != "outage-one\noutage-two\nfinished\n" {
		t.Fatal("caller output was lost", string(caller), err)
	}
	state, err := f.gate.Snapshot()
	if err != nil || obj(obj(state["groups"])["default"])["status"] != "Running" {
		t.Fatal("clean handover paused dispatch", state, err)
	}
	receipt, err := readQueueHandoverReceipt(f.gate)
	if err != nil || receipt.Phase != "consumed" {
		t.Fatal("handover was not consumed", receipt, err)
	}
	if err := validateConsumedQueueHandover(f.gate, value, handoverTarget(t)); err != nil {
		t.Fatal("newly completed queue work invalidated a live successor", err)
	}
}

func TestRealDaemonHandoverRetainsClaimedWorkerWaitingForExternalSlot(t *testing.T) {
	f := daemonAcceptance(t)
	if err := os.Mkdir(f.gate.Slot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.gate.Slot, "external-owner"), []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	// An external owner remains exclusive even after the new daemon is live.
	t.Cleanup(func() { _ = os.Remove(filepath.Join(f.gate.Slot, "external-owner")); _ = os.Remove(f.gate.Slot) })
	attempts := filepath.Join(f.root, "waiting-command-attempts")
	client := f.launch("/bin/sh", "-c", `printf once >> "$1"; exit 37`, "fixture", attempts)
	var original queueJob
	fixtureUntil(t, 8*time.Second, func() bool {
		store, err := queueReadHandoverStore(f.gate)
		if err != nil {
			return false
		}
		defer store.close()
		job, err := store.job(0)
		if err != nil || job.phase != "running" {
			return false
		}
		original = job
		return true
	})
	value := prepareQueueHandover(t, f, handoverTarget(t))
	commitQueueHandover(t, f, value)
	// Exercise an actual listener outage longer than two 500ms capacity polls.
	// The claimed worker cannot run while the external slot is held, and must
	// keep its same durable attempt rather than recording transport failure125.
	deadline := time.NewTimer(1200 * time.Millisecond)
	defer deadline.Stop()
	select {
	case <-client.done:
		t.Fatal("caller exited during a valid handover")
	case <-deadline.C:
	}
	store, err := queueReadHandoverStore(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.job(0)
	store.close()
	if err != nil || current.phase != "running" || current.attempt != original.attempt || !jsonEqual(current.worker, original.worker) {
		t.Fatal("waiting worker failed during released custody", current, err)
	}
	if _, err := os.Stat(attempts); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worker ran while external slot held", err)
	}
	queueRestartFixture(t, f)
	if _, err := os.Stat(attempts); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successor bypassed external slot", err)
	}
	if err := os.Remove(filepath.Join(f.gate.Slot, "external-owner")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.gate.Slot); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 37)
	raw, err := os.ReadFile(attempts)
	if err != nil || string(raw) != "once" {
		t.Fatal("waiting command did not execute exactly once", string(raw), err)
	}
	store, err = queueReadHandoverStore(f.gate)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	finished, err := store.job(0)
	if err != nil || finished.attempt != original.attempt || !finished.outcome.Valid || finished.outcome.Int64 != 37 {
		t.Fatal("waiting execution lost its original outcome", finished, err)
	}
}

func TestQueueHandoverCapabilitiesDoesNotTouchPrivateState(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	command := exec.Command(executable, "handover-capabilities", "--json")
	command.Env = []string{"HOME=" + filepath.Join(root, "missing-home"), "AGENT_HEAVY_STATE=" + filepath.Join(root, "forbidden-state"), "AGENT_HEAVY_CONFIG=" + filepath.Join(root, "forbidden-config")}
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("capability inspection failed", string(raw), err)
	}
	var value map[string]int
	if err := json.Unmarshal(raw, &value); err != nil || len(value) != 3 || value["schemaVersion"] != 1 || value["protocol"] != 1 || value["queueSchema"] != 2 {
		t.Fatal("unsupported target declaration", string(raw), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("capability inspection created private state", entries, err)
	}
}

func TestRealDaemonHandoverRetainsTerminalCallerAndRejectsCallerTamper(t *testing.T) {
	f := daemonAcceptance(t)
	caller, err := queueHandoverIdentity()
	if err != nil {
		t.Fatal(err)
	}
	value := token()
	path, _ := f.gate.PayloadPath(value)
	if err := WriteJobPayload(path, Object{"argv": []string{"/usr/bin/true"}, "cwd": f.root, "env": Object{}, "token": value, "label": "terminal caller", "client_handover_protocol": 1, "client_handover_caller": caller}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.QueueControl("submit", Object{"token": value}); err != nil {
		t.Fatal(err)
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		store, err := queueReadHandoverStore(f.gate)
		if err != nil {
			return false
		}
		defer store.close()
		job, err := store.job(0)
		return err == nil && job.phase == "done"
	})
	raw, err := f.gate.QueueControl("handover-status", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := decodeQueueHandoverStatus(raw)
	if err != nil || !status.Eligible || len(status.Attempts) != 1 || status.Attempts[0].Phase != "done" || !jsonEqual(status.Attempts[0].Caller, &caller) {
		t.Fatal("terminal caller executable was not retained", status, err)
	}
	handover := prepareQueueHandover(t, f, handoverTarget(t))
	store, err := openQueueStore(f.gate.State, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	job, err := store.job(0)
	if err != nil {
		t.Fatal(err)
	}
	altered := caller
	altered.Birth = "changed original caller birth"
	job.task["handoverCaller"] = altered
	if err := store.saveTask(job); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.QueueControl("commit-handover", Object{"token": handover}); err == nil {
		t.Fatal("changed caller custody authorized handover")
	}
	if _, err := f.gate.QueueControl("cancel-handover", Object{"token": handover}); err != nil {
		t.Fatal(err)
	}
	// A historical terminal PID with another birth, or a caller from a prior
	// boot, cannot keep all future upgrades waiting on old process history.
	for _, historical := range []queueHandoverDaemon{altered, {PID: caller.PID, PGID: caller.PGID, Birth: caller.Birth, BootUUID: "00000000-0000-0000-0000-000000000001"}} {
		job.task["handoverCaller"] = historical
		if err := store.saveTask(job); err != nil {
			t.Fatal(err)
		}
		raw, err := f.gate.QueueControl("handover-status", nil)
		if err != nil {
			t.Fatal(err)
		}
		status, err := decodeQueueHandoverStatus(raw)
		if err != nil || !status.Eligible || len(status.Attempts) != 0 {
			t.Fatal("terminal history permanently blocked upgrades", status, err)
		}
	}
}

func TestRealDaemonHandoverProtocolRequiresVerifiedCaller(t *testing.T) {
	f := daemonAcceptance(t)
	identity, err := queueHandoverIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"missing", "dead", "birth", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			caller, err := queueHandoverObject(identity)
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "missing":
				caller = nil
			case "dead":
				caller["pid"] = 2147000000
			case "birth":
				caller["birth"] = "forged"
			case "unknown":
				caller["additional"] = true
			}
			value := token()
			path, _ := f.gate.PayloadPath(value)
			if err := WriteJobPayload(path, Object{"argv": []string{"/usr/bin/true"}, "cwd": f.root, "env": Object{}, "token": value, "label": "invalid caller", "client_handover_protocol": 1, "client_handover_caller": caller}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.gate.QueueControl("submit", Object{"token": value}); err == nil {
				t.Fatal("unverified caller was accepted")
			}
		})
	}
	snapshot, err := f.gate.Snapshot()
	if err != nil || len(obj(snapshot["tasks"])) != 0 {
		t.Fatal("failed caller validation submitted a command", snapshot, err)
	}
}

func TestQueueHandoverCallerMovedProcessGroupFailsClosed(t *testing.T) {
	identity := queueHandoverDaemon{PID: 23, PGID: 24, Birth: "original process birth", BootUUID: "00000000-0000-0000-0000-000000000001"}
	snapshot := queueHandoverProcesses{Boot: identity.BootUUID, Processes: map[int]queueHandoverProcess{identity.PID: {PGID: 25, Birth: identity.Birth, State: "S"}}}
	if alive, err := snapshot.alive(identity); err == nil || alive {
		t.Fatal("moved caller was classified dead", alive, err)
	}
}

func TestQueueHandoverWorkerRetriesSchedulingSnapshotAfterCapacity(t *testing.T) {
	for _, variant := range []string{"valid-handover", "lease-error", "missing-custody"} {
		t.Run(variant, func(t *testing.T) {
			g, store, receipt := releasedQueueFixture(t)
			identity, err := queueHandoverIdentity()
			if err != nil {
				t.Fatal(err)
			}
			job := edgeInsertJob(t, store, "running")
			job.task["handoverProtocol"] = 1
			job.task["handoverCaller"] = identity
			members, err := ProcessGroupMembers(identity.PGID)
			if err != nil {
				t.Fatal(err)
			}
			job.worker = Object{"pid": identity.PID, "pgid": identity.PGID, "boot_uuid": identity.BootUUID, "members": members}
			task, err := queueJSON(job.task)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := queueJSON(job.worker)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec("UPDATE queue_jobs SET task=?,worker=? WHERE id=?", task, worker, job.id); err != nil {
				t.Fatal(err)
			}
			receipt.Attempts = []queueHandoverAttempt{{ID: job.id, Token: job.token, Attempt: job.attempt, Phase: "running", Worker: job.worker, Caller: &identity}}
			if err := writeQueueHandoverReceipt(g, receipt); err != nil {
				t.Fatal(err)
			}
			if variant == "missing-custody" {
				if err := os.Remove(queueHandoverPath(g)); err != nil {
					t.Fatal(err)
				}
			}
			path, _ := g.PayloadPath(job.token)
			attempts := filepath.Join(t.TempDir(), "command-attempts")
			if err := WriteJobPayload(path, Object{"token": job.token, "task_id": job.id, "argv": []string{"/bin/sh", "-c", `printf once >> "$1"; exit 41`, "fixture", attempts}, "cwd": g.State, "env": Object{}, "resource_managed": true}); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, g.ReceiptPath(job.id), Object{"task_id": job.id, "token": job.token})
			snapshots := 0
			leaseFailure := errors.New("refuse uncertain validation lease owner")
			g.queueControl = func(operation string, _ Object) (Object, error) {
				if operation != "snapshot" {
					t.Fatalf("unexpected operation %s", operation)
				}
				snapshots++
				if snapshots == 2 {
					if _, err := os.Stat(attempts); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("command executed before scheduling permission", err)
					}
					if variant == "lease-error" {
						return nil, leaseFailure
					}
					return nil, errQueueTransportUnavailable
				}
				return testSnapshot(Object{"7": job.task}), nil
			}
			code, err := executeWithAttempt(g, job.token, job.attempt)
			if variant != "valid-handover" {
				if code != 125 || err == nil || snapshots != 2 {
					t.Fatal("unsafe admission error was retried", code, err, snapshots)
				}
				if variant == "lease-error" && !errors.Is(err, leaseFailure) {
					t.Fatal("lease ownership error was hidden", err)
				}
				if _, err := os.Stat(attempts); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("rejected command executed", err)
				}
				return
			}
			if err != nil || code != 41 || snapshots != 4 {
				t.Fatal("valid scheduling outage lost original execution", code, err, snapshots)
			}
			raw, err := os.ReadFile(attempts)
			if err != nil || string(raw) != "once" {
				t.Fatal("command did not execute exactly once", string(raw), err)
			}
		})
	}
}

func releasedQueueFixture(t *testing.T) (*Gate, *queueStore, *queueHandoverReceipt) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("handover process identity uses macOS ownership APIs")
	}
	g, store := edgeQueueStore(t)
	boot, err := BootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := queueHandoverDatabase(g)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &queueHandoverReceipt{SchemaVersion: 1, Protocol: 1, QueueSchema: 2, Token: token(), TargetSHA256: handoverTarget(t), Phase: "released", ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), DatabaseDevice: device, DatabaseInode: inode, Daemon: queueHandoverDaemon{PID: 2147000000, PGID: 2147000000, BootUUID: boot, Birth: "absent private fixture process"}, Attempts: []queueHandoverAttempt{}, QueueStatus: "Running", ParallelLimit: 1, NextID: 0}
	if err := writeQueueHandoverReceipt(g, receipt); err != nil {
		t.Fatal(err)
	}
	return g, store, receipt
}

func TestQueueReleasedHandoverRejectsChangedCustodyWithoutMigration(t *testing.T) {
	for _, variant := range []string{"token", "target", "boot", "schema", "database", "live-issuer", "queue-state"} {
		t.Run(variant, func(t *testing.T) {
			g, store, receipt := releasedQueueFixture(t)
			value, target := receipt.Token, receipt.TargetSHA256
			switch variant {
			case "token":
				value = token()
			case "target":
				target = strings.Repeat("0", 64)
			case "boot":
				receipt.Daemon.BootUUID = "00000000-0000-0000-0000-000000000001"
				if err := writeQueueHandoverReceipt(g, receipt); err != nil {
					t.Fatal(err)
				}
			case "schema":
				if _, err := store.db.Exec("PRAGMA user_version=1"); err != nil {
					t.Fatal(err)
				}
			case "database":
				receipt.DatabaseInode++
				if err := writeQueueHandoverReceipt(g, receipt); err != nil {
					t.Fatal(err)
				}
			case "live-issuer":
				identity, err := queueHandoverIdentity()
				if err != nil {
					t.Fatal(err)
				}
				receipt.Daemon = identity
				if err := writeQueueHandoverReceipt(g, receipt); err != nil {
					t.Fatal(err)
				}
			case "queue-state":
				if _, err := store.db.Exec("UPDATE queue_settings SET status='Paused'"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := validateReleasedQueueHandover(g, value, target); err == nil {
				t.Fatal("changed handover custody was accepted")
			}
			if variant == "schema" {
				var version int
				if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
					t.Fatal("read-only handover validation migrated the database", version, err)
				}
			}
		})
	}
}

func TestQueueReleasedHandoverRefusesUnsafeReceiptNamespace(t *testing.T) {
	for _, variant := range []string{"symlink", "hardlink", "public", "symlink-parent", "unknown-field"} {
		t.Run(variant, func(t *testing.T) {
			g, _, _ := releasedQueueFixture(t)
			path := queueHandoverPath(g)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink-parent":
				directory := filepath.Dir(path)
				if err := os.Rename(directory, directory+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(directory+".original", directory); err != nil {
					t.Fatal(err)
				}
			case "unknown-field":
				var value Object
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				value["overrideOwnership"] = true
				if err := WritePrivate(path, value); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readQueueHandoverReceipt(g); err == nil {
				t.Fatal("unsafe receipt namespace accepted")
			}
		})
	}
}

func TestQueuePreparedHandoverDoesNotOverwriteChangedReceipt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("handover process identity uses macOS ownership APIs")
	}
	g, store := edgeQueueStore(t)
	d := &queueDaemon{g: g, store: store, children: map[int]*exec.Cmd{}, errors: map[int]string{}, cleaning: map[int]bool{}}
	value := token()
	if _, err := d.handoverControl("prepare-handover", Object{"token": value, "targetSHA256": handoverTarget(t)}); err != nil {
		t.Fatal(err)
	}
	receipt, err := readQueueHandoverReceipt(g)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Token = token()
	if err := writeQueueHandoverReceipt(g, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := d.handoverControl("commit-handover", Object{"token": value}); err == nil {
		t.Fatal("commit overwrote substituted receipt")
	}
	current, err := readQueueHandoverReceipt(g)
	if err != nil || current.Token != receipt.Token || current.Phase != "prepared" {
		t.Fatal("substituted evidence was changed", current, err)
	}
}

func TestRealDaemonHandoverFreezesBeforeAcceptanceAndPreservesSettings(t *testing.T) {
	f := daemonAcceptance(t)
	if _, err := f.gate.QueueControl("limit", Object{"limit": 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.QueueControl("pause", nil); err != nil {
		t.Fatal(err)
	}
	value := prepareQueueHandover(t, f, handoverTarget(t))
	for _, operation := range []string{"limit", "resume", "pause", "submit", "cancel", "start"} {
		payload := Object{"id": 0, "limit": 1, "token": strings.Repeat("a", 32)}
		if _, err := f.gate.QueueControl(operation, payload); !errors.Is(err, errQueueHandoverBusy) {
			t.Fatalf("%s mutation was accepted during handover: %v", operation, err)
		}
	}
	if _, err := f.gate.QueueControl("commit-handover", Object{"token": token()}); err == nil {
		t.Fatal("stale handover token stopped daemon")
	}
	commitQueueHandover(t, f, value)
	queueRestartFixture(t, f)
	state, err := f.gate.Snapshot()
	group := obj(obj(state["groups"])["default"])
	limit, _ := integer(group["parallel_tasks"])
	if err != nil || group["status"] != "Paused" || limit != 3 {
		t.Fatal("handover changed deliberate queue settings", state, err)
	}
}

func TestRealDaemonHandoverRefusesLegacyQueuedCaller(t *testing.T) {
	f := daemonAcceptance(t)
	release := filepath.Join(f.root, "release")
	started := filepath.Join(f.root, "started")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	client := f.launch("/bin/sh", "-c", `touch "$1"; while [ ! -f "$2" ]; do sleep .04; done`, "fixture", started, release)
	fixtureUntil(t, 8*time.Second, func() bool { _, err := os.Stat(started); return err == nil })
	legacy := token()
	path, _ := f.gate.PayloadPath(legacy)
	if err := WriteJobPayload(path, Object{"argv": []string{"/usr/bin/true"}, "cwd": f.root, "env": Object{}, "token": legacy, "label": "legacy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.QueueControl("submit", Object{"token": legacy}); err != nil {
		t.Fatal(err)
	}
	status, err := f.gate.QueueControl("handover-status", nil)
	if err != nil || status["eligible"] != false || !strings.Contains(text(status["reason"]), "legacy caller") {
		t.Fatal("legacy caller silently qualified", status, err)
	}
	if _, err := f.gate.QueueControl("prepare-handover", Object{"token": token(), "targetSHA256": handoverTarget(t)}); err == nil {
		t.Fatal("legacy caller authorized live handover")
	}
	if _, err := os.Stat(queueHandoverPath(f.gate)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused handover mutated custody", err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	client.wait(t, 0)
}

func TestRealDaemonHandoverRejectsTargetAndCannotReplayAfterCrash(t *testing.T) {
	for _, wrongTarget := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongTarget), func(t *testing.T) {
			f := daemonAcceptance(t)
			target := handoverTarget(t)
			if wrongTarget {
				target = strings.Repeat("0", 64)
			}
			value := prepareQueueHandover(t, f, target)
			commitQueueHandover(t, f, value)
			if wrongTarget {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(executable, "daemon")
				cmd.Env = f.environment
				raw, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(raw), "does not match queue handover") {
					t.Fatal("unmatched target adopted active state", string(raw), err)
				}
				if err := os.Remove(queueHandoverPath(f.gate)); err != nil {
					t.Fatal(err)
				} // Private fixture cleanup after the refusal.
				queueRestartFixture(t, f)
			} else {
				queueRestartFixture(t, f)
				if err := f.daemon.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := f.daemon.Wait(); err == nil {
					t.Fatal("fixture daemon did not crash")
				}
				if err := validateConsumedQueueHandover(f.gate, value, target); err == nil {
					t.Fatal("dead successor authorized consumed installer retry")
				}
				queueRestartFixture(t, f)
			}
			state, err := f.gate.Snapshot()
			if err != nil || obj(obj(state["groups"])["default"])["status"] != "Paused" {
				t.Fatal("unsafe or consumed custody authorized crash auto-resume", state, err)
			}
		})
	}
}

func TestQueuePreparedHandoverLeaseExpiresWithoutChangingQueueState(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("handover process identity uses macOS ownership APIs")
	}
	g, store := edgeQueueStore(t)
	d := &queueDaemon{g: g, store: store, children: map[int]*exec.Cmd{}, errors: map[int]string{}, cleaning: map[int]bool{}}
	value := token()
	if _, err := d.handoverControl("prepare-handover", Object{"token": value, "targetSHA256": handoverTarget(t)}); err != nil {
		t.Fatal(err)
	}
	d.handover.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := writeQueueHandoverReceipt(g, d.handover); err != nil {
		t.Fatal(err)
	}
	if err := d.tick(); err != nil || d.handover != nil {
		t.Fatal("abandoned preparation did not unfreeze", err)
	}
	state, err := store.settings()
	if err != nil || state["status"] != "Running" {
		t.Fatal("lease expiry paused queue", state, err)
	}
	if _, err := d.handoverControl("cancel-handover", Object{"token": value}); err != nil {
		t.Fatal("expired cancellation was not idempotent", err)
	}
}

func TestRealDaemonAdvertisesSafeLiveHandover(t *testing.T) {
	f := daemonAcceptance(t)
	status, err := f.gate.QueueControl("handover-status", nil)
	if err != nil {
		t.Fatalf("daemon has no live-upgrade protocol: %v", err)
	}
	if status["protocol"] != 1 && fmt.Sprint(status["protocol"]) != "1" {
		t.Fatal("unsupported handover protocol", status)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	prepared, err := f.gate.QueueControl("prepare-handover", Object{"token": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "targetSHA256": digest})
	if err != nil || prepared["state"] != "prepared" {
		t.Fatal("safe handover was not prepared", prepared, err)
	}
	if _, err := f.gate.QueueControl("cancel-handover", Object{"token": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.gate.State, "run/handover.json")); err != nil {
		t.Fatal("handover custody was not retained", err)
	}
}
