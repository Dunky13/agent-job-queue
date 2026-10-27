package backend

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/appservices"
	"github.com/google/uuid"
)

// The installer retains a whole signed runtime, rather than copying worker
// executables into support state. Only later installer transactions reclaim it.
type runtimeGenerationAttempt struct {
	ID      int                  `json:"id"`
	Token   string               `json:"token"`
	Attempt string               `json:"attempt"`
	Worker  Object               `json:"worker"`
	Caller  *queueHandoverDaemon `json:"caller"`
}

type managedRuntimeGeneration struct {
	SchemaVersion int                        `json:"schemaVersion"`
	App           string                     `json:"app"`
	Path          string                     `json:"path"`
	Phase         string                     `json:"phase"`
	Runtime       managedBundleIdentity      `json:"runtime"`
	Candidate     managedBundleIdentity      `json:"candidate"`
	Attempts      []runtimeGenerationAttempt `json:"attempts"`
}

func runtimeGenerationName(identity managedBundleIdentity) string {
	// Equal signed bytes do not confer inode custody. Include every filesystem
	// identity component so identical reinstalls cannot alias another generation.
	return fmt.Sprintf("%s-%x-%x", hex.EncodeToString(identity.Digest[:]), identity.Device, identity.Inode)
}

func (install *installer) runtimeGenerationDirectory() string {
	return filepath.Join(install.home, "Applications/Agent Queue Runtimes")
}

func (install *installer) runtimeGenerationRecords() string {
	return filepath.Join(install.root, "runtime-generations")
}

func (install *installer) runtimeGenerationPath(identity managedBundleIdentity) string {
	return filepath.Join(install.runtimeGenerationDirectory(), runtimeGenerationName(identity)+".app")
}

func (install *installer) runtimeGenerationRecordPath(identity managedBundleIdentity) string {
	return filepath.Join(install.runtimeGenerationRecords(), runtimeGenerationName(identity)+".json")
}

func validateRuntimeGenerationAttempt(attempt runtimeGenerationAttempt) error {
	if attempt.ID < 0 || !validToken(attempt.Token) || attempt.Caller == nil {
		return errors.New("Invalid retained runtime attempt identity")
	}
	if err := validateQueueHandoverIdentity(*attempt.Caller); err != nil {
		return fmt.Errorf("Invalid retained runtime caller identity: %w", err)
	}
	if attempt.Worker == nil {
		if attempt.Attempt != "" && !validToken(attempt.Attempt) {
			return errors.New("Invalid retained runtime outcome attempt identity")
		}
		return nil // Queued callers and failed-to-spawn outcomes still need code.
	}
	if !validToken(attempt.Attempt) || len(attempt.Worker) != 4 {
		return errors.New("Invalid retained runtime worker attempt identity")
	}
	pgid, groupOK := integer(attempt.Worker["pgid"])
	pid, pidOK := integer(attempt.Worker["pid"])
	boot, err := uuid.Parse(text(attempt.Worker["boot_uuid"]))
	if !groupOK || !pidOK || pgid <= 1 || pid != pgid || err != nil || boot.String() != text(attempt.Worker["boot_uuid"]) {
		return errors.New("Invalid retained runtime process identity")
	}
	members := list(attempt.Worker["members"])
	seen, leader := map[int]bool{}, false
	for _, raw := range members {
		member := obj(raw)
		id, valid := integer(member["pid"])
		if len(member) != 2 || !valid || id <= 1 || text(member["birth"]) == "" || seen[id] {
			return errors.New("Invalid retained runtime process birth identities")
		}
		seen[id] = true
		leader = leader || id == pid
	}
	if !leader {
		return errors.New("Retained runtime has no worker leader birth identity")
	}
	return nil
}

func validateRuntimeGenerationAttemptFields(raw []byte) error {
	attempt, err := metadataFields(raw, "id", "token", "attempt", "worker", "caller")
	if err != nil {
		return err
	}
	if _, err := metadataFields(attempt["caller"], "pid", "pgid", "bootUUID", "birth"); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(attempt["worker"]), []byte("null")) {
		return nil
	}
	worker, err := metadataFields(attempt["worker"], "pid", "pgid", "boot_uuid", "members")
	if err != nil {
		return err
	}
	var members []json.RawMessage
	if err := json.Unmarshal(worker["members"], &members); err != nil {
		return err
	}
	for _, member := range members {
		if _, err := metadataFields(member, "pid", "birth"); err != nil {
			return err
		}
	}
	return nil
}

func (install *installer) readRuntimeGeneration(filename string) (*managedRuntimeGeneration, []byte, error) {
	raw, err := appservices.ReadFile(install.home, filename, 4*1024*1024, false)
	if err != nil {
		return nil, nil, err
	}
	fields, err := metadataFields(raw, "schemaVersion", "app", "path", "phase", "runtime", "candidate", "attempts")
	if err != nil {
		return nil, nil, err
	}
	for _, key := range []string{"runtime", "candidate"} {
		bundle, err := metadataFields(fields[key], "device", "inode", "digest", "build")
		if err != nil {
			return nil, nil, err
		}
		if _, err := ValidateBuildInfo(bundle["build"]); err != nil {
			return nil, nil, err
		}
		var digest []json.RawMessage
		if err := json.Unmarshal(bundle["digest"], &digest); err != nil || len(digest) != 32 {
			return nil, nil, errors.New("Invalid retained runtime byte digest")
		}
	}
	var attempts []json.RawMessage
	if err := json.Unmarshal(fields["attempts"], &attempts); err != nil {
		return nil, nil, err
	}
	for _, rawAttempt := range attempts {
		if err := validateRuntimeGenerationAttemptFields(rawAttempt); err != nil {
			return nil, nil, err
		}
	}
	var record managedRuntimeGeneration
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil {
		return nil, nil, err
	}
	if record.SchemaVersion != 1 || record.App != install.app || record.Path != install.runtimeGenerationPath(record.Runtime) || filename != install.runtimeGenerationRecordPath(record.Runtime) || record.Runtime.Inode == 0 || record.Runtime.Digest == [32]byte{} || record.Candidate.Inode == 0 || record.Candidate.Digest == [32]byte{} || len(record.Attempts) == 0 || len(record.Attempts) > 4096 || (record.Phase != "transfer" && record.Phase != "retained" && record.Phase != "retiring") {
		return nil, nil, errors.New("Retained runtime custody belongs to another app, generation, or operation")
	}
	seen := map[int]bool{}
	for _, attempt := range record.Attempts {
		if err := validateRuntimeGenerationAttempt(attempt); err != nil {
			return nil, nil, err
		}
		if seen[attempt.ID] {
			return nil, nil, errors.New("Duplicate retained runtime job identity")
		}
		seen[attempt.ID] = true
	}
	return &record, raw, nil
}

func (install *installer) replaceRuntimeGeneration(record *managedRuntimeGeneration, expected []byte) error {
	filename := install.runtimeGenerationRecordPath(record.Runtime)
	current, err := appservices.ReadFile(install.home, filename, 4*1024*1024, false)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return errors.New("Retained runtime receipt changed; evidence preserved")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return installAtomic(filename, append(raw, '\n'), 0600)
}

// Called only after candidate readiness under the installer's operation locks.
// A transfer receipt precedes the rename so either filesystem location can be
// reconciled after interruption without adopting a bundle by name or digest.
func (install *installer) retainManagedRuntime(ctx context.Context, attempts []runtimeGenerationAttempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, attempt := range attempts {
		if err := validateRuntimeGenerationAttempt(attempt); err != nil {
			return err
		}
		if seen[attempt.ID] {
			return errors.New("Duplicate retained runtime job identity")
		}
		seen[attempt.ID] = true
	}
	if err := install.managedBackupAccess(); err != nil {
		return err
	}
	publication, _, err := install.managedReadReceipt()
	if err != nil || publication == nil || publication.Backup == nil {
		return err
	}
	if err := managedRequireBundle(install.app, publication.Candidate); err != nil {
		return err
	}
	identity := *publication.Backup
	filename := install.runtimeGenerationRecordPath(identity)
	record, raw, err := install.readRuntimeGeneration(filename)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if record == nil && len(attempts) == 0 {
		return nil // A drained replacement uses the normal publication cleanup.
	}
	if record != nil {
		if !record.Runtime.equal(identity) || !record.Candidate.equal(publication.Candidate) || record.Phase == "retiring" {
			return errors.New("Retained runtime receipt differs from publication custody")
		}
		for _, attempt := range attempts {
			if !slicesContainRuntimeAttempt(record.Attempts, attempt) {
				return errors.New("Retained runtime attempt changed; evidence preserved")
			}
		}
	}
	previous, err := updateExists(install.managedPreviousPath())
	if err != nil {
		return err
	}
	target := install.runtimeGenerationPath(identity)
	present, err := updateExists(target)
	if err != nil {
		return err
	}
	if previous == present || present && record == nil || previous && record != nil && record.Phase != "transfer" {
		return errors.New("Retained runtime transfer has missing, duplicate, or unowned bundles")
	}
	if present {
		if err := managedRequireBundle(target, identity); err != nil {
			return err
		}
		if record.Phase == "retained" {
			return nil
		}
	} else {
		if err := managedRequireBundle(install.managedPreviousPath(), identity); err != nil {
			return err
		}
		if err := install.managedVerify(ctx, install.managedPreviousPath()); err != nil {
			return err
		}
		if record == nil {
			record = &managedRuntimeGeneration{SchemaVersion: 1, App: install.app, Path: target, Phase: "transfer", Runtime: identity, Candidate: publication.Candidate, Attempts: attempts}
			raw, err = json.Marshal(record)
			if err != nil {
				return err
			}
			raw = append(raw, '\n')
			parent, err := integrationParent(filename, true)
			if err != nil {
				return err
			}
			err = integrationPrivateWrite(parent, filepath.Base(filename), raw)
			if err == nil {
				err = parent.Sync()
			}
			parent.Close()
			if err != nil {
				return err
			}
			// Parse the actual persisted record before transferring executable
			// custody, including the attempt and process-birth shape checks.
			record, raw, err = install.readRuntimeGeneration(filename)
			if err != nil {
				return err
			}
		}
		from, err := install.managedPublicationParent(true)
		if err != nil {
			return err
		}
		defer from.Close()
		to, err := integrationParent(target, true)
		if err != nil {
			return err
		}
		defer to.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := managedRequireBundle(install.app, publication.Candidate); err != nil {
			return err
		}
		if err := managedRequireBundle(install.managedPreviousPath(), identity); err != nil {
			return err
		}
		if err := runtimeGenerationRenameNoReplace(int(from.Fd()), filepath.Base(install.managedPreviousPath()), int(to.Fd()), filepath.Base(target)); err != nil {
			return fmt.Errorf("Retain signed worker runtime: %w", err)
		}
		if err := errors.Join(from.Sync(), to.Sync()); err != nil {
			return err
		}
		if err := managedRequireBundle(target, identity); err != nil {
			return err
		}
	}
	record.Phase = "retained"
	return install.replaceRuntimeGeneration(record, raw)
}

func slicesContainRuntimeAttempt(attempts []runtimeGenerationAttempt, expected runtimeGenerationAttempt) bool {
	for _, actual := range attempts {
		if actual.ID == expected.ID && actual.Token == expected.Token && actual.Attempt == expected.Attempt && jsonEqual(actual.Worker, expected.Worker) && jsonEqual(actual.Caller, expected.Caller) {
			return true
		}
	}
	return false
}

// Cleanup is deliberately installer-only. Runtime processes cannot delete code
// after installation, and live/ambiguous generations remain retained.
func (install *installer) cleanupManagedRuntimes(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := integrationParent(filepath.Join(install.runtimeGenerationRecords(), ".inspect"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	type retained struct {
		record *managedRuntimeGeneration
		raw    []byte
	}
	records := []retained{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(entry.Name(), ".json") || strings.ContainsAny(entry.Name(), "/\\") {
			return errors.New("Unknown retained runtime receipt; evidence preserved")
		}
		record, raw, err := install.readRuntimeGeneration(filepath.Join(install.runtimeGenerationRecords(), entry.Name()))
		if err != nil {
			return err
		}
		records = append(records, retained{record, raw})
	}
	publication, _, err := install.managedReadReceipt()
	if err != nil {
		return err
	}
	for _, retained := range records {
		record, raw := retained.record, retained.raw
		if err := ctx.Err(); err != nil {
			return err
		}
		if publication != nil && publication.Backup != nil && publication.Backup.equal(record.Runtime) {
			continue // This generation is still the pending publication's backup.
		}
		present, err := updateExists(record.Path)
		if err != nil {
			return err
		}
		if record.Phase == "transfer" {
			return errors.New("Retained runtime transfer has no matching pending publication; evidence preserved")
		}
		if !present {
			if record.Phase != "retiring" {
				return errors.New("Retained runtime disappeared without cleanup custody")
			}
			if err := install.managedRemoveFile(install.runtimeGenerationRecordPath(record.Runtime), raw); err != nil {
				return err
			}
			continue
		}
		if err := managedRequireBundle(record.Path, record.Runtime); err != nil {
			return err
		}
		finished, err := install.runtimeGenerationFinished(ctx, record.Attempts)
		if err != nil {
			return err
		}
		if !finished {
			continue
		}
		if err := install.managedVerify(ctx, record.Path); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		record.Phase = "retiring"
		if err := install.replaceRuntimeGeneration(record, raw); err != nil {
			return err
		}
		expected, err := json.Marshal(record)
		if err != nil {
			return err
		}
		expected = append(expected, '\n')
		_, raw, err = install.readRuntimeGeneration(install.runtimeGenerationRecordPath(record.Runtime))
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, expected) {
			return errors.New("Retained runtime retirement receipt changed before code cleanup")
		}
		if err := managedRequireBundle(record.Path, record.Runtime); err != nil {
			return err
		}
		directory, err := integrationParent(record.Path, false)
		if err != nil {
			return err
		}
		root, err := os.OpenRoot(install.runtimeGenerationDirectory())
		if err != nil {
			directory.Close()
			return err
		}
		held, heldErr := directory.Stat()
		named, namedErr := root.Stat(".")
		if heldErr != nil || namedErr != nil || !os.SameFile(held, named) {
			root.Close()
			directory.Close()
			return errors.Join(errors.New("Retained runtime directory changed before cleanup"), heldErr, namedErr)
		}
		err = root.RemoveAll(filepath.Base(record.Path))
		root.Close()
		if err == nil {
			err = directory.Sync()
		}
		directory.Close()
		if err != nil {
			return err
		}
		if err := install.managedRemoveFile(install.runtimeGenerationRecordPath(record.Runtime), raw); err != nil {
			return err
		}
	}
	return nil
}

func (install *installer) runtimeGenerationFinished(ctx context.Context, attempts []runtimeGenerationAttempt) (bool, error) {
	store, err := openQueueStore(install.state, false)
	if err != nil {
		return false, err
	}
	defer store.close()
	for _, attempt := range attempts {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := validateRuntimeGenerationAttempt(attempt); err != nil {
			return false, err
		}
		job, err := store.job(attempt.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // Missing history does not establish a terminal attempt.
		}
		if err != nil {
			return false, err
		}
		if job.token != attempt.Token || job.phase != "done" {
			return false, nil
		}
		if attempt.Attempt != "" && job.attempt != attempt.Attempt {
			return false, nil
		}
		if attempt.Worker != nil && !jsonEqual(job.worker, attempt.Worker) {
			return false, nil
		}
		if err := validateQueueHandoverJobState(job); err != nil {
			return false, fmt.Errorf("Retained runtime terminal task state is invalid: %w", err)
		}
		boot, err := BootIdentity()
		if err != nil {
			return false, err
		}
		if boot == attempt.Caller.BootUUID {
			alive, err := queueHandoverDaemonAlive(*attempt.Caller)
			if err != nil {
				fmt.Fprintln(os.Stdout, "Retaining an older signed runtime because caller liveness remains unresolved:", err)
				return false, nil
			}
			if alive {
				return false, nil
			}
		}
		if attempt.Worker != nil && boot == text(attempt.Worker["boot_uuid"]) {
			owned, err := VerifyGroupIdentity(attempt.Worker)
			if err != nil {
				fmt.Fprintln(os.Stdout, "Retaining an older signed runtime because worker liveness remains unresolved:", err)
				return false, nil
			}
			if owned {
				return false, nil
			}
		}
	}
	return true, nil
}
