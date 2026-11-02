package notaryapi

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// stageRecord is the durable notarization state of one stage. Records written
// by earlier runs omit prepared_run_id, and writers never add it to them. Output
// follows the last shell writers. Older records can hold prepared_attempt first;
// a rewrite reorders those keys, which is harmless because consumers ignore order.
type stageRecord struct {
	Stage           string `json:"stage"`
	Phase           string `json:"phase"`
	PreparedRunID   string `json:"prepared_run_id,omitempty"`
	PreparedAttempt string `json:"prepared_attempt"`
	ArchiveSHA256   string `json:"archive_sha256"`
	SubmissionID    string `json:"submission_id"`
	CompletedSHA256 string `json:"completed_sha256"`
}

const (
	phasePrepared  = "prepared"
	phaseUploading = "uploading"
	phaseSubmitted = "submitted"
	phaseAccepted  = "accepted"
	phaseFinished  = "finished"
)

// previousPhase is the only legal source of each later phase. Nothing moves
// backwards or repeats, so an uncertain upload can never be re-marked prepared.
var previousPhase = map[string]string{
	phaseUploading: phasePrepared,
	phaseSubmitted: phaseUploading,
	phaseAccepted:  phaseSubmitted,
	phaseFinished:  phaseAccepted,
}

func (r stageRecord) valid() bool {
	if (r.Stage != "app" && r.Stage != "dmg") || !attemptPattern.MatchString(r.PreparedAttempt) || !hashPattern.MatchString(r.ArchiveSHA256) {
		return false
	}
	if r.PreparedRunID != "" && !attemptPattern.MatchString(r.PreparedRunID) {
		return false
	}
	switch r.Phase {
	case phasePrepared, phaseUploading:
		if r.SubmissionID != "" {
			return false
		}
	case phaseSubmitted, phaseAccepted, phaseFinished:
		if !uuidPattern.MatchString(r.SubmissionID) {
			return false
		}
	default:
		return false
	}
	if r.Phase == phaseFinished {
		return hashPattern.MatchString(r.CompletedSHA256)
	}
	return r.CompletedSHA256 == ""
}

// advance returns r in its next phase. Only submitted may set the ID and only
// finished may set the completed hash, so custody evidence is never replaced.
func (r stageRecord) advance(phase, id, completed string) (stageRecord, error) {
	previous, known := previousPhase[phase]
	if !known || r.Phase != previous || (id != "") != (phase == phaseSubmitted) || (completed != "") != (phase == phaseFinished) {
		return r, failure("illegal_transition")
	}
	r.Phase = phase
	if id != "" {
		r.SubmissionID = id
	}
	if completed != "" {
		r.CompletedSHA256 = completed
	}
	if !r.valid() {
		return r, failure("illegal_transition")
	}
	return r, nil
}

func (r stageRecord) field(name string) (string, bool) {
	switch name {
	case "stage":
		return r.Stage, true
	case "phase":
		return r.Phase, true
	case "prepared_attempt":
		return r.PreparedAttempt, true
	case "prepared_run_id":
		return r.PreparedRunID, true
	case "archive_sha256":
		return r.ArchiveSHA256, true
	case "submission_id":
		return r.SubmissionID, true
	case "completed_sha256":
		return r.CompletedSHA256, true
	}
	return "", false
}

func parseRecord(data []byte) (stageRecord, error) {
	var record stageRecord
	fields, err := recordFields(data)
	if err != nil || (len(fields) != 6 && len(fields) != 7) {
		return record, failure("invalid_record")
	}
	if raw, ok := fields["prepared_run_id"]; ok {
		if json.Unmarshal(raw, &record.PreparedRunID) != nil || !attemptPattern.MatchString(record.PreparedRunID) {
			return record, failure("invalid_record")
		}
	} else if len(fields) != 6 {
		return record, failure("invalid_record")
	}
	for _, key := range []string{"stage", "phase", "prepared_attempt", "archive_sha256", "submission_id", "completed_sha256"} {
		var value string
		if raw, ok := fields[key]; !ok || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return record, failure("invalid_record")
		}
	}
	if json.Unmarshal(data, &record) != nil || !record.valid() {
		return record, failure("invalid_record")
	}
	return record, nil
}

func readRecord(dir *os.File, name string) (stageRecord, error) {
	f, err := openRegularAt(dir, name)
	if err != nil {
		return stageRecord{}, failure("invalid_record")
	}
	defer f.Close()
	data, err := readBounded(f, 16<<10)
	if err != nil {
		return stageRecord{}, failure("invalid_record")
	}
	return parseRecord(data)
}

func readRecordPath(path string) (stageRecord, error) {
	dir, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return stageRecord{}, failure("invalid_record")
	}
	defer dir.Close()
	return readRecord(dir, filepath.Base(path))
}

// lockRecordDirectory locks the directory inode, not the atomically replaced
// record inode. The returned function releases the lock and the directory.
func lockRecordDirectory(path string) (*os.File, func(), error) {
	dir, err := openDirectory(filepath.Dir(path))
	if err != nil {
		return nil, nil, failure("invalid_record")
	}
	if err = unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = dir.Close()
		return nil, nil, failure("record_locked")
	}
	return dir, func() {
		_ = unix.Flock(int(dir.Fd()), unix.LOCK_UN)
		_ = dir.Close()
	}, nil
}

func saveRecord(persist func(*os.File, string, []byte) error, dir *os.File, name string, record stageRecord) error {
	if !record.valid() {
		return failure("invalid_record")
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err == nil {
		err = persist(dir, name, append(data, '\n'))
	}
	if err != nil {
		return failure("record_persistence_failed")
	}
	return nil
}

func createRecord(path string, record stageRecord) error {
	dir, unlock, err := lockRecordDirectory(path)
	if err != nil {
		return err
	}
	defer unlock()
	name := filepath.Base(path)
	var info unix.Stat_t
	err = unix.Fstatat(int(dir.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return failure("record_exists")
	}
	if !errors.Is(err, unix.ENOENT) {
		return failure("invalid_record")
	}
	return saveRecord(atomicWrite, dir, name, record)
}

func advanceRecord(path, phase, id, completed string) error {
	dir, unlock, err := lockRecordDirectory(path)
	if err != nil {
		return err
	}
	defer unlock()
	name := filepath.Base(path)
	record, err := readRecord(dir, name)
	if err != nil {
		return err
	}
	if record, err = record.advance(phase, id, completed); err != nil {
		return err
	}
	return saveRecord(atomicWrite, dir, name, record)
}

// runRecord is the credential-free interface the pipeline scripts use. Every
// failure exits nonzero with a fixed code, and paths never reach the output.
func runRecord(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return failure("invalid_arguments")
	}
	flags := flag.NewFlagSet("notarize record", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var path, stage, field, phase, id, completed, run, attempt, hash string
	flags.StringVar(&path, "record", "", "durable stage record")
	switch args[0] {
	case "validate":
		flags.StringVar(&stage, "stage", "", "expected stage")
	case "get":
		flags.StringVar(&field, "field", "", "comma-separated record fields to print")
		flags.StringVar(&stage, "stage", "", "expected stage")
	case "create":
		flags.StringVar(&stage, "stage", "", "stage")
		flags.StringVar(&run, "run", "", "preparing execution ID")
		flags.StringVar(&attempt, "attempt", "", "preparing execution attempt")
		flags.StringVar(&hash, "sha256", "", "prepared archive SHA-256")
	case "advance":
		flags.StringVar(&phase, "to", "", "next phase")
		flags.StringVar(&id, "id", "", "Apple submission UUID")
		flags.StringVar(&completed, "completed-sha256", "", "completed artifact SHA-256")
	default:
		return failure("invalid_arguments")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || path == "" {
		return failure("invalid_arguments")
	}
	switch args[0] {
	case "validate":
		if stage == "" {
			return failure("invalid_arguments")
		}
		record, err := readRecordPath(path)
		if err != nil {
			return err
		}
		if record.Stage != stage {
			return failure("invalid_record")
		}
	case "get":
		record, err := readRecordPath(path)
		if err != nil {
			return err
		}
		if stage != "" && record.Stage != stage {
			return failure("invalid_record")
		}
		// Validated values never contain commas. One call per read keeps resume
		// cheap: every operation verifies all state twice.
		names := strings.Split(field, ",")
		values := make([]string, len(names))
		for i, name := range names {
			var known bool
			if values[i], known = record.field(name); !known {
				return failure("invalid_arguments")
			}
		}
		if _, err = fmt.Fprintln(stdout, strings.Join(values, ",")); err != nil {
			return failure("output_failed")
		}
	case "create":
		// Every record created now carries its execution ID. Only earlier runs omit it.
		if run == "" {
			return failure("invalid_arguments")
		}
		return createRecord(path, stageRecord{Stage: stage, Phase: phasePrepared, PreparedAttempt: attempt, PreparedRunID: run, ArchiveSHA256: hash})
	case "advance":
		return advanceRecord(path, phase, id, completed)
	}
	return nil
}
