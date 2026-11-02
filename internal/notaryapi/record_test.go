package notaryapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	recordHash      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recordCompleted = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	recordID        = "01234567-89ab-cdef-0123-456789ABCDEF"
)

func recordCommand(args ...string) (string, error) {
	var stdout bytes.Buffer
	err := runRecord(args, &stdout)
	return stdout.String(), err
}

func recordJSON(t *testing.T, change func(map[string]any)) []byte {
	t.Helper()
	fields := map[string]any{"stage": "app", "phase": "prepared", "prepared_attempt": "1", "prepared_run_id": "9", "archive_sha256": recordHash, "submission_id": "", "completed_sha256": ""}
	if change != nil {
		change(fields)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeRecordFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.json")
	writeFile(t, path, data)
	return path
}

func TestRecordValidationKeysPhasesAndFormats(t *testing.T) {
	type change = func(map[string]any)
	set := func(key string, value any) change { return func(f map[string]any) { f[key] = value } }
	phase := func(name, id, completed string) change {
		return func(f map[string]any) { f["phase"], f["submission_id"], f["completed_sha256"] = name, id, completed }
	}
	for _, test := range []struct {
		name  string
		valid bool
		edit  change
	}{
		{"prepared", true, nil},
		{"legacy without run id", true, func(f map[string]any) { delete(f, "prepared_run_id") }},
		{"dmg rejected by app binding", false, set("stage", "dmg")},
		{"uploading without id", true, phase("uploading", "", "")},
		{"submitted", true, phase("submitted", recordID, "")},
		{"accepted", true, phase("accepted", recordID, "")},
		{"finished", true, phase("finished", recordID, recordCompleted)},

		{"unknown key", false, set("extra", "x")},
		{"unknown key replacing run id", false, func(f map[string]any) { delete(f, "prepared_run_id"); f["extra"] = "x" }},
		{"missing stage", false, func(f map[string]any) { delete(f, "stage") }},
		{"missing phase", false, func(f map[string]any) { delete(f, "phase") }},
		{"missing attempt", false, func(f map[string]any) { delete(f, "prepared_attempt") }},
		{"missing hash", false, func(f map[string]any) { delete(f, "archive_sha256") }},
		{"missing id", false, func(f map[string]any) { delete(f, "submission_id") }},
		{"missing completed", false, func(f map[string]any) { delete(f, "completed_sha256") }},
		{"null id", false, set("submission_id", nil)},
		{"numeric attempt", false, set("prepared_attempt", 1)},
		{"empty run id", false, set("prepared_run_id", "")},
		{"null run id", false, set("prepared_run_id", nil)},
		{"zero run id", false, set("prepared_run_id", "0")},
		{"numeric run id", false, set("prepared_run_id", 9)},

		{"installer era stage", false, set("stage", "payload")},
		{"unknown phase", false, set("phase", "rejected")},
		{"empty phase", false, set("phase", "")},
		{"capitalized phase", false, set("phase", "Prepared")},

		{"zero attempt", false, set("prepared_attempt", "0")},
		{"padded attempt", false, set("prepared_attempt", "01")},
		{"short hash", false, set("archive_sha256", recordHash[:63])},
		{"uppercase hash", false, set("archive_sha256", strings.ToUpper(recordHash))},
		{"hash with newline", false, set("archive_sha256", recordHash+"\n")},

		{"prepared with id", false, phase("prepared", recordID, "")},
		{"uploading with id", false, phase("uploading", recordID, "")},
		{"submitted without id", false, phase("submitted", "", "")},
		{"accepted without id", false, phase("accepted", "", "")},
		{"finished without id", false, phase("finished", "", recordCompleted)},
		{"malformed id", false, phase("submitted", "01234567-89ab-cdef-0123", "")},
		{"id with newline", false, phase("submitted", recordID+"\n", "")},
		{"prepared with completed", false, phase("prepared", "", recordCompleted)},
		{"accepted with completed", false, phase("accepted", recordID, recordCompleted)},
		{"finished without completed", false, phase("finished", recordID, "")},
		{"finished with malformed completed", false, phase("finished", recordID, recordCompleted[:63])},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeRecordFile(t, recordJSON(t, test.edit))
			_, err := recordCommand("validate", "--record", path, "--stage", "app")
			if test.valid {
				if err != nil {
					t.Fatalf("rejected valid record: %v", err)
				}
				return
			}
			requireError(t, err, "invalid_record")
		})
	}
}

func TestRecordValidationRejectsUnsafeFiles(t *testing.T) {
	valid := recordJSON(t, nil)
	for name, data := range map[string][]byte{
		"duplicate key": append([]byte(`{"phase":"finished",`), valid[1:]...),
		"trailing json": append(append([]byte{}, valid...), []byte(`{}`)...),
		"array":         []byte(`[]`),
		"empty":         {},
		"oversized":     append(append(append([]byte{}, valid[:len(valid)-1]...), bytes.Repeat([]byte(" "), 16<<10)...), '}'),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := recordCommand("validate", "--record", writeRecordFile(t, data), "--stage", "app")
			requireError(t, err, "invalid_record")
		})
	}
	path := writeRecordFile(t, valid)
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"validate", "--record", link, "--stage", "app"}, {"get", "--record", link, "--field", "phase"}, {"advance", "--record", link, "--to", "uploading"}} {
		_, err := recordCommand(args...)
		requireError(t, err, "invalid_record")
	}
	fifo := filepath.Join(filepath.Dir(path), "fifo.json")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := recordCommand("validate", "--record", fifo, "--stage", "app")
	requireError(t, err, "invalid_record")
}

func TestRecordTransitionsAreExactlyTheLinearCustodyChain(t *testing.T) {
	stateOf := map[string]func(map[string]any){
		"prepared":  nil,
		"uploading": func(f map[string]any) { f["phase"] = "uploading" },
		"submitted": func(f map[string]any) { f["phase"], f["submission_id"] = "submitted", recordID },
		"accepted":  func(f map[string]any) { f["phase"], f["submission_id"] = "accepted", recordID },
		"finished": func(f map[string]any) {
			f["phase"], f["submission_id"], f["completed_sha256"] = "finished", recordID, recordCompleted
		},
	}
	arguments := map[string][]string{
		"prepared": {}, "uploading": {}, "accepted": {},
		"submitted": {"--id", recordID},
		"finished":  {"--completed-sha256", recordCompleted},
	}
	legal := map[string]string{"prepared": "uploading", "uploading": "submitted", "submitted": "accepted", "accepted": "finished"}
	for from, edit := range stateOf {
		for to, extra := range arguments {
			t.Run(from+" to "+to, func(t *testing.T) {
				before := recordJSON(t, edit)
				path := writeRecordFile(t, before)
				_, err := recordCommand(append([]string{"advance", "--record", path, "--to", to}, extra...)...)
				if legal[from] != to {
					requireError(t, err, "illegal_transition")
					if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
						t.Fatalf("refused transition changed the record: %s", after)
					}
					return
				}
				if err != nil {
					t.Fatalf("legal transition refused: %v", err)
				}
				if phase, err := recordCommand("get", "--record", path, "--field", "phase"); err != nil || phase != to+"\n" {
					t.Fatalf("phase after advance: %q, %v", phase, err)
				}
			})
		}
	}
}

func TestRecordAdvanceCannotForgeOrReplaceEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any)
		args []string
	}{
		{"submitted needs an id", func(f map[string]any) { f["phase"] = "uploading" }, []string{"--to", "submitted"}},
		{"submitted refuses a malformed id", func(f map[string]any) { f["phase"] = "uploading" }, []string{"--to", "submitted", "--id", "not-a-uuid"}},
		{"finished needs a hash", func(f map[string]any) { f["phase"], f["submission_id"] = "accepted", recordID }, []string{"--to", "finished"}},
		{"finished refuses a malformed hash", func(f map[string]any) { f["phase"], f["submission_id"] = "accepted", recordID }, []string{"--to", "finished", "--completed-sha256", "abc"}},
		{"accepted cannot replace the id", func(f map[string]any) { f["phase"], f["submission_id"] = "submitted", recordID }, []string{"--to", "accepted", "--id", "00000000-0000-0000-0000-000000000000"}},
		{"uploading cannot set a hash", nil, []string{"--to", "uploading", "--completed-sha256", recordCompleted}},
		{"prepared is never a target", nil, []string{"--to", "prepared"}},
		{"unknown target", nil, []string{"--to", "rejected"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := recordJSON(t, test.edit)
			path := writeRecordFile(t, before)
			_, err := recordCommand(append([]string{"advance", "--record", path}, test.args...)...)
			requireError(t, err, "illegal_transition")
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Fatalf("refused advance changed the record: %s", after)
			}
		})
	}
	_, err := recordCommand("advance", "--record", writeRecordFile(t, []byte(`{"phase":"prepared"}`)), "--to", "uploading")
	requireError(t, err, "invalid_record")
}

// The fixtures are the exact text the writers at 7b4b27c produced, including
// records from before f892944 that have no prepared_run_id.
func TestRecordCompatibilityWithRecordsAlreadyWritten(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.json")
	if err != nil || len(paths) != 12 {
		t.Fatalf("compatibility fixtures: %v %v", paths, err)
	}
	next := map[string][]string{
		"prepared": {"--to", "uploading"}, "uploading": {"--to", "submitted", "--id", recordID},
		"submitted": {"--to", "accepted"}, "accepted": {"--to", "finished", "--completed-sha256", recordCompleted},
	}
	for _, source := range paths {
		t.Run(filepath.Base(source), func(t *testing.T) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			path := writeRecordFile(t, data)
			stage, err := recordCommand("get", "--record", path, "--field", "stage")
			if err != nil {
				t.Fatalf("existing record no longer parses: %v", err)
			}
			if _, err = recordCommand("validate", "--record", path, "--stage", strings.TrimSpace(stage)); err != nil {
				t.Fatalf("existing record no longer validates: %v", err)
			}
			run, _ := recordCommand("get", "--record", path, "--field", "prepared_run_id")
			if legacy := strings.HasPrefix(filepath.Base(source), "legacy-"); legacy != (run == "\n") {
				t.Fatalf("optional run id: %q", run)
			}
			for {
				phase, err := recordCommand("get", "--record", path, "--field", "phase")
				if err != nil {
					t.Fatal(err)
				}
				step, more := next[strings.TrimSpace(phase)]
				if !more {
					break
				}
				if _, err = recordCommand(append([]string{"advance", "--record", path}, step...)...); err != nil {
					t.Fatalf("existing %s record cannot advance: %v", phase, err)
				}
			}
			// Advancing never invents the key a legacy record lacks.
			after, _ := recordCommand("get", "--record", path, "--field", "prepared_run_id")
			if after != run {
				t.Fatalf("run id changed from %q to %q", run, after)
			}
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if run == "\n" && bytes.Contains(written, []byte("prepared_run_id")) {
				t.Fatalf("writer added the optional key: %s", written)
			}
		})
	}
}

// Real production records hold prepared_attempt first. A rewrite reorders the keys
// and changes nothing else, and every consumer ignores key order.
func TestRecordRewriteOfAttemptFirstRecordChangesOnlyKeyOrder(t *testing.T) {
	data, err := os.ReadFile("testdata/indented-attempt-first-submitted.json")
	if err != nil {
		t.Fatal(err)
	}
	if run, attempt := bytes.Index(data, []byte("prepared_run_id")), bytes.Index(data, []byte("prepared_attempt")); run < 0 || attempt < 0 || attempt > run {
		t.Fatalf("fixture lost the production shape: %s", data)
	}
	path := writeRecordFile(t, data)
	if _, err := recordCommand("advance", "--record", path, "--to", "accepted"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	want["phase"] = "accepted"
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("rewrite changed more than key order:\n%v\n%v", want, got)
	}
}

func TestRecordWritersKeepTheExistingSerialization(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	path := filepath.Join(t.TempDir(), "app.json")
	if _, err := recordCommand("create", "--record", path, "--stage", "app", "--run", "40297953014", "--attempt", "2", "--sha256", recordHash); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		want string
		args []string
	}{
		{"jq-prepared", nil},
		{"jq-uploading", []string{"--to", "uploading"}},
		{"jq-submitted", []string{"--to", "submitted", "--id", recordID}},
		{"jq-accepted", []string{"--to", "accepted"}},
		{"jq-finished", []string{"--to", "finished", "--completed-sha256", recordCompleted}},
	} {
		if step.args != nil {
			if _, err := recordCommand(append([]string{"advance", "--record", path}, step.args...)...); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, read(step.want)) {
			t.Fatalf("%s bytes changed:\n%s", step.want, got)
		}
	}
}

func TestRecordGetPrintsOneFieldAndRefusesUnknownFields(t *testing.T) {
	path := writeRecordFile(t, recordJSON(t, func(f map[string]any) {
		f["phase"], f["submission_id"], f["completed_sha256"] = "finished", recordID, recordCompleted
	}))
	for field, want := range map[string]string{"stage": "app", "phase": "finished", "prepared_attempt": "1", "prepared_run_id": "9", "archive_sha256": recordHash, "submission_id": recordID, "completed_sha256": recordCompleted} {
		if got, err := recordCommand("get", "--record", path, "--field", field); err != nil || got != want+"\n" {
			t.Errorf("%s: %q, %v", field, got, err)
		}
	}
	if got, err := recordCommand("get", "--record", path, "--stage", "app", "--field", "phase,submission_id,completed_sha256"); err != nil || got != "finished,"+recordID+","+recordCompleted+"\n" {
		t.Errorf("several fields: %q, %v", got, err)
	}
	if got, err := recordCommand("get", "--record", path, "--stage", "dmg", "--field", "phase"); got != "" {
		t.Errorf("wrong stage printed %q", got)
	} else {
		requireError(t, err, "invalid_record")
	}
	for _, field := range []string{"", "unknown", "Phase", ".phase", "phase,unknown", "phase,"} {
		got, err := recordCommand("get", "--record", path, "--field", field)
		requireError(t, err, "invalid_arguments")
		if got != "" {
			t.Errorf("%q printed %q", field, got)
		}
	}
	empty := writeRecordFile(t, recordJSON(t, nil))
	if got, err := recordCommand("get", "--record", empty, "--field", "submission_id"); err != nil || got != "\n" {
		t.Fatalf("empty field must print empty and succeed like jq -er: %q, %v", got, err)
	}
	if got, err := recordCommand("get", "--record", empty, "--field", "phase,submission_id,completed_sha256"); err != nil || got != "prepared,,\n" {
		t.Fatalf("empty fields must keep their positions: %q, %v", got, err)
	}
}

func TestRecordCreateIsPrivateValidatedAndNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	args := []string{"create", "--record", path, "--stage", "app", "--run", "9", "--attempt", "1", "--sha256", recordHash}
	if _, err := recordCommand(args...); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("record mode: %v, %v", info, err)
	}
	before, _ := os.ReadFile(path)
	var fields map[string]any
	if err := json.Unmarshal(before, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"stage": "app", "phase": "prepared", "prepared_run_id": "9", "prepared_attempt": "1", "archive_sha256": recordHash, "submission_id": "", "completed_sha256": ""}
	if len(fields) != len(want) {
		t.Fatalf("created keys: %v", fields)
	}
	for key, value := range want {
		if fields[key] != value {
			t.Fatalf("%s: %v", key, fields[key])
		}
	}
	_, err = recordCommand(args...)
	requireError(t, err, "record_exists")
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("create replaced an existing record")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("create left temporary files: %v %v", entries, err)
	}
	for name, replace := range map[string][2]string{"stage": {"app", "payload"}, "attempt": {"1", "0"}, "run": {"9", "abc"}, "hash": {recordHash, "abc"}} {
		other := append([]string{}, args...)
		other[2] = filepath.Join(dir, name+".json")
		for i := range other {
			if i > 2 && other[i] == replace[0] {
				other[i] = replace[1]
			}
		}
		_, err := recordCommand(other...)
		requireError(t, err, "invalid_record")
		if _, statErr := os.Stat(other[2]); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("invalid %s was written", name)
		}
	}
	// A failed existence check fails closed without claiming the record exists.
	_, err = recordCommand("create", "--record", filepath.Join(dir, strings.Repeat("x", 300)), "--stage", "app", "--run", "9", "--attempt", "1", "--sha256", recordHash)
	requireError(t, err, "invalid_record")
	_, err = recordCommand("create", "--record", filepath.Join(dir, "norun.json"), "--stage", "app", "--attempt", "1", "--sha256", recordHash)
	requireError(t, err, "invalid_arguments")
}

func TestRecordWritesRespectTheDirectoryLockAndStayAtomic(t *testing.T) {
	path := writeRecordFile(t, recordJSON(t, nil))
	_, unlock, err := lockRecordDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = recordCommand("advance", "--record", path, "--to", "uploading")
	requireError(t, err, "record_locked")
	unlock()
	if _, err = recordCommand("advance", "--record", path, "--to", "uploading"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("advance left temporary files: %v %v", entries, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("record mode: %v, %v", info, err)
	}
	// Reads do not take the lock, like the jq reads they replace.
	_, unlock, err = lockRecordDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err = recordCommand("get", "--record", path, "--field", "phase"); err != nil {
		t.Fatal(err)
	}
}

// An fsync failure after the rename reports failure with the ID already durable.
func TestSubmitWriteFailureAfterRenameKeepsSubmissionAndRefusesAnother(t *testing.T) {
	f := makeFixture(t)
	posts := 0
	c := f.client(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return response(req, http.StatusOK, `{"data":[]}`), nil
		}
		posts++
		return response(req, http.StatusOK, `{"data":{"id":"`+testID+`","attributes":{}}}`), nil
	})
	saves := 0
	c.persist = func(dir *os.File, name string, data []byte) error {
		saves++
		if err := atomicWrite(dir, name, data); err != nil || saves != 2 {
			return err
		}
		return errors.New("synthetic directory sync failure")
	}
	_, err := f.command(context.Background(), f.submitArgs(), c)
	requireError(t, err, "record_persistence_failed")
	if r := f.readRecord(t); r.Phase != "submitted" || r.SubmissionID != testID {
		t.Fatalf("record: %+v", r)
	}
	_, err = f.command(context.Background(), f.submitArgs(), c)
	requireError(t, err, "submission_refused")
	if posts != 1 {
		t.Fatalf("created %d submissions", posts)
	}
}

func TestRecordCommandNeedsNoCredentialsAndHidesPaths(t *testing.T) {
	path := writeRecordFile(t, []byte(`{}`))
	var stdout bytes.Buffer
	getenv := func(string) string { t.Fatal("record commands must not read credentials"); return "" }
	factory := func(*credentials) *client { t.Fatal("record commands must not build a client"); return nil }
	err := run(t.Context(), []string{"record", "validate", "--record", path, "--stage", "app"}, &stdout, getenv, factory)
	requireError(t, err, "invalid_record")
	if strings.Contains(err.Error(), filepath.Dir(path)) || stdout.Len() != 0 {
		t.Fatalf("output exposed a private path: %v %q", err, stdout.String())
	}
	for _, args := range [][]string{{}, {"unknown"}, {"validate", "--record", path}, {"validate", "--stage", "app"}, {"get", "--record", path, "extra", "--field", "phase"}, {"advance", "--record", path, "--unknown", "x"}} {
		_, err := recordCommand(args...)
		requireError(t, err, "invalid_arguments")
	}
}

// recoveryProjectionDisagreements runs the rules distribution-recover.sh restates
// through real jq and through valid over the same records. Values with a trailing
// newline are left out: jq's $ accepts them and that difference is deliberate.
func recoveryProjectionDisagreements(t *testing.T, script string, valid func(stageRecord) bool) []string {
	t.Helper()
	hash := regexp.MustCompile(`def hash: test\("[^"]+"\);`).FindString(script)
	enum := regexp.MustCompile(`\(\.phase == "[a-z]+"( or \.phase == "[a-z]+")+\)`).FindString(script)
	rule := regexp.MustCompile(`(?s)\(\.archive_sha256\|hash\) and \(if .*? end\)`).FindString(script)
	if hash == "" || enum == "" || rule == "" {
		t.Fatalf("recovery rules moved: hash=%q enum=%q rule=%q", hash, enum, rule)
	}
	phases := map[string]bool{phasePrepared: true, "rejected": true}
	for phase := range previousPhase {
		phases[phase] = true
	}
	for _, match := range regexp.MustCompile(`"([a-z]+)"`).FindAllStringSubmatch(enum, -1) {
		phases[match[1]] = true
	}
	var records []stageRecord
	for phase := range phases {
		for _, id := range []string{"", recordID, strings.ToLower(recordID), "ABCDEF01-ABCD-ABCD-ABCD-ABCDEFABCDEF", recordID[:35], recordID + "0", strings.ReplaceAll(recordID, "-", "_")} {
			for _, archive := range []string{recordHash, recordHash[:40], recordHash[:63], recordHash + "a", strings.ToUpper(recordHash), ""} {
				record := stageRecord{Stage: "app", Phase: phase, PreparedAttempt: "1", ArchiveSHA256: archive, SubmissionID: id}
				if phase == phaseFinished {
					record.CompletedSHA256 = recordCompleted
				}
				records = append(records, record)
			}
		}
	}
	input, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("jq", "-c", hash+" map("+enum+" and "+rule+")")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run recovery rules through jq: %v", err)
	}
	var accepted []bool
	if err := json.Unmarshal(output, &accepted); err != nil || len(accepted) != len(records) {
		t.Fatalf("jq output: %v: %s", err, output)
	}
	var disagreements []string
	for i, record := range records {
		if accepted[i] != valid(record) {
			disagreements = append(disagreements, fmt.Sprintf("jq=%t %+v", accepted[i], record))
		}
	}
	return disagreements
}

// shortcut: distribution-recovery.yml has no Go toolchain, so its recovery.json
// jq restates these rules. This test catches drift; a Go step there would end it.
func TestRecoveryProjectionMatchesRecordRules(t *testing.T) {
	data, err := os.ReadFile("../../scripts/distribution-recover.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if found := recoveryProjectionDisagreements(t, script, stageRecord.valid); len(found) != 0 {
		t.Fatalf("recovery and the record module disagree:\n%s", strings.Join(found, "\n"))
	}
	// Each drift below must be seen, whichever side it starts on.
	replace := func(old, new string) string {
		t.Helper()
		if !strings.Contains(script, old) {
			t.Fatalf("mutation target moved: %s", old)
		}
		return strings.Replace(script, old, new, 1)
	}
	for name, test := range map[string]struct {
		script string
		valid  func(stageRecord) bool
	}{
		"go accepts uploading with an id": {script, func(r stageRecord) bool {
			if r.Phase == phaseUploading {
				r.SubmissionID = ""
			}
			return r.valid()
		}},
		"go requires no id once submitted": {script, func(r stageRecord) bool {
			if r.Phase == phaseSubmitted {
				r.SubmissionID = recordID
			}
			return r.valid()
		}},
		"go accepts a short hash": {script, func(r stageRecord) bool {
			if len(r.ArchiveSHA256) == 40 {
				r.ArchiveSHA256 = recordHash
			}
			return r.valid()
		}},
		"go gains a phase": {script, func(r stageRecord) bool {
			if r.Phase == "rejected" {
				r.Phase = phaseSubmitted
			}
			return r.valid()
		}},
		"recovery hash is 40 characters":       {replace(`def hash: test("^[a-f0-9]{64}$");`, `def hash: test("^[a-f0-9]{40}$");`), stageRecord.valid},
		"recovery hash accepts uppercase":      {replace(`def hash: test("^[a-f0-9]{64}$");`, `def hash: test("^[a-fA-F0-9]{64}$");`), stageRecord.valid},
		"recovery allows uploading with an id": {replace(`if .phase == "prepared" or .phase == "uploading" then .submission_id == "" else`, `if .phase == "prepared" then .submission_id == "" else`), stageRecord.valid},
		"recovery id format narrows":           {replace(`"^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-`, `"^[a-f0-9]{8}-[a-fA-F0-9]{4}-`), stageRecord.valid},
		"recovery drops a phase":               {replace(`.phase == "submitted" or .phase == "accepted"`, `.phase == "accepted"`), stageRecord.valid},
	} {
		t.Run(name, func(t *testing.T) {
			if len(recoveryProjectionDisagreements(t, test.script, test.valid)) == 0 {
				t.Fatal("drift went unnoticed")
			}
		})
	}
}
