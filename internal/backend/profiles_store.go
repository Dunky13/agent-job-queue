package backend

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"unicode"

	_ "modernc.org/sqlite"
)

const maxProfileRuns = 4096
const maxProfileEnvelopes = 2048
const maxProfileMachines = 64
const maxProfileDecisions = 4096
const maxRegisteredFamilies = 8192
const maxRetainedEnvelopes = 8192

type ProfileStore struct {
	state, path     string
	readonly, empty bool
}
type profileSQL interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var profileContext = context.Background()

const profileBaseSchema = `CREATE TABLE runs (token TEXT PRIMARY KEY,family TEXT NOT NULL,variant TEXT NOT NULL,context TEXT NOT NULL,machine TEXT NOT NULL,memory INTEGER NOT NULL,cpu REAL NOT NULL,cpu_seconds REAL NOT NULL,duration REAL NOT NULL,sampled REAL NOT NULL,success INTEGER NOT NULL);
CREATE INDEX runs_profile ON runs(machine,family,variant,sampled);
CREATE TABLE envelopes (machine TEXT NOT NULL,family TEXT NOT NULL,variant TEXT NOT NULL,memory INTEGER NOT NULL,cpu REAL NOT NULL,duration REAL NOT NULL,sampled REAL NOT NULL,PRIMARY KEY(machine,family,variant));
CREATE TABLE floors (machine TEXT PRIMARY KEY,memory INTEGER NOT NULL,cpu REAL NOT NULL,sampled REAL NOT NULL);PRAGMA user_version=1;`
const profileDecisionSchema = `CREATE TABLE IF NOT EXISTS decisions (id INTEGER PRIMARY KEY,token TEXT NOT NULL,mode TEXT NOT NULL,memory INTEGER NOT NULL,cpu REAL NOT NULL,duration REAL,samples INTEGER NOT NULL,confidence TEXT NOT NULL,source TEXT NOT NULL,allowed INTEGER NOT NULL,reason TEXT NOT NULL,sampled REAL NOT NULL);CREATE INDEX IF NOT EXISTS decisions_token ON decisions(token,mode,id DESC);`
const profileLearningSchema = `CREATE TABLE IF NOT EXISTS learning_state (id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL);INSERT OR IGNORE INTO learning_state VALUES (1,0);
CREATE TABLE IF NOT EXISTS learning_profiles (id TEXT PRIMARY KEY,label TEXT,memory_floor INTEGER,cpu_floor REAL,pinned INTEGER NOT NULL,ignore_legacy INTEGER NOT NULL,updated REAL NOT NULL);
CREATE TABLE IF NOT EXISTS learning_members (family TEXT PRIMARY KEY,profile TEXT NOT NULL,title TEXT,project TEXT,executable TEXT,updated REAL NOT NULL);CREATE INDEX IF NOT EXISTS learning_member_profile ON learning_members(profile);
CREATE TABLE IF NOT EXISTS learning_resets (family TEXT PRIMARY KEY,sampled REAL NOT NULL);
CREATE TABLE IF NOT EXISTS learning_retained (machine TEXT NOT NULL,family TEXT NOT NULL,variant TEXT NOT NULL,memory INTEGER NOT NULL,cpu REAL NOT NULL,duration REAL NOT NULL,sampled REAL NOT NULL,PRIMARY KEY(machine,family,variant));`

func NewProfileStore(state string, readonly bool) (*ProfileStore, error) {
	s := &ProfileStore{state: state, path: filepath.Join(state, "resource-history.sqlite3"), readonly: readonly}
	if !readonly {
		if e := os.MkdirAll(state, 0700); e != nil {
			return nil, fmt.Errorf("Cannot safely open resource history")
		}
	}
	info, e := os.Lstat(state)
	if readonly && os.IsNotExist(e) {
		s.empty = true
		return s, nil
	}
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("Unsafe resource history directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("Unsafe resource history directory")
	}
	if !readonly {
		fd, e := syscall.Open(s.path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if e == nil {
			syscall.Close(fd)
		} else if !os.IsExist(e) {
			return nil, fmt.Errorf("Cannot safely open resource history")
		}
	} else if _, e := os.Lstat(s.path); os.IsNotExist(e) {
		s.empty = true
		return s, nil
	}
	e = s.connection(!readonly, func(c profileSQL) error {
		var version int
		if e := c.QueryRowContext(profileContext, "PRAGMA user_version").Scan(&version); e != nil {
			return e
		}
		if version == 0 && !readonly {
			if _, e := c.ExecContext(profileContext, profileBaseSchema); e != nil {
				return e
			}
		} else if version != 1 {
			return fmt.Errorf("Unsupported resource history schema")
		}
		if !readonly {
			if _, e := c.ExecContext(profileContext, profileDecisionSchema+profileLearningSchema); e != nil {
				return e
			}
		}
		var check string
		if e := c.QueryRowContext(profileContext, "PRAGMA quick_check").Scan(&check); e != nil {
			return e
		}
		if check != "ok" {
			return fmt.Errorf("Resource history integrity check failed")
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return s, nil
}
func (s *ProfileStore) checkFiles() error {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		info, e := os.Lstat(s.path + suffix)
		if os.IsNotExist(e) && suffix != "" {
			continue
		}
		if e != nil {
			return fmt.Errorf("Resource history is missing")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 || st.Nlink != 1 {
			return fmt.Errorf("Unsafe resource history file")
		}
	}
	return nil
}
func (s *ProfileStore) connection(write bool, fn func(profileSQL) error) error {
	if write && s.readonly {
		return fmt.Errorf("Resource history was opened read-only")
	}
	if s.empty {
		return fmt.Errorf("Resource history is missing")
	}
	if e := s.checkFiles(); e != nil {
		return e
	}
	mode := "ro"
	if write {
		mode = "rw"
	}
	u := url.URL{Scheme: "file", Path: s.path}
	query := u.Query()
	query.Set("mode", mode)
	query.Add("_pragma", "busy_timeout(250)")
	u.RawQuery = query.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return fmt.Errorf("Cannot safely access resource history")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, e := db.Conn(profileContext)
	if e != nil {
		return fmt.Errorf("Cannot safely access resource history")
	}
	defer conn.Close()
	if write {
		if _, e = conn.ExecContext(profileContext, "PRAGMA journal_mode=DELETE;PRAGMA synchronous=FULL;BEGIN IMMEDIATE"); e != nil {
			return fmt.Errorf("Cannot safely access resource history: %w", e)
		}
	} else {
		if _, e = conn.ExecContext(profileContext, "PRAGMA query_only=ON;BEGIN"); e != nil {
			return fmt.Errorf("Cannot safely access resource history")
		}
	}
	defer conn.ExecContext(profileContext, "ROLLBACK")
	if e = fn(conn); e != nil {
		return e
	}
	if write {
		_, e = conn.ExecContext(profileContext, "COMMIT")
	}
	return e
}
func profileRows(c profileSQL, query string, args ...any) ([]Object, error) {
	rows, e := c.QueryContext(profileContext, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	names, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	result := []Object{}
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if e := rows.Scan(pointers...); e != nil {
			return nil, e
		}
		row := Object{}
		for i, name := range names {
			row[name] = values[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
func profileRow(c profileSQL, query string, args ...any) (Object, error) {
	rows, e := profileRows(c, query, args...)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}
func profileExec(c profileSQL, query string, args ...any) error {
	_, e := c.ExecContext(profileContext, query, args...)
	return e
}
func hasProfileLearning(c profileSQL) (bool, error) {
	row, e := profileRow(c, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='learning_state'")
	return row != nil, e
}
func profileRevision(c profileSQL) (int, error) {
	has, e := hasProfileLearning(c)
	if e != nil {
		return 0, e
	}
	if !has {
		return 0, nil
	}
	row, e := profileRow(c, "SELECT revision FROM learning_state WHERE id=1")
	if e != nil {
		return 0, e
	}
	revision, ok := integer(row["revision"])
	if !ok || revision < 0 {
		return 0, fmt.Errorf("Invalid learning revision")
	}
	return revision, nil
}
func (s *ProfileStore) Revision() (revision int, e error) {
	if s.empty {
		return 0, nil
	}
	e = s.connection(false, func(c profileSQL) error { var err error; revision, err = profileRevision(c); return err })
	return
}
func (s *ProfileStore) Close() {}
func profileNumber(value any, name string) (float64, error) {
	n, ok := number(value)
	if !ok || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("Invalid resource metric: %s", name)
	}
	return n, nil
}
func profileID(value string) error {
	if !profileHash.MatchString(value) {
		return fmt.Errorf("Invalid workload profile identity")
	}
	return nil
}
func profileText(value any, name string, optional bool, maximum int, basename bool) (any, error) {
	if optional && value == nil {
		return nil, nil
	}
	v, ok := value.(string)
	if !ok || len([]rune(v)) < 1 || len([]rune(v)) > maximum || strings.TrimSpace(v) != v || !profilePrintable(v) || (basename && (strings.ContainsAny(v, "/\\") || v == "." || v == "..")) {
		return nil, fmt.Errorf("Invalid workload %s", name)
	}
	return v, nil
}
func profilePrintable(value string) bool {
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
func (s *ProfileStore) RecordDecision(token, mode string, prediction Object, allowed bool, reason string, sampledAt float64) error {
	if token == "" || (mode != "shadow" && mode != "adaptive") {
		return fmt.Errorf("Invalid resource decision identity or mode")
	}
	if len([]rune(reason)) < 1 || len([]rune(reason)) > 256 || !profilePrintable(reason) || strings.TrimSpace(reason) != reason {
		return fmt.Errorf("Invalid resource decision reason")
	}
	memory, ok := integer(prediction["memoryBytes"])
	samples, good := integer(prediction["samples"])
	if !ok || !good || memory < 0 || samples < 0 {
		return fmt.Errorf("Invalid resource decision memory or sample count")
	}
	cpu, e := profileNumber(prediction["cpuCores"], "cpuCores")
	if e != nil {
		return e
	}
	var duration any
	if prediction["durationSeconds"] != nil {
		duration, e = profileNumber(prediction["durationSeconds"], "durationSeconds")
		if e != nil {
			return e
		}
	}
	confidence, source := text(prediction["confidence"]), text(prediction["source"])
	if confidence == "" {
		confidence = "unknown"
	}
	if source == "" {
		source = "prior"
	}
	if !containsString([]string{"unknown", "low", "medium", "high"}, confidence) || !containsString([]string{"prior", "family", "variant"}, source) {
		return fmt.Errorf("Invalid resource decision confidence or source")
	}
	if sampledAt < 0 || math.IsInf(sampledAt, 0) || math.IsNaN(sampledAt) || sampledAt > resourceNow()+300 {
		return fmt.Errorf("Resource decision timestamp is out of range")
	}
	hashed := profileDigest([]any{"run-v1", token})
	allow := 0
	if allowed {
		allow = 1
	}
	values := []any{int64(memory), cpu, duration, int64(samples), confidence, source, int64(allow), reason}
	return s.connection(true, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT memory,cpu,duration,samples,confidence,source,allowed,reason FROM decisions WHERE token=? AND mode=? ORDER BY id DESC LIMIT 1", hashed, mode)
		if e != nil {
			return e
		}
		if row != nil {
			old := []any{row["memory"], row["cpu"], row["duration"], row["samples"], row["confidence"], row["source"], row["allowed"], row["reason"]}
			if reflect.DeepEqual(old, values) {
				return nil
			}
		}
		args := append([]any{hashed, mode}, values...)
		args = append(args, sampledAt)
		if e = profileExec(c, "INSERT INTO decisions (token,mode,memory,cpu,duration,samples,confidence,source,allowed,reason,sampled) VALUES (?,?,?,?,?,?,?,?,?,?,?)", args...); e != nil {
			return e
		}
		return profileExec(c, "DELETE FROM decisions WHERE id IN (SELECT id FROM decisions ORDER BY sampled DESC,id DESC LIMIT -1 OFFSET ?)", maxProfileDecisions)
	})
}
func profileRegistrySlot(c profileSQL) (bool, error) {
	var count int
	if e := c.QueryRowContext(profileContext, "SELECT COUNT(*) FROM learning_members").Scan(&count); e != nil {
		return false, e
	}
	if count < maxRegisteredFamilies {
		return true, nil
	}
	candidate, e := profileRow(c, `SELECT m.family,m.profile FROM learning_members m JOIN learning_profiles p ON p.id=m.profile WHERE p.pinned=0 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.family=m.family) AND NOT EXISTS(SELECT 1 FROM envelopes e WHERE e.family=m.family) AND NOT EXISTS(SELECT 1 FROM learning_retained e WHERE e.family=m.family) ORDER BY m.updated,m.family LIMIT 1`)
	if e != nil || candidate == nil {
		return false, e
	}
	if e = profileExec(c, "DELETE FROM learning_members WHERE family=?", candidate["family"]); e != nil {
		return false, e
	}
	e = profileExec(c, "DELETE FROM learning_profiles WHERE id=?", candidate["profile"])
	return e == nil, e
}
func profileRegister(c profileSQL, family string, description Object, required bool) (Object, error) {
	member, e := profileRow(c, "SELECT * FROM learning_members WHERE family=?", family)
	if e != nil || member != nil {
		return member, e
	}
	slot, e := profileRegistrySlot(c)
	if e != nil {
		return nil, e
	}
	if !slot {
		if required {
			return nil, fmt.Errorf("Learning registry is full; existing operator corrections were preserved")
		}
		return nil, nil
	}
	if description == nil {
		description = Object{}
	}
	id := family
	existing, e := profileRow(c, "SELECT 1 FROM learning_profiles WHERE id=?", id)
	if e != nil {
		return nil, e
	}
	if existing != nil {
		id = profileDigest([]any{"profile-v1", family, token()})
	}
	now := resourceNow()
	if e = profileExec(c, "INSERT INTO learning_profiles VALUES (?,?,NULL,NULL,0,0,?)", id, nil, now); e != nil {
		return nil, e
	}
	if e = profileExec(c, "INSERT INTO learning_members VALUES (?,?,?,?,?,?)", family, id, description["title"], description["project"], description["executable"], now); e != nil {
		return nil, e
	}
	return Object{"family": family, "profile": id, "title": description["title"], "project": description["project"], "executable": description["executable"], "updated": now}, nil
}
func (s *ProfileStore) Observe(identity, description Object) error {
	if description == nil {
		return fmt.Errorf("Invalid safe workload description")
	}
	if !validProfileIdentity(identity) {
		return fmt.Errorf("Invalid workload identity")
	}
	for key := range description {
		if !containsString([]string{"title", "project", "executable"}, key) {
			return fmt.Errorf("Invalid safe workload description")
		}
	}
	clean := Object{}
	for _, key := range []string{"title", "project", "executable"} {
		maximum := 80
		if key == "title" {
			maximum = 120
		}
		value, e := profileText(description[key], key, true, maximum, key != "title")
		if e != nil {
			return e
		}
		clean[key] = value
	}
	family := text(identity["family"])
	return s.connection(true, func(c profileSQL) error {
		prior, e := profileRow(c, "SELECT * FROM learning_members WHERE family=?", family)
		if e != nil {
			return e
		}
		if prior == nil {
			_, e = profileRegister(c, family, clean, false)
			return e
		}
		changed := false
		for _, key := range []string{"title", "project", "executable"} {
			if clean[key] == nil {
				clean[key] = prior[key]
			}
			if !reflect.DeepEqual(clean[key], prior[key]) {
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return profileExec(c, "UPDATE learning_members SET title=?,project=?,executable=?,updated=? WHERE family=?", clean["title"], clean["project"], clean["executable"], resourceNow(), family)
	})
}
func profileLabel(family string, member Object) string {
	for _, key := range []string{"title", "project", "executable"} {
		if v := text(member[key]); v != "" {
			return v
		}
	}
	return "Workload " + family[:min(12, len(family))]
}
func profileGroupLabel(profile Object, members []Object) string {
	if value := text(profile["label"]); value != "" {
		return value
	}
	var chosen Object
	for _, member := range members {
		if chosen == nil || text(member["family"]) == text(profile["id"]) || (text(chosen["family"]) != text(profile["id"]) && text(member["family"]) < text(chosen["family"])) {
			chosen = member
		}
	}
	return profileLabel(text(chosen["family"]), chosen)
}
func (s *ProfileStore) Match(identity Object) (Object, error) {
	if !validProfileIdentity(identity) {
		return nil, fmt.Errorf("Invalid workload identity")
	}
	family := text(identity["family"])
	result := Object{"id": family, "label": profileLabel(family, nil), "reason": "Automatic workload family"}
	if s.empty {
		return result, nil
	}
	e := s.connection(false, func(c profileSQL) error {
		has, e := hasProfileLearning(c)
		if e != nil || !has {
			return e
		}
		row, e := profileRow(c, "SELECT m.*,p.label FROM learning_members m JOIN learning_profiles p ON p.id=m.profile WHERE family=?", family)
		if e != nil || row == nil {
			return e
		}
		members, e := profileRows(c, "SELECT * FROM learning_members WHERE profile=?", row["profile"])
		if e != nil {
			return e
		}
		reason := "Automatic workload family"
		if len(members) > 1 || text(row["profile"]) != family {
			reason = "Operator grouping"
		}
		result = Object{"id": row["profile"], "label": profileGroupLabel(Object{"id": row["profile"], "label": row["label"]}, members), "reason": reason}
		return nil
	})
	return result, e
}
