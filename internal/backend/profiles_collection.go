package backend

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"syscall"
)

// profileSession owns one connection for a finite collection. It never retains a
// transaction while sampling processes, accessing queue RPC, or waiting on locks.
type profileSession struct {
	store           *ProfileStore
	db              *sql.DB
	conn            *sql.Conn
	file, directory os.FileInfo
}

func (s *ProfileStore) openSession(write bool) (*profileSession, error) {
	if write && s.readonly {
		return nil, fmt.Errorf("Resource history was opened read-only")
	}
	if s.empty {
		return nil, fmt.Errorf("Resource history is missing")
	}
	if e := s.checkFiles(); e != nil {
		return nil, e
	}
	directory, e := os.Lstat(s.state)
	if e != nil {
		return nil, fmt.Errorf("Unsafe resource history directory")
	}
	st, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || !directory.IsDir() || directory.Mode().Perm()&0077 != 0 || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("Unsafe resource history directory")
	}
	file, e := os.Lstat(s.path)
	if e != nil {
		return nil, e
	}
	mode := "ro"
	if write {
		mode = "rw"
	}
	u := url.URL{Scheme: "file", Path: s.path}
	q := u.Query()
	q.Set("mode", mode)
	q.Add("_pragma", "busy_timeout(250)")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, fmt.Errorf("Cannot safely access resource history")
	}
	db.SetMaxOpenConns(1)
	conn, e := db.Conn(profileContext)
	if e != nil {
		db.Close()
		return nil, fmt.Errorf("Cannot safely access resource history: %w", e)
	}
	session := &profileSession{store: s, db: db, conn: conn, file: file, directory: directory}
	if e = session.check(); e != nil {
		session.Close()
		return nil, e
	}
	return session, nil
}
func (s *profileSession) check() error {
	if e := s.store.checkFiles(); e != nil {
		return e
	}
	file, e := os.Lstat(s.store.path)
	if e != nil || !os.SameFile(s.file, file) {
		return fmt.Errorf("Resource history changed during collection")
	}
	directory, e := os.Lstat(s.store.state)
	if e != nil || !os.SameFile(s.directory, directory) || !directory.IsDir() || directory.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("Resource history directory changed during collection")
	}
	st, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("Unsafe resource history directory")
	}
	return nil
}
func (s *profileSession) Close() {
	if s.conn != nil {
		s.conn.ExecContext(profileContext, "ROLLBACK")
		s.conn.Close()
		s.conn = nil
		s.db.Close()
	}
}
func (s *profileSession) transaction(write bool, fn func(profileSQL) error) error {
	if e := s.check(); e != nil {
		return e
	}
	begin := "PRAGMA query_only=ON;BEGIN"
	if write {
		if s.store.readonly {
			return fmt.Errorf("Resource history was opened read-only")
		}
		begin = "PRAGMA query_only=OFF;PRAGMA journal_mode=DELETE;PRAGMA synchronous=FULL;BEGIN IMMEDIATE"
	}
	if _, e := s.conn.ExecContext(profileContext, begin); e != nil {
		return fmt.Errorf("Cannot safely access resource history: %w", e)
	}
	defer s.conn.ExecContext(profileContext, "ROLLBACK")
	if e := fn(s.conn); e != nil {
		return e
	}
	if e := s.check(); e != nil {
		return e
	}
	if write {
		_, e := s.conn.ExecContext(profileContext, "COMMIT")
		return e
	}
	return nil
}

type profileRequest struct{ identity, description Object }
type profileCollection struct {
	session              *profileSession
	revision, version    int
	predictions, matches []Object
}

func (c *profileCollection) Close() { c.session.Close() }

// Validate locks the current snapshot through the caller's durable admission
// write. Release must run before queue RPC. data_version is compared only on the
// same live connection; new samples and operator corrections both invalidate it.
func (c *profileCollection) Validate() (valid bool, release func(), err error) {
	s := c.session
	if e := s.check(); e != nil {
		return false, nil, e
	}
	if _, e := s.conn.ExecContext(profileContext, "PRAGMA query_only=ON;BEGIN"); e != nil {
		return false, nil, e
	}
	released := false
	release = func() {
		if !released {
			s.conn.ExecContext(profileContext, "ROLLBACK")
			released = true
		}
	}
	// Reading a table establishes the SHARED lock before checking data_version.
	var version int
	if e := s.conn.QueryRowContext(profileContext, "SELECT COUNT(*) FROM sqlite_master").Scan(&version); e != nil {
		release()
		return false, nil, e
	}
	if e := s.conn.QueryRowContext(profileContext, "PRAGMA data_version").Scan(&version); e != nil {
		release()
		return false, nil, e
	}
	if e := s.check(); e != nil {
		release()
		return false, nil, e
	}
	return version == c.version, release, nil
}

// Encode the membership set as one JSON parameter to avoid SQLite's host
// parameter limit without imposing a job-count policy. All values are strings.
func profileFamilySet(families []any) string {
	encoded, _ := json.Marshal(families)
	return string(encoded)
}

// collectProfiles batches descriptions and prediction inputs within a collection.
// Only changed descriptions acquire a writer lock, and that transaction rechecks
// rows so a concurrent operator regroup cannot be overwritten by a stale read.
func (s *ProfileStore) collectProfiles(requests []profileRequest, machine string) (*profileCollection, error) {
	if machine == "" {
		return nil, fmt.Errorf("A prediction needs a machine identity")
	}
	descriptions := map[string]Object{}
	families := []any{}
	for _, request := range requests {
		clean, e := profileDescription(request.identity, request.description)
		if e != nil {
			return nil, e
		}
		family := text(request.identity["family"])
		if _, exists := descriptions[family]; !exists {
			families = append(families, family)
			descriptions[family] = Object{}
		}
		for key, value := range clean {
			if value != nil {
				descriptions[family][key] = value
			}
		}
	}
	session, e := s.openSession(!s.readonly)
	if e != nil {
		return nil, e
	}
	result := &profileCollection{session: session, predictions: make([]Object, len(requests)), matches: make([]Object, len(requests))}
	succeeded := false
	defer func() {
		if !succeeded {
			result.Close()
		}
	}()
	has := false
	changed := []string{}
	if e = session.transaction(false, func(c profileSQL) error {
		var err error
		has, err = hasProfileLearning(c)
		if err != nil || !has || len(families) == 0 {
			return err
		}
		rows, err := profileRows(c, "SELECT * FROM learning_members WHERE family IN (SELECT value FROM json_each(?))", profileFamilySet(families))
		if err != nil {
			return err
		}
		prior := map[string]Object{}
		for _, row := range rows {
			prior[text(row["family"])] = row
		}
		for family, clean := range descriptions {
			if profileDescriptionChanged(prior[family], clean) {
				changed = append(changed, family)
			}
		}
		return nil
	}); e != nil {
		return nil, e
	}
	// Bound the amount of description work under each writer lock. This is an
	// internal transaction batch size, not a limit on jobs or workload families.
	const descriptionBatch = 128
	if !s.readonly {
		for start := 0; start < len(changed); start += descriptionBatch {
			batch := changed[start:min(start+descriptionBatch, len(changed))]
			if e = session.transaction(true, func(c profileSQL) error {
				for _, family := range batch {
					if e := profileObserve(c, family, descriptions[family]); e != nil {
						return e
					}
				}
				return nil
			}); e != nil {
				return nil, e
			}
		}
	}
	if e = session.transaction(false, func(c profileSQL) error {
		var err error
		if err = session.conn.QueryRowContext(profileContext, "PRAGMA data_version").Scan(&result.version); err != nil {
			return err
		}
		has, err = hasProfileLearning(c)
		if err != nil {
			return err
		}
		result.revision, err = profileRevision(c)
		if err != nil {
			return err
		}
		if len(families) == 0 {
			return nil
		}
		return result.read(c, requests, families, profileDigest([]any{"machine-v1", machine}), has)
	}); e != nil {
		return nil, e
	}
	succeeded = true
	return result, nil
}

func (c *profileCollection) read(db profileSQL, requests []profileRequest, families []any, machine string, has bool) error {
	members, profiles := map[string]Object{}, map[string]Object{}
	groups := map[string][]Object{}
	familySet := map[string]bool{}
	for _, family := range families {
		familySet[text(family)] = true
	}
	if has {
		rows, e := profileRows(db, "SELECT * FROM learning_members WHERE profile IN (SELECT profile FROM learning_members WHERE family IN (SELECT value FROM json_each(?)))", profileFamilySet(families))
		if e != nil {
			return e
		}
		for _, row := range rows {
			family, group := text(row["family"]), text(row["profile"])
			members[family] = row
			groups[group] = append(groups[group], row)
			familySet[family] = true
		}
		rows, e = profileRows(db, "SELECT * FROM learning_profiles WHERE id IN (SELECT profile FROM learning_members WHERE family IN (SELECT value FROM json_each(?)))", profileFamilySet(families))
		if e != nil {
			return e
		}
		for _, row := range rows {
			profiles[text(row["id"])] = row
		}
	}
	expanded := []any{}
	for family := range familySet {
		expanded = append(expanded, family)
	}
	args := []any{machine, profileFamilySet(expanded)}
	where := " WHERE machine=? AND family IN (SELECT value FROM json_each(?))"
	envelopes, e := profileRows(db, "SELECT * FROM envelopes"+where, args...)
	if e != nil {
		return e
	}
	if has {
		retained, e := profileRows(db, "SELECT * FROM learning_retained"+where, args...)
		if e != nil {
			return e
		}
		envelopes = append(envelopes, retained...)
	}
	runs, e := profileRows(db, "SELECT * FROM runs WHERE machine=? AND family IN (SELECT value FROM json_each(?)) AND sampled>=?", machine, profileFamilySet(families), resourceNow()-90*profileDay)
	if e != nil {
		return e
	}
	floors, e := profileRows(db, "SELECT memory,cpu FROM floors WHERE machine IN (?, '*')", machine)
	if e != nil {
		return e
	}
	envelopesByGroup, runsByVariant := map[string][]Object{}, map[string][]Object{}
	for _, row := range envelopes {
		family := text(row["family"])
		group := family
		if members[family] != nil {
			group = text(members[family]["profile"])
		}
		envelopesByGroup[group] = append(envelopesByGroup[group], row)
	}
	for _, row := range runs {
		key := text(row["family"]) + text(row["variant"])
		runsByVariant[key] = append(runsByVariant[key], row)
	}
	estimates, matches := map[string]Object{}, map[string]Object{}
	for i, request := range requests {
		family, variant := text(request.identity["family"]), text(request.identity["variant"])
		group := family
		if members[family] != nil {
			group = text(members[family]["profile"])
			if profiles[group] == nil {
				return fmt.Errorf("Workload grouping is inconsistent")
			}
		}
		if matches[family] == nil {
			match := Object{"id": family, "label": profileLabel(family, nil), "reason": "Automatic workload family"}
			if members[family] != nil {
				reason := "Automatic workload family"
				if len(groups[group]) > 1 || group != family {
					reason = "Operator grouping"
				}
				match = Object{"id": group, "label": profileGroupLabel(profiles[group], groups[group]), "reason": reason}
			}
			matches[family] = match
		}
		key := family + variant
		if estimates[key] == nil {
			selectedFloors := floors
			if metric(profiles[group]["ignore_legacy"]) != 0 {
				selectedFloors = nil
			}
			estimates[key] = profileEstimate(family, variant, profiles[group], envelopesByGroup[group], runsByVariant[key], selectedFloors)
		}
		// Each job owns its map because live process sampling raises its reservation.
		c.predictions[i] = maps.Clone(estimates[key])
		c.matches[i] = maps.Clone(matches[family])
	}
	return nil
}
