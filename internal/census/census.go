// Package census parses the process table and answers identity and liveness
// questions from it. It takes text, so callers keep their own execution
// mechanism and timeouts, and it reads no arguments, environment or paths.
//
// A listing that cannot be trusted is an error, never an empty answer: a
// process missing from a rejected census must not read as a process that exited.
package census

import (
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Columns is the only ps column list. Run it as `ps -axo Columns` with LC_ALL=C
// or an environment without locale settings; another locale fails closed.
const Columns = "pid=,pgid=,uid=,stat=,lstart="

const (
	leadingColumns = 4
	lstartColumns  = 5
	lstartLayout   = "Mon Jan 2 15:04:05 2006"
)

var (
	ErrTruncated  = errors.New("process listing row is truncated")
	ErrInvalid    = errors.New("process listing row is invalid")
	ErrAmbiguous  = errors.New("process listing repeats a process")
	ErrIncomplete = errors.New("process listing does not show its observer as running")
)

// Wording lets a caller keep its established message for each rejection.
type Wording struct{ Truncated, Invalid, Ambiguous, Incomplete string }

// Of returns the caller's error for a Parse failure.
func (w Wording) Of(err error) error {
	switch {
	case errors.Is(err, ErrTruncated):
		return errors.New(w.Truncated)
	case errors.Is(err, ErrInvalid):
		return errors.New(w.Invalid)
	case errors.Is(err, ErrAmbiguous):
		return errors.New(w.Ambiguous)
	case errors.Is(err, ErrIncomplete):
		return errors.New(w.Incomplete)
	}
	return err
}

// Observer is the process that took the listing. A complete listing must show
// it running in its own process group.
type Observer struct{ PID, PGID int }

// Self returns the calling process as the observer.
func Self() Observer { return Observer{os.Getpid(), syscall.Getpgrp()} }

// Process is one row. Birth has single spaces, the form backend records hold.
// RawBirth keeps ps's spacing, the form remote service records hold.
type Process struct {
	PID, PGID, UID         int
	State, Birth, RawBirth string
}

// Table is a validated listing keyed by PID.
type Table map[int]Process

// Parse validates a whole listing. UIDs may be negative because macOS prints
// nobody as -2; PGID 0 is the kernel's group.
func Parse(output string, observer Observer) (Table, error) {
	table := Table{}
	observerSeen := false
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		process, err := parseRow(line)
		if err != nil {
			return nil, err
		}
		if _, duplicate := table[process.PID]; duplicate {
			return nil, ErrAmbiguous
		}
		table[process.PID] = process
		observerSeen = observerSeen || process.PID == observer.PID && process.PGID == observer.PGID && !IsZombie(process.State)
	}
	if !observerSeen {
		return nil, ErrIncomplete
	}
	return table, nil
}

func parseRow(line string) (Process, error) {
	fields := strings.Fields(line)
	if len(fields) < leadingColumns+lstartColumns {
		return Process{}, ErrTruncated
	}
	pid, pidErr := strconv.Atoi(fields[0])
	pgid, pgidErr := strconv.Atoi(fields[1])
	uid, uidErr := strconv.Atoi(fields[2])
	if pidErr != nil || pgidErr != nil || uidErr != nil || pid <= 0 || pgid < 0 {
		return Process{}, ErrInvalid
	}
	birth := strings.Join(fields[leadingColumns:], " ")
	// The full timestamp also catches a row cut off inside its last column.
	if _, err := time.Parse(lstartLayout, birth); err != nil {
		return Process{}, ErrInvalid
	}
	raw := line
	for _, field := range fields[:leadingColumns] {
		raw = raw[strings.Index(raw, field)+len(field):]
	}
	return Process{PID: pid, PGID: pgid, UID: uid, State: fields[3], Birth: birth, RawBirth: strings.TrimSpace(raw)}, nil
}

// Group returns a process group's members in PID order.
func (t Table) Group(pgid int) []Process {
	members := []Process{}
	for _, process := range t {
		if process.PGID == pgid {
			members = append(members, process)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PID < members[j].PID })
	return members
}

// Liveness is what a census says about a recorded PID and birth.
type Liveness int

const (
	Absent Liveness = iota
	Reused
	Defunct
	Alive
)

// Identify compares a recorded identity with the census. The returned process
// is meaningful unless the result is Absent.
func (t Table) Identify(pid int, birth string) (Process, Liveness) {
	process, present := t[pid]
	switch {
	case !present:
		return process, Absent
	case process.Birth != NormalizeBirth(birth):
		return process, Reused
	case IsZombie(process.State):
		return process, Defunct
	}
	return process, Alive
}

// IsZombie reports whether a ps state describes an exited, unreaped process.
func IsZombie(state string) bool { return strings.HasPrefix(state, "Z") }

// NormalizeBirth gives a recorded birth the spacing of Process.Birth.
func NormalizeBirth(birth string) string { return strings.Join(strings.Fields(birth), " ") }

var bootIdentityPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseBootIdentity validates kern.bootsessionuuid or Linux boot_id output and
// returns its lowercase form.
func ParseBootIdentity(output string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(output))
	return value, bootIdentityPattern.MatchString(value)
}
