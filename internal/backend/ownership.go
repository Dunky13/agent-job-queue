package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

func jsonEqual(a, b any) bool {
	left, e1 := json.Marshal(a)
	right, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(left) == string(right)
}
func identityCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	out := &boundedOutput{limit: 8 * 1024 * 1024}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || out.exceeded {
		return "", errors.New("Cannot establish process identity; shared slot retained")
	}
	return out.String(), nil
}
func BootIdentity() (string, error) {
	value, err := identityCommand("/usr/sbin/sysctl", "-n", "kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", errors.New("Invalid boot identity; shared slot retained")
	}
	return id.String(), nil
}
func ProcessGroupMembers(pgid int) ([]Object, error) {
	output, err := identityCommand("/bin/ps", "-axo", "pid=,pgid=,lstart=")
	if err != nil {
		return nil, err
	}
	members := []Object{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return nil, errors.New("Invalid process identity listing; shared slot retained")
		}
		pid, e1 := strconv.Atoi(fields[0])
		group, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil || pid < 1 || group < 0 {
			return nil, errors.New("Invalid process identity listing; shared slot retained")
		}
		if group == pgid {
			members = append(members, Object{"pid": pid, "birth": strings.Join(fields[2:], " ")})
		}
	}
	return members, nil
}

type groupState struct {
	PID          int
	Birth, State string
}

func processGroupStates(pgid int) ([]groupState, error) {
	output, err := identityCommand("/bin/ps", "-axo", "pid=,pgid=,stat=,lstart=")
	if err != nil {
		return nil, err
	}
	members := []groupState{}
	observer := false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return nil, errors.New("Invalid process state listing; shared slot retained")
		}
		pid, e1 := strconv.Atoi(fields[0])
		group, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil {
			return nil, errors.New("Invalid process state listing; shared slot retained")
		}
		if pid == os.Getpid() && group == syscall.Getpgrp() && !strings.HasPrefix(fields[2], "Z") {
			observer = true
		}
		if group == pgid {
			members = append(members, groupState{pid, strings.Join(fields[3:], " "), fields[2]})
		}
	}
	if !observer {
		return nil, errors.New("Incomplete process state listing; shared slot retained")
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return members, nil
}
func GroupExists(pgid int) (bool, error) {
	return groupExistsWith(pgid, syscall.Kill, processGroupStates)
}

// groupExistsWith keeps the kernel probes and complete process snapshots in one
// decision, allowing deterministic coverage of processes reaped between probes.
func groupExistsWith(pgid int, probe func(int, syscall.Signal) error, snapshot func(int) ([]groupState, error)) (bool, error) {
	if pgid <= 1 {
		return false, errors.New("Unsafe process group")
	}
	err := probe(-pgid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		initial, e := snapshot(pgid)
		if e != nil {
			return false, e
		}
		if len(initial) == 0 && errors.Is(probe(-pgid, 0), syscall.ESRCH) {
			return false, nil
		}
		zombies := len(initial) > 0
		for _, entry := range initial {
			zombies = zombies && strings.HasPrefix(entry.State, "Z")
		}
		if zombies {
			again := probe(-pgid, 0)
			if errors.Is(again, syscall.ESRCH) {
				return false, nil
			}
			if again == nil {
				return true, nil
			}
			if errors.Is(again, syscall.EPERM) {
				current, e := snapshot(pgid)
				if e != nil {
					return false, e
				}
				if retainedZombieSubset(initial, current) {
					return false, nil
				}
				last := probe(-pgid, 0)
				if errors.Is(last, syscall.ESRCH) {
					return false, nil
				}
			}
		}
	}
	return false, errors.New("Cannot verify the owned process group; retaining its lock")
}

// A complete, nonempty snapshot containing only the same zombie identities
// cannot gain live work while members are reaped. Empty or changed identities
// still need an authoritative ESRCH; they never establish quiescence alone.
func retainedZombieSubset(initial, current []groupState) bool {
	if len(initial) == 0 || len(current) == 0 || len(current) > len(initial) {
		return false
	}
	known := make(map[int]string, len(initial))
	for _, entry := range initial {
		if entry.PID <= 1 || entry.Birth == "" || !strings.HasPrefix(entry.State, "Z") {
			return false
		}
		if _, duplicate := known[entry.PID]; duplicate {
			return false
		}
		known[entry.PID] = entry.Birth
	}
	seen := make(map[int]bool, len(current))
	for _, entry := range current {
		if entry.PID <= 1 || entry.Birth == "" || !strings.HasPrefix(entry.State, "Z") || known[entry.PID] != entry.Birth || seen[entry.PID] {
			return false
		}
		seen[entry.PID] = true
	}
	return true
}

func SharedManifest(g *Gate) (Object, error) {
	manifest, err := ReadPrivate(filepath.Join(g.Slot, "agent-heavy-lease.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	version, ok1 := integer(manifest["version"])
	uid, ok2 := integer(manifest["uid"])
	if len(manifest) != 4 || !ok1 || version != 1 || !ok2 || uid != os.Getuid() || !validToken(text(manifest["lease_id"])) || text(manifest["boot_uuid"]) == "" {
		return nil, errors.New("Invalid shared lease ownership; retaining the validation slot")
	}
	for _, directory := range []string{g.Slot, filepath.Join(g.Slot, "agent-heavy-jobs")} {
		info, err := os.Lstat(directory)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("Unsafe shared lease directory; retaining the validation slot")
		}
	}
	entries, err := os.ReadDir(g.Slot)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"agent-heavy-jobs", "agent-heavy-lease.json", "owner"}) {
		return nil, errors.New("Unexpected shared lease metadata; retaining the validation slot")
	}
	entries, err = os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		member, err := ReadPrivate(filepath.Join(g.Slot, "agent-heavy-jobs", entry.Name()))
		if err != nil {
			return nil, err
		}
		id, valid := integer(member["task_id"])
		if !valid || id < 0 || !validToken(text(member["token"])) || entry.Name() != fmt.Sprintf("%d-%s.json", id, text(member["token"])) || member["lease_id"] != manifest["lease_id"] || member["boot_uuid"] != manifest["boot_uuid"] {
			return nil, errors.New("Invalid shared lease member; retaining the validation slot")
		}
	}
	return manifest, nil
}
func JobMarkerPath(g *Gate, marker Object) string {
	id, _ := integer(marker["task_id"])
	if _, ok := marker["lease_id"]; ok {
		return filepath.Join(g.Slot, "agent-heavy-jobs", fmt.Sprintf("%d-%s.json", id, text(marker["token"])))
	}
	return filepath.Join(g.Slot, "agent-heavy.json")
}
func LeaseCapacity(g *Gate) (int, error) {
	snapshot, err := g.Snapshot()
	if err != nil {
		return 0, err
	}
	limit, ok := integer(obj(obj(snapshot["groups"])["default"])["parallel_tasks"])
	if !ok {
		return 0, errors.New("Invalid parallel limit")
	}
	if err = ValidateLimit(limit); err != nil {
		return 0, err
	}
	admitted := 0
	for _, raw := range obj(snapshot["tasks"]) {
		task := obj(raw)
		name, _, err := TaskStatus(task)
		if err != nil {
			return 0, err
		}
		group := text(task["group"])
		if (group == "" || group == "default") && (name == "Running" || name == "Paused") {
			admitted++
		}
	}
	return max(limit, admitted), nil
}
func AcquireLease(g *Gate, marker Object, capacity int) (bool, error) {
	unlock, err := LockPrivate(g, "")
	if err != nil {
		return false, err
	}
	defer unlock()
	var manifest Object
	count := 0
	if _, err = os.Lstat(g.Slot); err == nil {
		manifest, err = SharedManifest(g)
		if err != nil {
			return false, err
		}
		if manifest == nil {
			return false, nil
		}
		entries, e := os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
		if e != nil {
			return false, e
		}
		count = len(entries)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	permitted, err := SchedulingPermit(g, marker, count)
	if err != nil || !permitted {
		return false, err
	}
	if err = os.Mkdir(g.Slot, 0o700); err == nil {
		manifest = Object{"version": 1, "lease_id": token(), "uid": os.Getuid(), "boot_uuid": marker["boot_uuid"]}
		if err = os.Mkdir(filepath.Join(g.Slot, "agent-heavy-jobs"), 0o700); err != nil {
			return false, err
		}
		file, e := os.OpenFile(filepath.Join(g.Slot, "owner"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return false, e
		}
		_, e = file.WriteString("agent-heavy shared validation lease\n")
		closeErr := file.Close()
		if e != nil {
			return false, e
		}
		if closeErr != nil {
			return false, closeErr
		}
		if err = WritePrivate(filepath.Join(g.Slot, "agent-heavy-lease.json"), manifest); err != nil {
			return false, err
		}
	} else if errors.Is(err, os.ErrExist) {
		manifest, err = SharedManifest(g)
		if err != nil {
			return false, err
		}
		if manifest == nil {
			return false, nil
		}
	} else {
		return false, err
	}
	if manifest["boot_uuid"] != marker["boot_uuid"] {
		return false, errors.New("Shared lease belongs to another boot; inspect retained owners before recovery")
	}
	entries, err := os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
	if err != nil {
		return false, err
	}
	if len(entries) >= capacity {
		return false, nil
	}
	marker["lease_id"] = manifest["lease_id"]
	marker["executionStartedAt"] = float64(time.Now().UnixNano()) / 1e9
	path := JobMarkerPath(g, marker)
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("Job already owns a validation lease member")
	}
	if err = WritePrivate(path, marker); err != nil {
		return false, err
	}
	id, _ := integer(marker["task_id"])
	if err = WritePrivate(g.ReceiptPath(id), marker); err != nil {
		return false, err
	}
	return true, nil
}
func ValidateMarker(g *Gate, id int) (Object, error) {
	receipt, err := g.Receipt(id)
	if err != nil || receipt == nil {
		return nil, err
	}
	if _, ok := receipt["pgid"]; !ok {
		return nil, nil
	}
	if _, ok := receipt["lease_id"]; ok {
		manifest, err := SharedManifest(g)
		if err != nil {
			return nil, err
		}
		if manifest == nil || manifest["lease_id"] != receipt["lease_id"] {
			return nil, errors.New("Shared lease identity changed; refusing cleanup")
		}
	}
	marker, err := ReadPrivate(JobMarkerPath(g, receipt))
	if err != nil {
		return nil, fmt.Errorf("Owned job marker unavailable; refusing cleanup: %w", err)
	}
	immutable := Object{}
	expected := Object{}
	for k, v := range marker {
		if k != "members" {
			immutable[k] = v
		}
	}
	for k, v := range receipt {
		if k != "members" {
			expected[k] = v
		}
	}
	markerID, ok := integer(marker["task_id"])
	if !jsonEqual(immutable, expected) || !ok || markerID != id || !validToken(text(marker["token"])) {
		return nil, errors.New("Shared slot belongs to another owner; refusing cleanup")
	}
	pgid, ok := integer(marker["pgid"])
	if !ok || pgid <= 1 || pgid == syscall.Getpgrp() {
		return nil, errors.New("Unsafe cleanup process group; retaining the shared slot")
	}
	members := list(marker["members"])
	if text(marker["boot_uuid"]) == "" || len(members) == 0 {
		return nil, errors.New("Missing process birth identities; retaining the shared slot")
	}
	for _, raw := range members {
		member := obj(raw)
		pid, ok := integer(member["pid"])
		if !ok || pid <= 1 || text(member["birth"]) == "" {
			return nil, errors.New("Invalid process birth identities; retaining the shared slot")
		}
	}
	return marker, nil
}
func VerifyGroupIdentity(marker Object) (bool, error) {
	pgid, ok := integer(marker["pgid"])
	if !ok || pgid <= 1 || pgid == syscall.Getpgrp() {
		return false, errors.New("Unsafe process group; shared slot retained")
	}
	exists, err := GroupExists(pgid)
	if err != nil || !exists {
		return false, err
	}
	boot, err := BootIdentity()
	if err != nil {
		return false, err
	}
	if boot != text(marker["boot_uuid"]) {
		return false, errors.New("Process group boot identity changed; shared slot retained without signalling")
	}
	current, err := ProcessGroupMembers(pgid)
	if err != nil {
		return false, err
	}
	for _, member := range current {
		for _, raw := range list(marker["members"]) {
			known := obj(raw)
			pid, _ := integer(member["pid"])
			knownPID, _ := integer(known["pid"])
			if pid == knownPID && strings.Join(strings.Fields(text(member["birth"])), " ") == strings.Join(strings.Fields(text(known["birth"])), " ") {
				return true, nil
			}
		}
	}
	exists, err = GroupExists(pgid)
	if err != nil || !exists {
		return false, err
	}
	return false, errors.New("Process group ownership cannot be verified; shared slot retained without signalling")
}
func removeMissing(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func Cleanup(g *Gate, id int) error {
	if id < 0 {
		return errors.New("Task ID must be nonnegative")
	}
	if err := g.Prepare(); err != nil {
		return err
	}
	unlock, err := LockPrivate(g, "")
	if err != nil {
		return err
	}
	marker, err := ValidateMarker(g, id)
	unlock()
	if err != nil {
		return err
	}
	if marker == nil {
		receipt, err := g.Receipt(id)
		if err != nil {
			return err
		}
		if receipt != nil {
			path, err := g.PayloadPath(text(receipt["token"]))
			if err != nil {
				return err
			}
			if err = removeMissing(path); err != nil {
				return err
			}
			return removeMissing(g.ReceiptPath(id))
		}
		return nil
	}
	pgid, _ := integer(marker["pgid"])
	for _, step := range []struct {
		sig  syscall.Signal
		wait time.Duration
	}{{syscall.SIGTERM, 2 * time.Second}, {syscall.SIGKILL, 4 * time.Second}} {
		owned, err := VerifyGroupIdentity(marker)
		if err != nil {
			return err
		}
		if !owned {
			break
		}
		err = syscall.Kill(-pgid, step.sig)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			exists, e := GroupExists(pgid)
			if e != nil {
				return e
			}
			if exists {
				return errors.New("Cannot signal the owned process group; retaining its lock")
			}
			break
		}
		deadline := time.Now().Add(step.wait)
		for time.Now().Before(deadline) {
			exists, err := GroupExists(pgid)
			if err != nil {
				return err
			}
			if !exists {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return releaseCompletedLease(g, id, marker)
}

func releaseCompletedLease(g *Gate, id int, marker Object) error {
	pgid, _ := integer(marker["pgid"])
	if exists, err := GroupExists(pgid); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("Job %d process group %d remains; shared slot retained", id, pgid)
	}
	unlock, err := LockPrivate(g, "")
	if err != nil {
		return err
	}
	defer unlock()
	current, err := ValidateMarker(g, id)
	if err != nil {
		return err
	}
	// Another cleanup may have removed the receipt while this caller waited for
	// the process group to exit. Nothing remains for this caller to release.
	if current == nil {
		receipt, err := g.Receipt(id)
		if err != nil {
			return err
		}
		if receipt == nil {
			return nil
		}
	}
	if !jsonEqual(current, marker) {
		return errors.New("Slot ownership changed during cleanup; shared slot retained")
	}
	if _, shared := marker["lease_id"]; shared {
		if err = os.Remove(JobMarkerPath(g, marker)); err != nil {
			return err
		}
		entries, err := os.ReadDir(filepath.Join(g.Slot, "agent-heavy-jobs"))
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			for _, path := range []string{filepath.Join(g.Slot, "agent-heavy-jobs"), filepath.Join(g.Slot, "owner"), filepath.Join(g.Slot, "agent-heavy-lease.json"), g.Slot} {
				if err = os.Remove(path); err != nil {
					return err
				}
			}
		}
	} else {
		entries, err := os.ReadDir(g.Slot)
		if err != nil {
			return err
		}
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		if !reflect.DeepEqual(names, []string{"agent-heavy.json", "owner"}) {
			return errors.New("Unexpected shared slot metadata; shared slot retained")
		}
		for _, path := range []string{filepath.Join(g.Slot, "owner"), filepath.Join(g.Slot, "agent-heavy.json"), g.Slot} {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
	}
	path, err := g.PayloadPath(text(marker["token"]))
	if err != nil {
		return err
	}
	if err = removeMissing(path); err != nil {
		return err
	}
	return removeMissing(g.ReceiptPath(id))
}
