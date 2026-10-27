package backend

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const queueHandoverProtocol = 1
const queueHandoverSchema = 2
const queueHandoverPrepareLifetime = 2 * time.Minute
const queueHandoverReleaseLifetime = 5 * time.Minute

var errQueueTransportUnavailable = errors.New("Queue daemon temporarily unavailable")
var errQueueHandoverBusy = errors.New("Queue handover is in progress; no new mutation was accepted")

func queueTransportError(err error) error {
	var network net.Error
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network) && network.Timeout() {
		return errors.Join(errQueueTransportUnavailable, err)
	}
	return err
}

type queueHandoverDaemon struct {
	PID      int    `json:"pid"`
	PGID     int    `json:"pgid"`
	BootUUID string `json:"bootUUID"`
	Birth    string `json:"birth"`
}

type queueHandoverAttempt struct {
	ID      int                  `json:"id"`
	Token   string               `json:"token"`
	Attempt string               `json:"attempt"`
	Phase   string               `json:"phase"`
	Worker  Object               `json:"worker"`
	Caller  *queueHandoverDaemon `json:"caller"`
}

type queueHandoverStatus struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Protocol      int                    `json:"protocol"`
	QueueSchema   int                    `json:"queueSchema"`
	Daemon        queueHandoverDaemon    `json:"daemon"`
	Eligible      bool                   `json:"eligible"`
	Reason        string                 `json:"reason"`
	Attempts      []queueHandoverAttempt `json:"attempts"`
	State         string                 `json:"state"`
	Token         string                 `json:"token,omitempty"`
	TargetSHA256  string                 `json:"targetSHA256,omitempty"`
	ExpiresAt     string                 `json:"expiresAt,omitempty"`
}

type queueHandoverReceipt struct {
	SchemaVersion  int                    `json:"schemaVersion"`
	Protocol       int                    `json:"protocol"`
	QueueSchema    int                    `json:"queueSchema"`
	Token          string                 `json:"token"`
	TargetSHA256   string                 `json:"targetSHA256"`
	Phase          string                 `json:"phase"`
	ExpiresAt      string                 `json:"expiresAt"`
	DatabaseDevice uint64                 `json:"databaseDevice"`
	DatabaseInode  uint64                 `json:"databaseInode"`
	Daemon         queueHandoverDaemon    `json:"daemon"`
	Attempts       []queueHandoverAttempt `json:"attempts"`
	QueueStatus    string                 `json:"queueStatus"`
	ParallelLimit  int                    `json:"parallelLimit"`
	NextID         int                    `json:"nextID"`
	Consumer       *queueHandoverDaemon   `json:"consumer,omitempty"`
}

func queueHandoverObject(value any) (Object, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}
func queueHandoverDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && value == strings.ToLower(value)
}
func queueExecutableSHA256() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func queueHandoverIdentity() (queueHandoverDaemon, error) {
	result := queueHandoverDaemon{PID: os.Getpid(), PGID: syscall.Getpgrp()}
	boot, err := BootIdentity()
	if err != nil {
		return result, err
	}
	result.BootUUID = boot
	members, err := ProcessGroupMembers(result.PGID)
	if err != nil {
		return result, err
	}
	for _, member := range members {
		if pid, ok := integer(member["pid"]); ok && pid == result.PID {
			result.Birth = text(member["birth"])
			break
		}
	}
	if result.PID <= 1 || result.PGID <= 1 || result.Birth == "" {
		return result, errors.New("Cannot establish daemon handover identity")
	}
	return result, nil
}
func validateQueueHandoverIdentity(identity queueHandoverDaemon) error {
	id, err := uuid.Parse(identity.BootUUID)
	if identity.PID <= 1 || identity.PGID <= 1 || strings.TrimSpace(identity.Birth) == "" || err != nil || id.String() != identity.BootUUID {
		return errors.New("Invalid queue handover process identity")
	}
	return nil
}
func decodeQueueHandoverIdentity(value any) (*queueHandoverDaemon, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var identity queueHandoverDaemon
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return nil, err
	}
	if err := validateQueueHandoverIdentity(identity); err != nil {
		return nil, err
	}
	return &identity, nil
}

type queueHandoverProcess struct {
	PGID         int
	Birth, State string
}
type queueHandoverProcesses struct {
	Boot      string
	Processes map[int]queueHandoverProcess
}

func queueHandoverProcessSnapshot() (queueHandoverProcesses, error) {
	result := queueHandoverProcesses{Processes: map[int]queueHandoverProcess{}}
	boot, err := BootIdentity()
	if err != nil {
		return result, err
	}
	result.Boot = boot
	output, err := identityCommand("/bin/ps", "-axo", "pid=,pgid=,stat=,lstart=")
	if err != nil {
		return result, err
	}
	observer := false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return result, errors.New("Incomplete handover process snapshot")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		pgid, pgidErr := strconv.Atoi(fields[1])
		if pidErr != nil || pgidErr != nil || pid <= 0 || pgid < 0 {
			return result, errors.New("Invalid handover process snapshot")
		}
		state := queueHandoverProcess{PGID: pgid, Birth: strings.Join(fields[3:], " "), State: fields[2]}
		if _, duplicate := result.Processes[pid]; duplicate {
			return result, errors.New("Ambiguous handover process snapshot")
		}
		result.Processes[pid] = state
		observer = observer || pid == os.Getpid() && pgid == syscall.Getpgrp() && !strings.HasPrefix(state.State, "Z")
	}
	if !observer {
		return result, errors.New("Incomplete handover process snapshot")
	}
	return result, nil
}
func (snapshot queueHandoverProcesses) alive(identity queueHandoverDaemon) (bool, error) {
	if err := validateQueueHandoverIdentity(identity); err != nil {
		return false, err
	}
	if snapshot.Boot != identity.BootUUID {
		return false, errors.New("Queue handover belongs to another boot")
	}
	state, present := snapshot.Processes[identity.PID]
	if !present {
		return false, nil
	}
	if state.Birth != strings.Join(strings.Fields(identity.Birth), " ") {
		return false, errors.New("Queue handover process identity was reused")
	}
	if state.PGID != identity.PGID {
		return false, errors.New("Queue handover process left its recorded process group")
	}
	return !strings.HasPrefix(state.State, "Z"), nil
}
func queueHandoverDaemonAlive(identity queueHandoverDaemon) (bool, error) {
	snapshot, err := queueHandoverProcessSnapshot()
	if err != nil {
		return false, err
	}
	return snapshot.alive(identity)
}
func queueHandoverDatabase(g *Gate) (uint64, uint64, error) {
	path := QueueDatabasePath(g.State)
	if err := queuePrivateFiles(path); err != nil {
		return 0, 0, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !fileOwned(info, true) || stat.Nlink != 1 {
		return 0, 0, errors.New("Unsafe handover database identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
func queueHandoverPath(g *Gate) string { return filepath.Join(g.State, "run", "handover.json") }
func validateQueueHandoverShape(receipt queueHandoverReceipt) error {
	if receipt.SchemaVersion != 1 || receipt.Protocol != queueHandoverProtocol || receipt.QueueSchema != queueHandoverSchema || !validToken(receipt.Token) || !queueHandoverDigest(receipt.TargetSHA256) || receipt.DatabaseInode == 0 || validateQueueHandoverIdentity(receipt.Daemon) != nil || receipt.Attempts == nil || ValidateLimit(receipt.ParallelLimit) != nil || receipt.NextID < 0 || (receipt.QueueStatus != "Running" && receipt.QueueStatus != "Paused") {
		return errors.New("Invalid queue handover custody")
	}
	if _, err := time.Parse(time.RFC3339Nano, receipt.ExpiresAt); err != nil {
		return errors.New("Invalid queue handover expiry")
	}
	switch receipt.Phase {
	case "prepared", "committed", "released", "consumed", "cancelled", "expired":
	default:
		return errors.New("Invalid queue handover phase")
	}
	seen := map[int]bool{}
	for _, attempt := range receipt.Attempts {
		if attempt.ID < 0 || seen[attempt.ID] || !validToken(attempt.Token) || attempt.Caller == nil || validateQueueHandoverIdentity(*attempt.Caller) != nil || attempt.Caller.BootUUID != receipt.Daemon.BootUUID || (attempt.Phase != "queued" && attempt.Phase != "running" && attempt.Phase != "outcome" && attempt.Phase != "done") {
			return errors.New("Invalid queue handover attempt")
		}
		seen[attempt.ID] = true
		if attempt.Phase == "queued" {
			if attempt.Attempt != "" || attempt.Worker != nil {
				return errors.New("Invalid queued handover attempt")
			}
		} else if attempt.Phase == "running" && (attempt.Worker == nil || !validToken(attempt.Attempt)) || (attempt.Phase == "outcome" || attempt.Phase == "done") && (attempt.Attempt != "" && !validToken(attempt.Attempt) || attempt.Attempt == "" && attempt.Worker != nil) {
			return errors.New("Invalid active handover attempt")
		}
	}
	if receipt.Phase == "consumed" && (receipt.Consumer == nil || validateQueueHandoverIdentity(*receipt.Consumer) != nil || receipt.Consumer.BootUUID != receipt.Daemon.BootUUID) {
		return errors.New("Invalid queue handover consumer")
	}
	return nil
}
func readQueueHandoverReceipt(g *Gate) (*queueHandoverReceipt, error) {
	for _, directory := range []string{g.State, filepath.Join(g.State, "run")} {
		info, err := os.Lstat(directory)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() {
			return nil, errors.New("Unsafe queue handover directory")
		}
	}
	path := queueHandoverPath(g)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !fileOwned(info, true) || stat.Nlink != 1 {
		return nil, errors.New("Unsafe queue handover receipt")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 32*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 32*1024*1024 {
		return nil, errors.New("Queue handover receipt exceeds its limit")
	}
	var receipt queueHandoverReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("Trailing queue handover data")
	}
	if err := validateQueueHandoverShape(receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}
func writeQueueHandoverReceipt(g *Gate, receipt *queueHandoverReceipt) error {
	if err := validateQueueHandoverShape(*receipt); err != nil {
		return err
	}
	if _, err := readQueueHandoverReceipt(g); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	value, err := queueHandoverObject(receipt)
	if err != nil {
		return err
	}
	return WritePrivate(queueHandoverPath(g), value)
}
func decodeQueueHandoverStatus(value Object) (queueHandoverStatus, error) {
	var status queueHandoverStatus
	raw, err := json.Marshal(value)
	if err != nil {
		return status, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		return status, err
	}
	if status.SchemaVersion != 1 || status.Protocol != queueHandoverProtocol || status.QueueSchema != queueHandoverSchema || status.Attempts == nil || validateQueueHandoverIdentity(status.Daemon) != nil {
		return status, errors.New("Unsupported live queue handover status")
	}
	return status, nil
}
func queueHandoverAttempts(store *queueStore) ([]queueHandoverAttempt, error) {
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version != queueHandoverSchema {
		return nil, errors.New("Queue schema does not support live handover")
	}
	jobs, err := store.jobs()
	if err != nil {
		return nil, err
	}
	attempts := []queueHandoverAttempt{}
	processes, err := queueHandoverProcessSnapshot()
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		protocol, ok := integer(job.task["handoverProtocol"])
		if job.phase == "done" && (!ok || protocol != queueHandoverProtocol) {
			continue
		}
		if !ok || protocol != queueHandoverProtocol {
			return nil, errors.New("An unfinished job has a legacy caller; live upgrade must wait for that job")
		}
		caller, err := decodeQueueHandoverIdentity(job.task["handoverCaller"])
		if err != nil {
			return nil, errors.New("An execution has no verified original caller identity")
		}
		if job.phase == "done" {
			// Historical process identifiers can legitimately be reused or
			// belong to a previous boot. Those original callers have exited.
			state, present := processes.Processes[caller.PID]
			if caller.BootUUID != processes.Boot || !present || state.Birth != strings.Join(strings.Fields(caller.Birth), " ") || strings.HasPrefix(state.State, "Z") {
				continue
			}
		}
		alive, err := processes.alive(*caller)
		if err != nil {
			return nil, err
		}
		if job.phase == "done" && !alive {
			continue
		}
		if !validToken(job.token) || text(job.task["token"]) != job.token {
			return nil, errors.New("An execution has an invalid handover identity")
		}
		if job.phase == "starting" {
			return nil, errors.New("An execution attempt is still being claimed; retry live upgrade after its claim settles")
		}
		if job.phase != "queued" && job.phase != "running" && job.phase != "outcome" && job.phase != "done" {
			return nil, errors.New("An execution attempt has an unsupported handover phase")
		}
		if job.phase == "running" {
			if job.worker == nil {
				return nil, errors.New("Active worker has no ownership evidence")
			}
			owned, err := VerifyGroupIdentity(job.worker)
			if err != nil {
				return nil, err
			}
			if !owned {
				current, readErr := store.job(job.id)
				if readErr != nil {
					return nil, readErr
				}
				if current.token != job.token || current.attempt != job.attempt || !jsonEqual(current.worker, job.worker) || current.phase != "outcome" && current.phase != "done" {
					return nil, errors.New("Active worker outcome is not yet known; live upgrade must wait for reconciliation")
				}
				job = current
			}
		}
		if err := validateQueueHandoverJobState(job); err != nil {
			return nil, err
		}
		attempts = append(attempts, queueHandoverAttempt{ID: job.id, Token: job.token, Attempt: job.attempt, Phase: job.phase, Worker: job.worker, Caller: caller})
	}
	return attempts, nil
}

func validateQueueHandoverJobState(job queueJob) error {
	name, details, err := TaskStatus(job.task)
	if err != nil {
		return err
	}
	switch job.phase {
	case "queued":
		if job.attempt != "" || job.worker != nil || name != "Queued" && name != "Stashed" {
			return errors.New("Queued handover state is inconsistent")
		}
	case "running":
		if !validToken(job.attempt) || job.worker == nil || name != "Running" && name != "Paused" {
			return errors.New("Running handover state is inconsistent")
		}
	case "outcome", "done":
		code, err := ResultCode(details["result"])
		if name != "Done" || err != nil || !job.outcome.Valid || int64(code) != job.outcome.Int64 {
			return errors.New("Completed handover state is inconsistent")
		}
	default:
		return errors.New("Unsupported handover execution state")
	}
	return nil
}
func (d *queueDaemon) handoverStatus() (queueHandoverStatus, error) {
	identity, err := queueHandoverIdentity()
	if err != nil {
		return queueHandoverStatus{}, err
	}
	attempts, eligibility := queueHandoverAttempts(d.store)
	status := queueHandoverStatus{SchemaVersion: 1, Protocol: queueHandoverProtocol, QueueSchema: queueHandoverSchema, Daemon: identity, Eligible: eligibility == nil, Attempts: attempts, State: "ready"}
	if status.Attempts == nil {
		status.Attempts = []queueHandoverAttempt{}
	}
	if eligibility != nil {
		status.Reason = eligibility.Error()
	}
	if d.handover != nil {
		status.State = d.handover.Phase
		status.Token = d.handover.Token
		status.TargetSHA256 = d.handover.TargetSHA256
		status.ExpiresAt = d.handover.ExpiresAt
	}
	return status, nil
}
func (d *queueDaemon) expireHandover() error {
	if d.handover == nil || d.handover.Phase != "prepared" {
		return nil
	}
	expires, _ := time.Parse(time.RFC3339Nano, d.handover.ExpiresAt)
	if time.Now().Before(expires) {
		return nil
	}
	if err := d.validatePreparedHandover(); err != nil {
		return err
	}
	expired := *d.handover
	expired.Phase = "expired"
	if err := writeQueueHandoverReceipt(d.g, &expired); err != nil {
		return err
	}
	d.handover = nil
	return nil
}

func (d *queueDaemon) validatePreparedHandover() error {
	if d.handover == nil {
		return errors.New("Queue handover custody is unavailable")
	}
	current, err := readQueueHandoverReceipt(d.g)
	if err != nil {
		return err
	}
	if !jsonEqual(current, d.handover) {
		return errors.New("Queue handover custody changed unexpectedly")
	}
	return nil
}
func (d *queueDaemon) handoverControl(operation string, payload Object) (Object, error) {
	if err := d.expireHandover(); err != nil {
		return nil, err
	}
	if operation == "handover-status" {
		if len(payload) != 0 {
			return nil, errors.New("Handover status does not accept arguments")
		}
		status, err := d.handoverStatus()
		if err != nil {
			return nil, err
		}
		return queueHandoverObject(status)
	}
	value, ok := payload["token"].(string)
	if !ok || !validToken(value) {
		return nil, errors.New("A live handover requires its exact token")
	}
	if operation == "prepare-handover" {
		target, ok := payload["targetSHA256"].(string)
		if len(payload) != 2 || !ok || !queueHandoverDigest(target) {
			return nil, errors.New("A live handover requires the target executable SHA-256")
		}
		if d.handover != nil {
			if d.handover.Phase == "prepared" && d.handover.Token == value && d.handover.TargetSHA256 == target {
				status, err := d.handoverStatus()
				if err != nil {
					return nil, err
				}
				return queueHandoverObject(status)
			}
			return nil, errors.New("A different queue handover is already pending")
		}
		if previous, err := readQueueHandoverReceipt(d.g); err == nil {
			if previous.Phase == "committed" || previous.Phase == "released" || previous.Phase == "prepared" {
				return nil, errors.New("Retained queue handover custody requires recovery")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		status, err := d.handoverStatus()
		if err != nil {
			return nil, err
		}
		if !status.Eligible {
			return nil, errors.New(status.Reason)
		}
		settings, err := d.store.settings()
		if err != nil {
			return nil, err
		}
		limit, _ := integer(settings["parallel_tasks"])
		next, _ := integer(settings["next_id"])
		device, inode, err := queueHandoverDatabase(d.g)
		if err != nil {
			return nil, err
		}
		receipt := &queueHandoverReceipt{SchemaVersion: 1, Protocol: queueHandoverProtocol, QueueSchema: queueHandoverSchema, Token: value, TargetSHA256: target, Phase: "prepared", ExpiresAt: time.Now().Add(queueHandoverPrepareLifetime).UTC().Format(time.RFC3339Nano), DatabaseDevice: device, DatabaseInode: inode, Daemon: status.Daemon, Attempts: status.Attempts, QueueStatus: text(settings["status"]), ParallelLimit: limit, NextID: next}
		if err := writeQueueHandoverReceipt(d.g, receipt); err != nil {
			return nil, err
		}
		d.handover = receipt
		if d.handoverStop == nil {
			d.handoverStop = make(chan struct{}, 1)
		}
		status.State = receipt.Phase
		status.Token = value
		status.TargetSHA256 = target
		status.ExpiresAt = receipt.ExpiresAt
		return queueHandoverObject(status)
	}
	if len(payload) != 1 {
		return nil, errors.New("Unexpected queue handover arguments")
	}
	if d.handover == nil {
		previous, err := readQueueHandoverReceipt(d.g)
		if operation == "cancel-handover" && err == nil && previous.Token == value && (previous.Phase == "cancelled" || previous.Phase == "expired") {
			return Object{"schemaVersion": 1, "protocol": queueHandoverProtocol, "token": value, "state": previous.Phase}, nil
		}
		return nil, errors.New("The queue handover is no longer prepared")
	}
	if d.handover.Token != value || d.handover.Phase != "prepared" {
		return nil, errors.New("The queue handover token or phase changed")
	}
	if err := d.validatePreparedHandover(); err != nil {
		return nil, err
	}
	receipt := *d.handover
	switch operation {
	case "cancel-handover":
		receipt.Phase = "cancelled"
		if err := writeQueueHandoverReceipt(d.g, &receipt); err != nil {
			return nil, err
		}
		d.handover = nil
	case "commit-handover":
		if err := validateQueueHandoverAttempts(d.store, d.handover); err != nil {
			return nil, err
		}
		receipt.Phase = "committed"
		receipt.ExpiresAt = time.Now().Add(queueHandoverReleaseLifetime).UTC().Format(time.RFC3339Nano)
		if err := writeQueueHandoverReceipt(d.g, &receipt); err != nil {
			return nil, err
		}
		d.handover = &receipt
		d.handoverStop <- struct{}{}
	default:
		return nil, errors.New("Unknown queue handover operation")
	}
	return Object{"schemaVersion": 1, "protocol": queueHandoverProtocol, "token": value, "state": receipt.Phase}, nil
}
func (d *queueDaemon) releaseHandover() error {
	if d.handover == nil || d.handover.Phase != "committed" {
		return errors.New("The queue handover was not committed")
	}
	if err := d.validatePreparedHandover(); err != nil {
		return err
	}
	if err := validateQueueHandoverAttempts(d.store, d.handover); err != nil {
		return err
	}
	receipt := *d.handover
	receipt.Phase = "released"
	if err := writeQueueHandoverReceipt(d.g, &receipt); err != nil {
		return err
	}
	d.handover = &receipt
	return nil
}

// Read-only SQLite inspection deliberately does not run schema migrations or
// normalize queue settings while workers from the old runtime are still alive.
func queueReadHandoverStore(g *Gate) (*queueStore, error) {
	if _, _, err := queueHandoverDatabase(g); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: QueueDatabasePath(g.State)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(1000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != queueHandoverSchema {
		db.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("Queue schema is incompatible with retained workers")
	}
	return &queueStore{state: g.State, path: QueueDatabasePath(g.State), db: db}, nil
}
func validateQueueHandoverDatabase(g *Gate, receipt *queueHandoverReceipt) error {
	device, inode, err := queueHandoverDatabase(g)
	if err != nil {
		return err
	}
	if device != receipt.DatabaseDevice || inode != receipt.DatabaseInode {
		return errors.New("Queue database changed after handover")
	}
	boot, err := BootIdentity()
	if err != nil {
		return err
	}
	if boot != receipt.Daemon.BootUUID {
		return errors.New("Queue handover belongs to another boot")
	}
	expires, _ := time.Parse(time.RFC3339Nano, receipt.ExpiresAt)
	if (receipt.Phase == "prepared" || receipt.Phase == "committed") && !time.Now().Before(expires) {
		return errors.New("Queue handover custody expired")
	}
	return nil
}
func validateQueueHandoverAttempts(store *queueStore, receipt *queueHandoverReceipt) error {
	settings, err := store.settings()
	if err != nil {
		return err
	}
	limit, _ := integer(settings["parallel_tasks"])
	next, _ := integer(settings["next_id"])
	if text(settings["status"]) != receipt.QueueStatus || limit != receipt.ParallelLimit || next != receipt.NextID {
		return errors.New("Queue settings changed after handover")
	}
	known := map[int]queueHandoverAttempt{}
	for _, attempt := range receipt.Attempts {
		known[attempt.ID] = attempt
		job, err := store.job(attempt.ID)
		if err != nil {
			return err
		}
		protocol, ok := integer(job.task["handoverProtocol"])
		caller, callerErr := decodeQueueHandoverIdentity(job.task["handoverCaller"])
		if !ok || protocol != queueHandoverProtocol || callerErr != nil || !jsonEqual(caller, attempt.Caller) || job.token != attempt.Token || job.attempt != attempt.Attempt || !jsonEqual(job.worker, attempt.Worker) {
			return errors.New("Retained execution attempt changed after handover")
		}
		allowed := job.phase == attempt.Phase || job.phase == "done" || job.phase == "outcome"
		if !allowed {
			return errors.New("Retained execution attempt advanced incompatibly")
		}
		if job.phase == "running" {
			owned, err := VerifyGroupIdentity(job.worker)
			if err != nil {
				return err
			}
			if !owned {
				current, readErr := store.job(job.id)
				if readErr != nil {
					return readErr
				}
				if current.token != job.token || current.attempt != job.attempt || !jsonEqual(current.worker, job.worker) || current.phase != "outcome" && current.phase != "done" {
					return errors.New("Retained worker ownership is uncertain")
				}
				job = current
			}
		}
		if err := validateQueueHandoverJobState(job); err != nil {
			return err
		}
	}
	jobs, err := store.unfinishedJobs()
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if _, ok := known[job.id]; !ok {
			return errors.New("A new execution appeared after queue handover")
		}
	}
	return nil
}
func validateReleasedQueueHandover(g *Gate, value, target string) ([]queueHandoverAttempt, error) {
	receipt, err := readQueueHandoverReceipt(g)
	if err != nil {
		return nil, err
	}
	if receipt.Phase != "released" || receipt.Token != value || receipt.TargetSHA256 != target {
		return nil, errors.New("Released queue handover identity does not match the installer")
	}
	if err := validateQueueHandoverDatabase(g, receipt); err != nil {
		return nil, err
	}
	alive, err := queueHandoverDaemonAlive(receipt.Daemon)
	if err != nil {
		return nil, err
	}
	if alive {
		return nil, errors.New("The previous queue daemon has not released ownership")
	}
	store, err := queueReadHandoverStore(g)
	if err != nil {
		return nil, err
	}
	defer store.close()
	if err := validateQueueHandoverAttempts(store, receipt); err != nil {
		return nil, err
	}
	return receipt.Attempts, nil
}

// A successful successor may already have admitted new work or completed its
// inherited attempts. Installer retry proves that exact live consumer without
// treating the old dispatch inventory as a permanent snapshot of the queue.
func validateConsumedQueueHandover(g *Gate, value, target string) error {
	receipt, err := readQueueHandoverReceipt(g)
	if err != nil {
		return err
	}
	if receipt.Phase != "consumed" || receipt.Token != value || receipt.TargetSHA256 != target || receipt.Consumer == nil {
		return errors.New("Consumed queue handover identity does not match the installer")
	}
	if err := validateQueueHandoverDatabase(g, receipt); err != nil {
		return err
	}
	alive, err := queueHandoverDaemonAlive(*receipt.Consumer)
	if err != nil {
		return err
	}
	if !alive {
		return errors.New("The exact adopted queue daemon is no longer alive")
	}
	store, err := queueReadHandoverStore(g)
	if err != nil {
		return err
	}
	store.close()
	return nil
}

func queueAdoptHandover(g *Gate, store *queueStore) (*queueHandoverReceipt, error) {
	receipt, err := readQueueHandoverReceipt(g)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if receipt.Phase != "released" {
		return nil, nil
	}
	digest, err := queueExecutableSHA256()
	if err != nil {
		return nil, err
	}
	if digest != receipt.TargetSHA256 {
		return nil, errors.New("The new daemon executable does not match queue handover custody")
	}
	if _, err := validateReleasedQueueHandover(g, receipt.Token, digest); err != nil {
		return nil, err
	}
	return receipt, nil
}
func consumeQueueHandover(g *Gate, receipt *queueHandoverReceipt) error {
	current, err := readQueueHandoverReceipt(g)
	if err != nil {
		return err
	}
	if !jsonEqual(current, receipt) {
		return errors.New("Queue handover custody changed before adoption")
	}
	identity, err := queueHandoverIdentity()
	if err != nil {
		return err
	}
	consumed := *receipt
	consumed.Phase = "consumed"
	consumed.Consumer = &identity
	return writeQueueHandoverReceipt(g, &consumed)
}
func queueHandoverFollowTask(g *Gate, id int, expectedToken string) (Object, bool, error) {
	return queueHandoverRetainedTask(g, id, expectedToken, "")
}

// A claimed worker waiting for the shared slot must retain its attempt through
// the same authenticated outage as its caller. It still needs a live daemon's
// capacity decision before it can acquire the slot and execute the command.
func queueHandoverWorkerWait(g *Gate, id int, value, attempt string) (bool, error) {
	_, allowed, err := queueHandoverRetainedTask(g, id, value, attempt)
	return allowed, err
}

func queueHandoverRetainedTask(g *Gate, id int, expectedToken, workerAttempt string) (Object, bool, error) {
	if !validToken(expectedToken) {
		return nil, false, nil
	}
	receipt, err := readQueueHandoverReceipt(g)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if receipt.Phase != "committed" && receipt.Phase != "released" && receipt.Phase != "consumed" {
		return nil, false, nil
	}
	if err := validateQueueHandoverDatabase(g, receipt); err != nil {
		return nil, false, err
	}
	if receipt.Phase == "committed" {
		alive, err := queueHandoverDaemonAlive(receipt.Daemon)
		if err != nil || !alive {
			if err != nil {
				return nil, false, err
			}
			return nil, false, errors.New("Queue daemon crashed before releasing handover custody")
		}
	}
	if receipt.Phase == "consumed" {
		alive, err := queueHandoverDaemonAlive(*receipt.Consumer)
		if err != nil || !alive {
			if err != nil {
				return nil, false, err
			}
			return nil, false, errors.New("Adopted queue daemon is unavailable after handover")
		}
	}
	var expected *queueHandoverAttempt
	for i := range receipt.Attempts {
		if receipt.Attempts[i].ID == id {
			expected = &receipt.Attempts[i]
			break
		}
	}
	store, err := queueReadHandoverStore(g)
	if err != nil {
		return nil, false, err
	}
	defer store.close()
	job, err := store.job(id)
	if err != nil {
		return nil, false, err
	}
	protocol, ok := integer(job.task["handoverProtocol"])
	caller, callerErr := decodeQueueHandoverIdentity(job.task["handoverCaller"])
	if expected == nil {
		// A job can finish and be cleaned just before commit. Its original
		// caller token still identifies its immutable terminal result.
		if workerAttempt == "" && ok && protocol == queueHandoverProtocol && callerErr == nil && job.token == expectedToken && job.phase == "done" && validateQueueHandoverJobState(job) == nil {
			return job.task, true, nil
		}
		return nil, false, errors.New("The followed job does not match queue handover custody")
	}
	if expected.Token != expectedToken {
		return nil, false, errors.New("The followed job token changed during queue handover")
	}
	if workerAttempt != "" && (job.phase != "running" || job.attempt != workerAttempt) {
		return nil, false, errors.New("Waiting worker attempt is no longer active")
	}
	if !ok || protocol != queueHandoverProtocol || callerErr != nil || !jsonEqual(caller, expected.Caller) || job.token != expectedToken || job.attempt != expected.Attempt || !jsonEqual(job.worker, expected.Worker) {
		return nil, false, errors.New("The followed execution changed during queue handover")
	}
	if job.phase != expected.Phase && job.phase != "outcome" && job.phase != "done" {
		return nil, false, errors.New("The followed execution advanced incompatibly during handover")
	}
	if job.phase == "running" {
		owned := false
		var err error
		if workerAttempt != "" {
			identity, identityErr := queueHandoverIdentity()
			pid, _ := integer(job.worker["pid"])
			pgid, _ := integer(job.worker["pgid"])
			if identityErr != nil {
				return nil, false, identityErr
			}
			owned = workerAttempt == job.attempt && pid == identity.PID && pgid == identity.PGID && text(job.worker["boot_uuid"]) == identity.BootUUID
			birthMatches := false
			for _, raw := range list(job.worker["members"]) {
				member := obj(raw)
				p, _ := integer(member["pid"])
				if p == identity.PID && text(member["birth"]) == identity.Birth {
					birthMatches = true
				}
			}
			owned = owned && birthMatches
			if !owned {
				return nil, false, errors.New("Waiting worker does not match retained handover attempt")
			}
		} else {
			owned, err = VerifyGroupIdentity(job.worker)
		}
		if err != nil {
			return nil, false, err
		}
		if !owned {
			current, err := store.job(job.id)
			if err != nil {
				return nil, false, err
			}
			if current.token != job.token || current.attempt != job.attempt || !jsonEqual(current.worker, job.worker) || current.phase != "outcome" && current.phase != "done" {
				return nil, false, errors.New("Retained worker outcome is uncertain during handover")
			}
			job = current
		}
	}
	if err := validateQueueHandoverJobState(job); err != nil {
		return nil, false, err
	}
	return job.task, true, nil
}
