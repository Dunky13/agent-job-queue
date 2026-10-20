package backend

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"testing"
	"time"
)

func historyBenchmarkStore(b *testing.B, done int) (*Gate, *queueStore) {
	b.Helper()
	root, err := os.MkdirTemp("/tmp", "aq-history-")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { os.RemoveAll(root) })
	b.Setenv("AGENT_HEAVY_STATE", filepath.Join(root, "state"))
	b.Setenv("AGENT_HEAVY_CONFIG", filepath.Join(root, "config.yml"))
	b.Setenv("AGENT_HEAVY_LEGACY_SLOT", filepath.Join(root, "slot"))
	b.Setenv("AGENT_HEAVY_ACTIVE", "")
	g, err := NewGate()
	if err != nil {
		b.Fatal(err)
	}
	if err = g.Prepare(); err != nil {
		b.Fatal(err)
	}
	s, err := openQueueStore(g.State, true)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.close)
	if _, err = s.db.Exec("UPDATE queue_settings SET status='Paused'"); err != nil {
		b.Fatal(err)
	}
	task, err := queueJSON(Object{"label": "history benchmark fixture", "created_at": "2026-10-01T10:00:00Z", "priority": 0, "status": Object{"Done": Object{"start": "2026-10-01T10:00:00Z", "end": "2026-10-01T10:00:01Z", "result": "Success"}}})
	if err != nil {
		b.Fatal(err)
	}
	_, err = s.db.Exec(`WITH RECURSIVE ids(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM ids WHERE i+1<?)
INSERT INTO queue_jobs(id,token,task,phase) SELECT i,'history-'||i,?,'done' FROM ids WHERE i<?`, done, task, done)
	if err != nil {
		b.Fatal(err)
	}
	return g, s
}

func BenchmarkQueueHistoryTick(b *testing.B) {
	for _, done := range []int{0, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("done=%d", done), func(b *testing.B) {
			g, s := historyBenchmarkStore(b, done)
			for i, phase := range []string{"queued", "running"} {
				name := "Queued"
				if phase == "running" {
					name = "Running"
				}
				task, err := queueJSON(Object{"label": "live fixture", "priority": 0, "status": Object{name: Object{}}})
				if err != nil {
					b.Fatal(err)
				}
				if _, err = s.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,?)", done+i, phase, task, phase); err != nil {
					b.Fatal(err)
				}
			}
			d := &queueDaemon{g: g, store: s, children: map[int]*exec.Cmd{done + 1: {}}, cleaning: map[int]bool{}, errors: map[int]string{}}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := d.tick(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Measure a small RPC through the actual private daemon while its reconciliation
// ticker runs. History snapshots remain intentionally proportional to history.
func BenchmarkQueueHistoryRPC(b *testing.B) {
	if runtime.GOOS != "darwin" {
		b.Skip("queue runtime uses macOS ownership APIs")
	}
	for _, done := range []int{0, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("done=%d", done), func(b *testing.B) {
			g, s := historyBenchmarkStore(b, done)
			s.close()
			executable, err := os.Executable()
			if err != nil {
				b.Fatal(err)
			}
			cmd := exec.Command(executable, "daemon")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if err = cmd.Start(); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				ended := make(chan error, 1)
				go func() { ended <- cmd.Wait() }()
				select {
				case <-ended:
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
					<-ended
				}
			})
			deadline := time.Now().Add(8 * time.Second)
			for {
				_, err = g.QueueControl("limit", Object{"limit": 1})
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					b.Fatalf("private daemon did not become ready: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Spread requests over at least several ticker intervals, measuring
			// response latency separately from the deliberate inter-request delay.
			b.ReportAllocs()
			durations := make([]time.Duration, b.N)
			b.ResetTimer()
			for i := range b.N {
				b.StopTimer()
				time.Sleep(10 * time.Millisecond)
				b.StartTimer()
				started := time.Now()
				result, err := g.QueueControl("limit", Object{"limit": 1})
				durations[i] = time.Since(started)
				if value, ok := integer(result["parallel_tasks"]); err != nil || !ok || value != 1 {
					b.Fatalf("private limit RPC: %v %v", result, err)
				}
			}
			b.StopTimer()
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			b.ReportMetric(float64(durations[(len(durations)-1)*95/100]), "p95-ns/op")
		})
	}
}
