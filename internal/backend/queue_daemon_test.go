package backend

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestQueueSocketRejectsUnsafePaths(t *testing.T) {
	for _, kind := range []string{"symlink", "regular", "directory", "public-socket", "public-parent", "hardlink-socket"} {
		t.Run(kind, func(t *testing.T) {
			g := testGate(t)
			root, err := os.MkdirTemp("/tmp", "queue-socket-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			g.State = root
			if err = privateDirectory(filepath.Join(root, "run")); err != nil {
				t.Fatal(err)
			}
			path := queueSocket(g)
			switch kind {
			case "symlink":
				target := filepath.Join(g.State, "untouched")
				if e := os.WriteFile(target, []byte("sentinel"), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink(target, path); e != nil {
					t.Fatal(e)
				}
			case "regular":
				if e := os.WriteFile(path, []byte("sentinel"), 0600); e != nil {
					t.Fatal(e)
				}
			case "directory":
				if e := os.Mkdir(path, 0700); e != nil {
					t.Fatal(e)
				}
			default:
				listenPath := path
				if kind == "hardlink-socket" {
					listenPath = filepath.Join(g.State, "other.sock")
				}
				listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: listenPath, Net: "unix"})
				if e != nil {
					t.Fatal(e)
				}
				defer listener.Close()
				if e = os.Chmod(listenPath, 0600); e != nil {
					t.Fatal(e)
				}
				if kind == "hardlink-socket" {
					e = os.Link(listenPath, path)
				} else if kind == "public-socket" {
					e = os.Chmod(path, 0666)
				} else {
					e = os.Chmod(filepath.Dir(path), 0755)
				}
				if e != nil {
					t.Fatal(e)
				}
			}
			if _, e := g.Snapshot(); e == nil {
				t.Fatal("unsafe queue transport accepted")
			}
		})
	}
}
func queueRestartFixture(t *testing.T, f *daemonFixture) {
	t.Helper()
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(executable, "daemon")
	cmd.Env = f.environment
	cmd.Stdout = f.log
	cmd.Stderr = f.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	f.daemon = cmd
	fixtureUntil(t, 8*time.Second, func() bool { _, e := f.gate.Snapshot(); return e == nil })
}
func TestRealDaemonWorkerOutcomeSurvivesDaemonDeath(t *testing.T) {
	f := daemonAcceptance(t)
	client := f.launch("/bin/sh", "-c", `printf attempt >> "$1"; while [ ! -f "$2" ]; do sleep .05; done; printf survived; exit 23`, "fixture", filepath.Join(f.root, "attempts"), filepath.Join(f.root, "release"))
	fixtureUntil(t, 8*time.Second, func() bool { _, e := os.Stat(filepath.Join(f.root, "attempts")); return e == nil })
	id := f.taskIDs()[0]
	if e := f.daemon.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e := f.daemon.Wait(); e == nil {
		t.Fatal("daemon did not die")
	}
	client.wait(t, 125)
	if e := os.WriteFile(filepath.Join(f.root, "release"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	fixtureUntil(t, 8*time.Second, func() bool {
		s, e := openQueueStore(f.gate.State, false)
		if e != nil {
			return false
		}
		defer s.close()
		j, e := s.job(id)
		return e == nil && j.phase == "outcome" && j.outcome.Valid && j.outcome.Int64 == 23
	})
	queueRestartFixture(t, f)
	fixtureUntil(t, 8*time.Second, func() bool { receipt, e := f.gate.Receipt(id); return e == nil && receipt == nil })
	task, e := f.gate.Task(id)
	if e != nil {
		t.Fatal(e)
	}
	state, details, e := TaskStatus(task)
	if e != nil || state != "Done" {
		t.Fatal(state, e)
	}
	code, e := ResultCode(details["result"])
	if e != nil || code != 23 {
		t.Fatal(code, e)
	}
	body, e := os.ReadFile(filepath.Join(f.root, "attempts"))
	if e != nil || string(body) != "attempt" {
		t.Fatalf("command replayed: %q %v", body, e)
	}
	snapshot, e := f.gate.Snapshot()
	if e != nil || obj(obj(snapshot["groups"])["default"])["status"] != "Paused" {
		t.Fatal("restart must pause", e)
	}
	output, e := os.ReadFile(f.gate.LogPath(id))
	if e != nil || string(output) != "survived" {
		t.Fatalf("final log lost %q %v", output, e)
	}
}
func TestRealDaemonUnclaimedAttemptFailsWithoutReplay(t *testing.T) {
	f := daemonAcceptance(t)
	s, e := openQueueStore(f.gate.State, false)
	if e != nil {
		t.Fatal(e)
	}
	defer s.close()
	value := strings.Repeat("c", 32)
	attempt := strings.Repeat("d", 32)
	id := 32
	task := Object{"token": value, "priority": 0, "created_at": queueNow(), "status": Object{"Running": Object{"start": queueNow()}}, "logPath": filepath.Join(f.gate.State, "logs", strconv.Itoa(id)+".log")}
	data, e := queueJSON(task)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("INSERT INTO queue_jobs(id,token,task,attempt,phase) VALUES(?,?,?,?,'starting')", id, value, data, attempt); e != nil {
		t.Fatal(e)
	}
	path, _ := f.gate.PayloadPath(value)
	mustWrite(t, path, Object{"token": value, "task_id": id, "argv": []string{"/bin/sh", "-c", "echo forbidden"}, "cwd": f.root, "env": Object{}})
	mustWrite(t, f.gate.ReceiptPath(id), Object{"task_id": id, "token": value})
	if e = f.daemon.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = f.daemon.Wait(); e == nil {
		t.Fatal("daemon did not die")
	}
	queueRestartFixture(t, f)
	fixtureUntil(t, 8*time.Second, func() bool { j, e := s.job(id); return e == nil && j.phase == "done" })
	j, e := s.job(id)
	if e != nil || !j.outcome.Valid || j.outcome.Int64 != 125 {
		t.Fatal(j, e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(executable, "execute-attempt", strconv.Itoa(id), value, attempt)
	cmd.Env = f.environment
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	e = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 125 || bytes.Contains(output.Bytes(), []byte("forbidden")) {
		t.Fatalf("old attempt ran: %v %s", e, output.String())
	}
}

func TestQueueCleanupDoesNotBlockSnapshotWhileLeaseLocked(t *testing.T) {
	g := testGate(t)
	s, e := openQueueStore(g.State, true)
	if e != nil {
		t.Fatal(e)
	}
	defer s.close()
	id := 0
	value := strings.Repeat("a", 32)
	task := Object{"token": value, "priority": 0, "status": Object{"Done": Object{"result": "Success", "end": queueNow()}}}
	data, e := queueJSON(task)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("INSERT INTO queue_jobs(id,token,task,phase,outcome) VALUES(?,?,?,'outcome',0)", id, value, data); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, g.ReceiptPath(id), Object{"task_id": id, "token": value})
	d := &queueDaemon{g: g, store: s, children: map[int]*exec.Cmd{}, errors: map[int]string{}, cleaning: map[int]bool{}}
	release, e := LockPrivate(g, "")
	if e != nil {
		t.Fatal(e)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
		d.cleanup.Wait()
	}()
	d.mu.Lock()
	e = d.tick()
	cleaning := d.cleaning[id]
	d.mu.Unlock()
	if e != nil || !cleaning {
		t.Fatalf("cleanup did not start: %v %v", cleaning, e)
	}
	// The scheduler holds this same lease lock across snapshot RPCs. Cleanup must
	// wait outside the dispatch mutex so those RPCs can complete and release it.
	done := make(chan error, 1)
	go func() { d.mu.Lock(); _, e := d.control("snapshot", nil); d.mu.Unlock(); done <- e }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup held the daemon dispatcher while waiting for lease.lock")
	}
	release()
	released = true
	d.cleanup.Wait()
	if receipt, e := g.Receipt(id); e != nil || receipt != nil {
		t.Fatalf("cleanup did not finish %v %v", receipt, e)
	}
}
func TestQueueLegacyUnfencedExecuteRejected(t *testing.T) {
	g := testGate(t)
	value := strings.Repeat("e", 32)
	sideEffect := filepath.Join(g.State, "forbidden")
	path, _ := g.PayloadPath(value)
	mustWrite(t, path, Object{"token": value, "task_id": 0, "argv": []string{"/bin/sh", "-c", `touch "$1"`, "fixture", sideEffect}, "cwd": g.State, "env": Object{}})
	mustWrite(t, g.ReceiptPath(0), Object{"task_id": 0, "token": value})
	code, e := dispatch(g, []string{"execute", value})
	if code != 125 || e == nil {
		t.Fatalf("unfenced legacy execute accepted: %d %v", code, e)
	}
	if _, e = os.Stat(sideEffect); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("unfenced command had side effects %v", e)
	}
}

func TestQueueLaunchRejectsUnsafeLogsWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "public"} {
		t.Run(kind, func(t *testing.T) {
			g := testGate(t)
			s, e := openQueueStore(g.State, true)
			if e != nil {
				t.Fatal(e)
			}
			defer s.close()
			value := strings.Repeat("f", 32)
			path := filepath.Join(g.State, "logs", "0.log")
			target := filepath.Join(g.State, "sentinel")
			if e = os.WriteFile(target, []byte("unchanged"), 0600); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "symlink":
				e = os.Symlink(target, path)
			case "hardlink":
				e = os.Link(target, path)
			case "fifo":
				e = syscall.Mkfifo(path, 0600)
			case "public":
				e = os.WriteFile(path, []byte("unchanged"), 0600)
				if e == nil {
					e = os.Chmod(path, 0644)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			task := Object{"token": value, "logPath": path, "priority": 0, "status": Object{"Queued": Object{}}}
			data, e := queueJSON(task)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(0,?,?,'queued')", value, data); e != nil {
				t.Fatal(e)
			}
			job, e := s.job(0)
			if e != nil {
				t.Fatal(e)
			}
			sideEffect := filepath.Join(g.State, "forbidden")
			payloadPath, _ := g.PayloadPath(value)
			mustWrite(t, payloadPath, Object{"token": value, "task_id": 0, "argv": []string{"/bin/sh", "-c", `touch "$1"`, "fixture", sideEffect}, "cwd": g.State, "env": Object{}})
			mustWrite(t, g.ReceiptPath(0), Object{"task_id": 0, "token": value})
			d := queueDaemon{g: g, store: s, children: map[int]*exec.Cmd{}}
			done := make(chan error, 1)
			go func() { done <- d.launch(job) }()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("unsafe output file blocked daemon")
			}
			current, e := s.job(0)
			if e != nil || current.phase != "outcome" || !current.outcome.Valid || current.outcome.Int64 != 125 {
				t.Fatalf("unsafe log did not fail closed: %v %v", current, e)
			}
			body, e := os.ReadFile(target)
			if e != nil || string(body) != "unchanged" {
				t.Fatalf("unsafe log target modified: %q %v", body, e)
			}
			if kind == "public" {
				body, e = os.ReadFile(path)
				if e != nil || string(body) != "unchanged" {
					t.Fatal("public log modified")
				}
			}
			if _, e = os.Stat(sideEffect); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("command ran with unsafe log")
			}
		})
	}
}
