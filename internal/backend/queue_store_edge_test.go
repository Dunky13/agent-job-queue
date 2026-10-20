package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func edgeQueueStore(t *testing.T) (*Gate, *queueStore) {
	t.Helper()
	g := testGate(t)
	s, err := openQueueStore(g.State, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return g, s
}

func edgeInsertJob(t *testing.T, s *queueStore, phase string) queueJob {
	t.Helper()
	j := queueJob{id: 7, token: strings.Repeat("a", 32), attempt: strings.Repeat("b", 32), phase: phase,
		task: Object{"token": strings.Repeat("a", 32), "priority": 0, "status": Object{"Running": Object{"start": "2026-10-20T00:00:00Z"}}}}
	data, err := queueJSON(j.task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO queue_jobs(id,token,attempt,phase,task) VALUES(?,?,?,?,?)", j.id, j.token, j.attempt, j.phase, data); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestQueueEdgeStaleFinishCannotOverwriteClaimOrOutcome(t *testing.T) {
	for _, phase := range []string{"running", "outcome"} {
		t.Run(phase, func(t *testing.T) {
			g, s := edgeQueueStore(t)
			stale := edgeInsertJob(t, s, "starting")
			actual := stale
			actual.phase = phase
			if phase == "outcome" {
				actual.task = Object{"status": Object{"Done": Object{"result": "Success", "end": "2026-10-20T00:01:00Z"}}}
			}
			data, err := queueJSON(actual.task)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := decodeObject([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("UPDATE queue_jobs SET phase=?,attempt=?,task=?,outcome=? WHERE id=?", phase, actual.attempt, data, 0, actual.id); err != nil {
				t.Fatal(err)
			}
			d := queueDaemon{g: g, store: s}
			if err = d.finish(stale, 130); !errors.Is(err, errQueueChanged) {
				t.Fatalf("stale finish error=%v; want errQueueChanged", err)
			}
			got, err := s.job(actual.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.phase != phase || got.attempt != actual.attempt || !jsonEqual(got.task, expected) || !got.outcome.Valid || got.outcome.Int64 != 0 {
				t.Fatalf("stale completion overwrote newer durable state: phase=%s attempt=%s task=%v outcome=%v", got.phase, got.attempt, got.task, got.outcome)
			}
		})
	}
}

func TestQueueEdgeRejectUnsafeDatabaseAndSidecars(t *testing.T) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		for _, kind := range []string{"symlink", "hardlink", "public"} {
			t.Run(fmt.Sprintf("%s/%s", suffix, kind), func(t *testing.T) {
				g := testGate(t)
				s, err := openQueueStore(g.State, true)
				if err != nil {
					t.Fatal(err)
				}
				s.close()
				path := QueueDatabasePath(g.State) + suffix
				if suffix == "" {
					if err = os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				target := filepath.Join(g.State, "harmless-target")
				if err = os.WriteFile(target, []byte("sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink":
					err = os.Symlink(target, path)
				case "hardlink":
					err = os.Link(target, path)
				case "public":
					err = os.WriteFile(path, []byte("sentinel"), 0644)
					if err == nil {
						err = os.Chmod(path, 0644)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := openQueueStore(g.State, false)
				if got != nil {
					got.close()
				}
				if err == nil {
					t.Fatal("accepted unsafe database or sidecar")
				}
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "sentinel" {
					t.Fatalf("unsafe target changed: %q %v", data, err)
				}
			})
		}
	}
}

func TestQueueEdgeVersionAloneDoesNotEstablishValidSchema(t *testing.T) {
	g, s := edgeQueueStore(t)
	if _, err := s.db.Exec("DROP TABLE queue_jobs"); err != nil {
		t.Fatal(err)
	}
	s.close()
	if err := ValidateQueueState(g.State); err == nil {
		t.Fatal("database with missing lifecycle table reported healthy")
	}
}

func TestQueueEdgeHistoricalIDOverflowFailsClosed(t *testing.T) {
	for _, directory := range []string{"receipts", "cancelled", "resource-runs"} {
		t.Run(directory, func(t *testing.T) {
			g := testGate(t)
			mustWrite(t, filepath.Join(g.State, directory, fmt.Sprintf("%d.json", math.MaxInt)), Object{"task_id": math.MaxInt})
			next, err := queueHistoricalNext(g.State, 0)
			if err == nil {
				t.Fatalf("exhausted numeric ID space accepted; next=%d", next)
			}
		})
	}
}

func TestQueueEdgeLegacyImportRequiresDefaultGroupSettings(t *testing.T) {
	for _, groups := range []any{nil, Object{}, Object{"other": Object{"parallel_tasks": 1, "status": "Running"}}} {
		t.Run(fmt.Sprint(groups), func(t *testing.T) {
			g, s := edgeQueueStore(t)
			mustWrite(t, filepath.Join(g.State, "legacy-queue-import.json"), Object{"tasks": Object{}, "groups": groups})
			if err := s.importLegacy(); err == nil {
				t.Fatal("legacy import invented a running queue instead of requiring retained settings")
			}
			settings, err := s.settings()
			if err != nil || settings["imported"] != 0 {
				t.Fatalf("invalid legacy snapshot was committed: %v %v", settings, err)
			}
		})
	}
}

func TestQueueEdgeLegacyImportPreservesSettingsAndHistory(t *testing.T) {
	g, s := edgeQueueStore(t)
	logPath := filepath.Join(g.State, "original-logs", "41.log")
	snapshot := Object{
		"tasks": Object{"41": Object{
			"token": strings.Repeat("a", 32), "priority": 17, "logPath": logPath,
			"status": Object{"Done": Object{"start": "2026-10-20T10:00:00Z", "end": "2026-10-20T10:01:00Z", "result": Object{"Failed": 7}}},
		}},
		"groups": Object{"default": Object{"parallel_tasks": 4, "status": "Paused"}},
	}
	mustWrite(t, filepath.Join(g.State, "legacy-queue-import.json"), snapshot)
	mustWrite(t, filepath.Join(g.Cancelled, "80.json"), Object{"task_id": 80, "exit_code": 130})
	for range 2 {
		if err := s.importLegacy(); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := s.settings()
	if err != nil || settings["parallel_tasks"] != 4 || settings["status"] != "Paused" || settings["next_id"] != 81 || settings["imported"] != 1 {
		t.Fatalf("legacy settings or retained ID range changed: %v %v", settings, err)
	}
	jobs, err := s.jobs()
	if err != nil || len(jobs) != 1 || jobs[0].id != 41 || jobs[0].phase != "done" {
		t.Fatalf("history was lost or duplicated: %+v %v", jobs, err)
	}
	state, details, err := TaskStatus(jobs[0].task)
	code, codeErr := ResultCode(details["result"])
	priority, validPriority := integer(jobs[0].task["priority"])
	if err != nil || codeErr != nil || state != "Done" || code != 7 || !validPriority || priority != 17 || jobs[0].task["logPath"] != logPath {
		t.Fatalf("completed result, order or original log changed: %v %v %v", jobs[0].task, err, codeErr)
	}
	retained, err := ReadPrivate(filepath.Join(g.State, "legacy-queue-import.json"))
	if err != nil || !jsonEqual(retained, snapshot) {
		t.Fatal("import rewrote its retained legacy source", err)
	}
}

func TestQueueEdgeLegacyImportMaximumIDIsAtomic(t *testing.T) {
	g, s := edgeQueueStore(t)
	mustWrite(t, filepath.Join(g.State, "legacy-queue-import.json"), testSnapshot(Object{fmt.Sprint(math.MaxInt): Object{"priority": 0, "status": Object{"Done": Object{"result": "Success"}}}}))
	if err := s.importLegacy(); err == nil {
		t.Fatal("legacy maximum ID imported despite no representable next ID")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM queue_jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed import committed %d rows", count)
	}
	settings, err := s.settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings["imported"] != 0 {
		t.Fatalf("failed import marked completed: %v", settings)
	}
}

func TestQueueEdgeMalformedJSONFailsClosed(t *testing.T) {
	_, s := edgeQueueStore(t)
	for _, bad := range []string{"null", "{} trailing", "[1]"} {
		if _, err := s.db.Exec("INSERT INTO queue_jobs(id,token,phase,task) VALUES(?,?,?,?)", 7, strings.Repeat("d", 32), "queued", bad); err != nil {
			t.Fatal(err)
		}
		if _, err := s.job(7); err == nil {
			t.Fatalf("accepted malformed task %q", bad)
		}
		if _, err := s.db.Exec("DELETE FROM queue_jobs WHERE id=7"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.job(7); err != sql.ErrNoRows {
		t.Fatalf("missing job error=%v", err)
	}
}

func TestQueueEdgeStaleSaveTaskPreservesNewAttemptAndOutcome(t *testing.T) {
	for _, change := range []string{"attempt", "outcome"} {
		t.Run(change, func(t *testing.T) {
			_, s := edgeQueueStore(t)
			stale := edgeInsertJob(t, s, "running")
			if change == "attempt" {
				if _, err := s.db.Exec("UPDATE queue_jobs SET attempt=? WHERE id=?", strings.Repeat("e", 32), stale.id); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := queueJSON(Object{"status": Object{"Done": Object{"result": "Success"}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec("UPDATE queue_jobs SET phase='outcome',task=?,outcome=0 WHERE id=?", data, stale.id); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.job(stale.id)
			if err != nil {
				t.Fatal(err)
			}
			stale.task["cancelled"] = true
			if err = s.saveTask(stale); !errors.Is(err, errQueueChanged) {
				t.Fatalf("stale update error=%v", err)
			}
			after, err := s.job(stale.id)
			if err != nil {
				t.Fatal(err)
			}
			if after.phase != before.phase || after.attempt != before.attempt || !jsonEqual(before.task, after.task) || after.outcome != before.outcome {
				t.Fatalf("stale task write changed durable state: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestQueueEdgeSubmitRetrySurvivesConsumedPayloadAndPausedQueue(t *testing.T) {
	g, s := edgeQueueStore(t)
	value := strings.Repeat("f", 32)
	path, _ := g.PayloadPath(value)
	mustWrite(t, path, Object{"token": value, "argv": []string{"/usr/bin/true"}, "cwd": g.State, "label": "idempotence", "env": Object{}})
	d := queueDaemon{g: g, store: s}
	first, err := d.submit(Object{"token": value})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE queue_settings SET status='Paused'"); err != nil {
		t.Fatal(err)
	}
	retry, err := d.submit(Object{"token": value})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(first, retry) {
		t.Fatalf("idempotent submission changed identity: %v vs %v", first, retry)
	}
	jobs, err := s.jobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("retry created %d jobs", len(jobs))
	}
}

func TestQueueEdgeLostSubmitResponseRetainsCommittedPayload(t *testing.T) {
	g, s := edgeQueueStore(t)
	d := queueDaemon{g: g, store: s}
	lost := errors.New("simulated response lost after commit")
	g.queueControl = func(op string, p Object) (Object, error) {
		if op == "snapshot" {
			return s.snapshot()
		}
		if op == "submit" {
			if _, err := d.submit(p); err != nil {
				return nil, err
			}
			return nil, lost
		}
		return nil, fmt.Errorf("unexpected operation %s", op)
	}
	_, err := Run(g, []string{"/usr/bin/true"}, "lost acknowledgement")
	if !errors.Is(err, lost) {
		t.Fatalf("submission error=%v", err)
	}
	jobs, err := s.jobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("committed job count=%d", len(jobs))
	}
	path, _ := g.PayloadPath(jobs[0].token)
	if _, err = ReadPrivate(path); err != nil {
		t.Fatalf("committed command payload lost after transport error: %v", err)
	}
}

func TestQueueEdgeLostAcknowledgementReconcilesSameJobAndExecutesOnce(t *testing.T) {
	f := daemonAcceptance(t)
	wire := *f.gate
	wire.queueControl = nil
	submissions := 0
	acknowledgedID := -1
	f.gate.queueControl = func(op string, p Object) (Object, error) {
		result, err := wire.QueueControl(op, p)
		if err != nil || op != "submit" {
			return result, err
		}
		id, ok := integer(result["id"])
		if !ok {
			return nil, errors.New("fixture daemon returned invalid ID")
		}
		submissions++
		if submissions == 1 {
			acknowledgedID = id
			return nil, errors.New("simulated response lost after daemon accepted job")
		}
		if id != acknowledgedID {
			return nil, fmt.Errorf("reconciliation changed ID from %d to %d", acknowledgedID, id)
		}
		return result, nil
	}
	marker := filepath.Join(f.root, "executed-once")
	code, err := Run(f.gate, []string{"/bin/sh", "-c", "printf x >> \"$1\"", "sh", marker}, "lost response")
	if err != nil || code != 0 {
		t.Fatalf("lost acknowledgement not reconciled: code=%d err=%v", code, err)
	}
	if submissions < 2 {
		t.Fatalf("submit was not reconciled, calls=%d", submissions)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "x" {
		t.Fatalf("command execution count wrong: content=%q err=%v", content, err)
	}
	ids := f.taskIDs()
	if len(ids) != 1 || ids[0] != acknowledgedID {
		t.Fatalf("reconciliation duplicated or replaced accepted job: ids=%v expected=%d", ids, acknowledgedID)
	}
}

func TestQueueEdgeDuplicateWorkerCannotClaimAlreadyRunningAttempt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("worker claim verification uses macOS process identities")
	}
	g, s := edgeQueueStore(t)
	j := edgeInsertJob(t, s, "running")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "execute-attempt", strconv.Itoa(j.id), j.token, j.attempt)
	command.Env = envList(g.ControlEnv)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := command.CombinedOutput()
	if err == nil || command.ProcessState == nil || command.ProcessState.ExitCode() != 125 || !strings.Contains(string(output), "Execution attempt is no longer authorized") {
		t.Fatalf("duplicate worker result=%v output=%s", err, output)
	}
	after, err := s.job(j.id)
	if err != nil {
		t.Fatal(err)
	}
	if after.phase != j.phase || after.attempt != j.attempt || !jsonEqual(after.task, j.task) || after.worker != nil || after.outcome.Valid {
		t.Fatalf("duplicate worker mutated existing attempt: %+v", after)
	}
}
