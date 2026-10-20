package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// QueueDatabasePath is shared by the daemon and the installation health check.
func QueueDatabasePath(state string) string { return filepath.Join(state, "queue.sqlite3") }

var errQueueChanged = errors.New("Queue job changed concurrently")

type queueStore struct {
	state, path string
	db          *sql.DB
}
type queueJob struct {
	id                    int
	token, attempt, phase string
	task, worker          Object
	outcome               sql.NullInt64
}

const queueSchema = `CREATE TABLE queue_settings(id INTEGER PRIMARY KEY CHECK(id=1),parallel_limit INTEGER NOT NULL CHECK(parallel_limit BETWEEN 1 AND 8),status TEXT NOT NULL CHECK(status IN ('Running','Paused')),dirty INTEGER NOT NULL,next_id INTEGER NOT NULL,imported INTEGER NOT NULL DEFAULT 0);
INSERT INTO queue_settings VALUES(1,1,'Running',0,0,0);
CREATE TABLE queue_jobs(id INTEGER PRIMARY KEY CHECK(id>=0),token TEXT NOT NULL UNIQUE,task TEXT NOT NULL,attempt TEXT NOT NULL DEFAULT '',phase TEXT NOT NULL CHECK(phase IN ('queued','starting','running','outcome','done')),worker TEXT,outcome INTEGER CHECK(outcome BETWEEN 0 AND 255));
PRAGMA user_version=1;`

func queuePrivateFiles(path string) error {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return fmt.Errorf("Queue database unavailable: %w", err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !fileOwned(info, true) || st.Nlink != 1 {
			return errors.New("Unsafe queue database or sidecar; work is held")
		}
	}
	return nil
}
func openQueueStore(state string, create bool) (*queueStore, error) {
	info, err := os.Lstat(state)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || int(st.Uid) != os.Getuid() {
		return nil, errors.New("Unsafe queue state directory")
	}
	path := QueueDatabasePath(state)
	if create {
		fd, e := syscall.Open(path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
		if e == nil {
			syscall.Close(fd)
		} else if !errors.Is(e, syscall.EEXIST) {
			return nil, e
		}
	}
	if err = queuePrivateFiles(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &queueStore{state: state, path: path, db: db}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err == nil {
		if version == 0 && create {
			tx, e := db.Begin()
			if e != nil {
				err = e
			} else {
				_, err = tx.Exec(queueSchema)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
		} else if version != 1 {
			err = errors.New("Unsupported queue schema")
		}
	}
	if err == nil {
		var check string
		err = db.QueryRow("PRAGMA quick_check").Scan(&check)
		if err == nil && check != "ok" {
			err = errors.New("Queue database integrity check failed")
		}
	}
	if err == nil {
		rows, e := db.Query("SELECT " + queueColumns + " FROM queue_jobs LIMIT 0")
		if e != nil {
			err = e
		} else {
			err = rows.Close()
		}
	}
	if err == nil {
		var limit, dirty, next, imported int
		var status string
		err = db.QueryRow("SELECT parallel_limit,status,dirty,next_id,imported FROM queue_settings WHERE id=1").Scan(&limit, &status, &dirty, &next, &imported)
		if err == nil && (ValidateLimit(limit) != nil || (status != "Running" && status != "Paused") || (dirty != 0 && dirty != 1) || (imported != 0 && imported != 1) || next < 0) {
			err = errors.New("Invalid persisted queue settings")
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ValidateQueueState validates the private database without starting a daemon.
func ValidateQueueState(state string) error {
	s, e := openQueueStore(state, false)
	if e != nil {
		return e
	}
	return s.db.Close()
}
func (s *queueStore) close() { s.db.Close() }
func (s *queueStore) settings() (Object, error) {
	if e := queuePrivateFiles(s.path); e != nil {
		return nil, e
	}
	var limit, dirty, next, imported int
	var status string
	e := s.db.QueryRow("SELECT parallel_limit,status,dirty,next_id,imported FROM queue_settings WHERE id=1").Scan(&limit, &status, &dirty, &next, &imported)
	return Object{"parallel_tasks": limit, "status": status, "dirty": dirty, "next_id": next, "imported": imported}, e
}
func scanQueueJob(row interface{ Scan(...any) error }) (queueJob, error) {
	var j queueJob
	var task string
	var worker sql.NullString
	e := row.Scan(&j.id, &j.token, &task, &j.attempt, &j.phase, &worker, &j.outcome)
	if e != nil {
		return j, e
	}
	j.task, e = decodeObject([]byte(task))
	if e == nil && worker.Valid {
		j.worker, e = decodeObject([]byte(worker.String))
	}
	return j, e
}

const queueColumns = "id,token,task,attempt,phase,worker,outcome"

func (s *queueStore) jobs() ([]queueJob, error) {
	if e := queuePrivateFiles(s.path); e != nil {
		return nil, e
	}
	rows, e := s.db.Query("SELECT " + queueColumns + " FROM queue_jobs ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	jobs := []queueJob{}
	for rows.Next() {
		j, e := scanQueueJob(rows)
		if e != nil {
			return nil, e
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
func (s *queueStore) job(id int) (queueJob, error) {
	if e := queuePrivateFiles(s.path); e != nil {
		return queueJob{}, e
	}
	return scanQueueJob(s.db.QueryRow("SELECT "+queueColumns+" FROM queue_jobs WHERE id=?", id))
}
func (s *queueStore) snapshot() (Object, error) {
	settings, e := s.settings()
	if e != nil {
		return nil, e
	}
	jobs, e := s.jobs()
	if e != nil {
		return nil, e
	}
	tasks := Object{}
	for _, j := range jobs {
		tasks[strconv.Itoa(j.id)] = j.task
	}
	return Object{"tasks": tasks, "groups": Object{"default": settings}}, nil
}
func queueJSON(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }
func queueNow() string                { return time.Now().UTC().Format(time.RFC3339Nano) }
func queueResult(code int) any {
	if code == 0 {
		return "Success"
	}
	return Object{"Failed": code}
}
func (s *queueStore) saveTask(j queueJob) error {
	v, e := queueJSON(j.task)
	if e != nil {
		return e
	}
	result, e := s.db.Exec("UPDATE queue_jobs SET task=? WHERE id=? AND token=? AND attempt=? AND phase=?", v, j.id, j.token, j.attempt, j.phase)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e == nil && n != 1 {
		return errQueueChanged
	}
	return e
}

// importLegacy is atomic and idempotent. The installer publishes this only after
// stopping and draining the old daemon; its original files remain untouched.
func (s *queueStore) importLegacy() error {
	settings, e := s.settings()
	if e != nil {
		return e
	}
	if settings["imported"] == 1 {
		return nil
	}
	snapshot, e := ReadPrivate(filepath.Join(s.state, "legacy-queue-import.json"))
	if errors.Is(e, os.ErrNotExist) {
		return s.seedIDs()
	}
	if e != nil {
		return e
	}
	tasks := obj(snapshot["tasks"])
	if tasks == nil {
		return errors.New("Invalid legacy queue import")
	}
	tx, e := s.db.BeginTx(context.Background(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var existing int
	if e = tx.QueryRow("SELECT count(*) FROM queue_jobs").Scan(&existing); e != nil {
		return e
	}
	if existing != 0 {
		return errors.New("Legacy import conflicts with an existing queue")
	}
	next := 0
	for key, raw := range tasks {
		id, e := strconv.Atoi(key)
		if e != nil || id < 0 || id == math.MaxInt {
			return errors.New("Invalid legacy job ID")
		}
		task := obj(raw)
		name, details, e := TaskStatus(task)
		if e != nil || name != "Done" {
			return errors.New("Legacy queue must be fully drained before import")
		}
		if _, e = ResultCode(details["result"]); e != nil {
			return e
		}
		priority := 0
		if raw, present := task["priority"]; present {
			var valid bool
			priority, valid = integer(raw)
			if !valid || ValidatePriority(priority) != nil {
				return errors.New("Invalid legacy priority")
			}
		}
		task["priority"] = priority
		if text(task["logPath"]) == "" {
			task["logPath"] = filepath.Join(s.state, "pueue", "task_logs", key+".log")
		}
		tokenValue := CommandToken(task)
		if tokenValue == "" {
			tokenValue = token()
		}
		data, e := queueJSON(task)
		if e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO queue_jobs(id,token,task,phase) VALUES(?,?,?,'done')", id, tokenValue, data); e != nil {
			return e
		}
		next = max(next, id+1)
	}
	next, e = queueHistoricalNext(s.state, next)
	if e != nil {
		return e
	}
	limit, status, e := legacyQueueSettings(snapshot)
	if e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE queue_settings SET next_id=?,parallel_limit=?,status=?,imported=1 WHERE id=1", next, limit, status); e != nil {
		return e
	}
	return tx.Commit()
}

func legacyQueueSettings(snapshot Object) (int, string, error) {
	group := obj(obj(snapshot["groups"])["default"])
	if group == nil {
		return 0, "", errors.New("Legacy import is missing default queue settings")
	}
	limit, valid := integer(group["parallel_tasks"])
	if !valid || ValidateLimit(limit) != nil {
		return 0, "", errors.New("Invalid legacy queue limit")
	}
	status := text(group["status"])
	if status != "Running" && status != "Paused" {
		return 0, "", errors.New("Invalid legacy queue status")
	}
	return limit, status, nil
}
func queueHistoricalNext(state string, next int) (int, error) {
	for _, directory := range []string{"receipts", "cancelled", "resource-runs", "metadata"} {
		entries, e := os.ReadDir(filepath.Join(state, directory))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return 0, e
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			base := strings.TrimSuffix(entry.Name(), ".json")
			if id, e := strconv.Atoi(base); e == nil && id >= 0 {
				if id == math.MaxInt {
					return 0, errors.New("Queue job ID space exhausted")
				}
				next = max(next, id+1)
			}
			value, e := ReadPrivate(filepath.Join(state, directory, entry.Name()))
			if e != nil {
				return 0, e
			}
			if id, ok := integer(value["task_id"]); ok && id >= 0 {
				if id == math.MaxInt {
					return 0, errors.New("Queue job ID space exhausted")
				}
				next = max(next, id+1)
			}
		}
	}
	return next, nil
}
func (s *queueStore) seedIDs() error {
	v, e := s.settings()
	if e != nil {
		return e
	}
	next, _ := integer(v["next_id"])
	next, e = queueHistoricalNext(s.state, next)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("UPDATE queue_settings SET next_id=max(next_id,?) WHERE id=1", next)
	return e
}
