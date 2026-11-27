package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWithdrawEndsOnlyItsCallersWaitingDefaultJobOfAPausedQueue(t *testing.T) {
	d, _ := laneDaemon(t, 1, nil)
	control := func(op string, payload Object) (Object, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.control(op, payload)
	}
	scopedRow(t, d, 1, "queued", Object{"Queued": Object{"enqueued_at": "2026-10-01T10:00:00Z"}})
	scopedTask(t, d, 2, "queued", Object{"lane": "qa", "status": Object{"Queued": Object{"enqueued_at": "2026-10-01T10:00:00Z"}}})
	scopedTask(t, d, 3, "queued", Object{"kind": "lease", "lane": "qa", "status": Object{"Queued": Object{"enqueued_at": "2026-10-01T10:00:00Z"}}})
	scopedRow(t, d, 4, "running", Object{"Running": Object{"start": "2026-10-01T10:00:00Z"}})
	scopedRow(t, d, 5, "queued", Object{"Stashed": Object{"enqueue_at": nil}})
	withdraw := func(id int, value string) error {
		_, err := control("withdraw", Object{"id": id, "token": value})
		return err
	}
	own := func(id int) string { return fmt.Sprintf("%032d", id) }
	if err := withdraw(1, own(1)); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatal("a running queue let a job be withdrawn", err)
	}
	if _, err := control("pause", nil); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		name  string
		id    int
		token string
	}{{"another caller's token", 1, own(2)}, {"a lane job", 2, own(2)}, {"a lease", 3, own(3)}, {"a started job", 4, own(4)}, {"an unknown job", 9, own(9)}} {
		if err := withdraw(refused.id, refused.token); err == nil {
			t.Fatalf("%s was withdrawn", refused.name)
		}
	}
	for _, id := range []int{1, 5} {
		if err := withdraw(id, own(id)); err != nil {
			t.Fatal(id, err)
		}
		j, err := d.store.job(id)
		name, _, statusErr := TaskStatus(j.task)
		if err != nil || statusErr != nil || name != "Done" || !boolean(j.task["withdrawn"]) || !boolean(j.task["cancelled"]) {
			t.Fatalf("job %d after withdraw: %+v %v", id, j, err)
		}
		if err = withdraw(id, own(id)); err == nil {
			t.Fatalf("job %d was withdrawn twice", id)
		}
	}
	for _, id := range []int{2, 3, 4} {
		if j, err := d.store.job(id); err != nil || boolean(j.task["withdrawn"]) || boolean(j.task["cancelled"]) {
			t.Fatalf("refused job %d changed: %+v %v", id, j, err)
		}
	}
}

// A withdraw whose reply is lost still committed: the caller reads its job as
// withdrawn and runs the command instead of reporting the cancellation.
func TestFollowRunsTheCommandWhenTheWithdrawReplyIsLost(t *testing.T) {
	g := testGate(t)
	writeJobLog(t, g, 7, "")
	value := token()
	task := Object{"token": value, "status": Object{"Queued": Object{}}}
	withdraws := 0
	g.queueControl = func(op string, payload Object) (Object, error) {
		switch op {
		case "snapshot":
			return Object{"groups": Object{"default": Object{"status": "Paused", "parallel_tasks": 1}}, "tasks": Object{"7": task}}, nil
		case "withdraw":
			withdraws++
			if payload["id"] != 7 || payload["token"] != value {
				t.Errorf("withdraw payload: %v", payload)
			}
			task = Object{"token": value, "withdrawn": true, "cancelled": true, "status": Object{"Done": Object{"result": Object{"Failed": 130}}}}
			return nil, errQueueTransportUnavailable
		}
		return nil, fmt.Errorf("unexpected %s", op)
	}
	root := t.TempDir()
	read := captureStreams(t)
	code, err := followWithToken(g, 7, make(chan os.Signal), value, []string{"/bin/sh", "-c", `echo ran >> "$1/count"; exit 9`, "fixture", root})
	_, stderr := read()
	if code != 9 || err != nil || withdraws != 1 || readText(t, filepath.Join(root, "count")) != "ran\n" {
		t.Fatalf("code=%d err=%v withdraws=%d stderr=%q", code, err, withdraws, stderr)
	}
	if !strings.Contains(stderr, "job 7 was withdrawn, running this command outside the queue") {
		t.Fatalf("stderr: %q", stderr)
	}
}

// A withdraw that committed while the daemon then stays away is read from the
// queue store: the caller runs the command, or reports an interrupt that came first.
func TestFollowRunsAWithdrawnCommandReadFromTheStore(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint("interrupted=", interrupted), func(t *testing.T) {
			fastOutage(t, 25*time.Millisecond)
			g := testGate(t)
			writeJobLog(t, g, 7, "")
			value := token()
			store, err := openQueueStore(g.State, true)
			if err != nil {
				t.Fatal(err)
			}
			data, err := queueJSON(Object{"token": value, "withdrawn": true, "cancelled": true, "status": Object{"Done": Object{"result": Object{"Failed": 130}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.db.Exec("INSERT INTO queue_jobs(id,token,task,phase,outcome) VALUES(7,?,?,'outcome',130)", value, data); err != nil {
				t.Fatal(err)
			}
			store.close()
			g.queueControl = func(string, Object) (Object, error) { return nil, transportTimeout(t) }
			signals := make(chan os.Signal, 1)
			want := 9
			if interrupted {
				signals <- syscall.SIGTERM
				want = 128 + int(syscall.SIGTERM)
			}
			root := t.TempDir()
			read := captureStreams(t)
			code, err := followWithToken(g, 7, signals, value, []string{"/bin/sh", "-c", `echo ran >> "$1/count"; exit 9`, "fixture", root})
			_, stderr := read()
			_, statErr := os.Stat(filepath.Join(root, "count"))
			if code != want || err != nil || interrupted != os.IsNotExist(statErr) {
				t.Fatalf("code=%d err=%v ran=%v stderr=%q", code, err, statErr == nil, stderr)
			}
		})
	}
}
