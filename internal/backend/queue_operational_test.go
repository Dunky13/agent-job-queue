package backend

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func operationalFixture(t testing.TB, s *queueStore, done int) {
	t.Helper()
	task, err := queueJSON(Object{"label": "retained fixture", "priority": 0, "status": Object{"Done": Object{"result": "Success"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`WITH RECURSIVE ids(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM ids WHERE i+1<?)
INSERT INTO queue_jobs(id,token,task,phase) SELECT i,'history-'||i,?,'done' FROM ids WHERE i<?`, done, task, done)
	if err != nil {
		t.Fatal(err)
	}
}

func TestQueueOperationalQueryPreservesAllUnfinishedPhasesAndHistory(t *testing.T) {
	_, s := edgeQueueStore(t)
	operationalFixture(t, s, 10000)
	for i, phase := range []string{"queued", "starting", "running", "outcome"} {
		if _, err := s.db.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,?)", 10000+i, phase, `{"status":{"Queued":{}},"priority":0}`, phase); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.unfinishedJobs()
	if err != nil {
		t.Fatal(err)
	}
	phases := []string{}
	for _, job := range jobs {
		phases = append(phases, job.phase)
	}
	if !reflect.DeepEqual(phases, []string{"queued", "starting", "running", "outcome"}) {
		t.Fatalf("unfinished phases or ID order changed: %v", phases)
	}
	snapshot, err := s.snapshot()
	if err != nil || len(obj(snapshot["tasks"])) != 10004 {
		t.Fatalf("public snapshot lost retained history: %v", err)
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN SELECT " + queueColumns + " FROM queue_jobs WHERE phase!='done' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	usedIndex := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		usedIndex = usedIndex || strings.Contains(detail, "queue_jobs_unfinished")
	}
	if err = rows.Err(); err != nil || !usedIndex {
		t.Fatalf("operational query did not use its partial index: %v", err)
	}
	// Outcome rows must remain visible until cleanup, then leave the index.
	if _, err = s.db.Exec("UPDATE queue_jobs SET phase='done' WHERE phase='outcome'"); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.unfinishedJobs()
	if err != nil || len(jobs) != 3 {
		t.Fatalf("completed cleanup did not leave operational set: %v %v", jobs, err)
	}
}

func TestQueueOperationalMigrationPreservesStateAndSerializesOpeners(t *testing.T) {
	g, s := edgeQueueStore(t)
	operationalFixture(t, s, 2)
	if _, err := s.db.Exec("DROP INDEX queue_jobs_unfinished; PRAGMA user_version=1; UPDATE queue_settings SET parallel_limit=4,status='Paused',dirty=1,next_id=29,imported=1"); err != nil {
		t.Fatal(err)
	}
	s.close()
	var group sync.WaitGroup
	problems := make(chan error, 6)
	for range 6 {
		group.Add(1)
		go func() {
			defer group.Done()
			opened, err := openQueueStore(g.State, false)
			if err == nil {
				defer opened.close()
				settings, readErr := opened.settings()
				if readErr != nil {
					err = readErr
				} else if settings["parallel_tasks"] != 4 || settings["status"] != "Paused" || settings["dirty"] != 1 || settings["next_id"] != 29 || settings["imported"] != 1 {
					err = fmt.Errorf("migration changed settings: %v", settings)
				}
				var version, count int
				if err == nil {
					err = opened.db.QueryRow("PRAGMA user_version").Scan(&version)
				}
				if err == nil {
					err = opened.db.QueryRow("SELECT count(*) FROM queue_jobs").Scan(&count)
				}
				if err == nil && (version != 2 || count != 2) {
					err = fmt.Errorf("migration lost version/history: %d %d", version, count)
				}
			}
			problems <- err
		}()
	}
	group.Wait()
	close(problems)
	for err := range problems {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestQueueOperationalMigrationFailureIsAtomic(t *testing.T) {
	g, s := edgeQueueStore(t)
	operationalFixture(t, s, 2)
	if _, err := s.db.Exec("DROP INDEX queue_jobs_unfinished; CREATE INDEX queue_jobs_unfinished ON queue_jobs(token); PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if opened, err := openQueueStore(g.State, false); err == nil {
		opened.close()
		t.Fatal("migration accepted a conflicting operational index")
	}
	var version, count int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM queue_jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 1 || count != 2 {
		t.Fatalf("failed migration changed durable state: version=%d rows=%d", version, count)
	}
}

func TestQueueOperationalVersionRequiresValidIndex(t *testing.T) {
	for _, change := range []string{"DROP INDEX queue_jobs_unfinished", "DROP INDEX queue_jobs_unfinished; CREATE INDEX queue_jobs_unfinished ON queue_jobs(token)", "PRAGMA user_version=3"} {
		t.Run(change, func(t *testing.T) {
			g, s := edgeQueueStore(t)
			if _, err := s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if opened, err := openQueueStore(g.State, false); err == nil {
				opened.close()
				t.Fatal("invalid operational schema accepted")
			}
		})
	}
}

func TestQueueOperationalQueryRetainsPrivateFileChecks(t *testing.T) {
	_, s := edgeQueueStore(t)
	if err := os.Chmod(s.path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.unfinishedJobs(); err == nil {
		t.Fatal("operational query read a public database")
	}
}

func TestQueueOperationalTickRetainsCompletedHistory(t *testing.T) {
	g, s := edgeQueueStore(t)
	operationalFixture(t, s, 10000)
	if _, err := s.db.Exec("UPDATE queue_settings SET status='Paused'"); err != nil {
		t.Fatal(err)
	}
	d := &queueDaemon{g: g, store: s, children: map[int]*exec.Cmd{}, cleaning: map[int]bool{}, errors: map[int]string{}}
	if err := d.tick(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM queue_jobs WHERE phase='done'").Scan(&count); err != nil || count != 10000 {
		t.Fatalf("tick changed retained history: %d %v", count, err)
	}
}
