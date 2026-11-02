package remote

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessBirthFailsClosedOnUntrustedListings(t *testing.T) {
	self, group := os.Getpid(), syscall.Getpgrp()
	observerRow := fmt.Sprintf("%d %d 501 S Mon Nov  2 18:22:25 2026\n", self, group)
	target := self + 1
	targetRow := func(state string) string {
		return fmt.Sprintf("%d 400 501 %s   Sun Nov  1 07:58:26 2026   \n", target, state)
	}
	for _, test := range []struct {
		name, output string
		status       int
		want         string
		fails        bool
	}{
		{name: "alive keeps ps spacing", output: observerRow + targetRow("S"), want: "Sun Nov  1 07:58:26 2026"},
		{name: "absent", output: observerRow},
		{name: "zombie", output: observerRow + targetRow("Z")},
		{name: "failed listing", status: 1, fails: true},
		{name: "nonzero exit with a usable row", output: observerRow, status: 1, fails: true},
		{name: "no observer", output: targetRow("S"), fails: true},
		{name: "observer in another group", output: fmt.Sprintf("%d %d 501 S Mon Nov  2 18:22:25 2026\n", self, group+1), fails: true},
		{name: "zombie observer", output: fmt.Sprintf("%d %d 501 Z Mon Nov  2 18:22:25 2026\n", self, group), fails: true},
		{name: "duplicate target", output: observerRow + targetRow("S") + targetRow("S"), fails: true},
		{name: "cut row", output: observerRow + strconv.Itoa(target) + " 400\n", fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &controller{ctx: context.Background()}
			c.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s' "$1"; exit "$2"`, "fixture", test.output, strconv.Itoa(test.status))
			}
			birth, err := c.processBirth(target)
			if (err != nil) != test.fails || birth != test.want {
				t.Fatalf("processBirth = %q, %v", birth, err)
			}
		})
	}
}

func TestProcessBirthReadsAnImpossiblePidAsExited(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ps limit")
	}
	c := &controller{ctx: context.Background()}
	birth, err := c.processBirth(darwinMaxPID + 1)
	if err != nil || birth != "" {
		t.Fatalf("impossible pid = %q, %v, want exited", birth, err)
	}
	// The real ps must still accept the largest PID, or the limit is wrong.
	if birth, err := c.processBirth(darwinMaxPID); err != nil || birth != "" {
		t.Fatalf("largest pid = %q, %v, want exited", birth, err)
	}
}

func TestProcessBirthMatchesRecordsFromTheSinglePidListing(t *testing.T) {
	c := &controller{ctx: context.Background()}
	birth, err := c.processBirth(os.Getpid())
	if err != nil || birth == "" {
		t.Fatalf("own birth = %q, %v", birth, err)
	}
	// Records written before the census used this listing, in this environment.
	legacy, err := c.command([]string{"/bin/ps", "-p", strconv.Itoa(os.Getpid()), "-o", "lstart=,stat="}, nil, 5*time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	value := strings.TrimSpace(string(legacy))
	if recorded := strings.TrimSpace(value[:strings.LastIndexAny(value, " \t")]); recorded != birth {
		t.Fatalf("birth %q no longer matches recorded %q", birth, recorded)
	}
	// macOS allocates PIDs below 99999, so this one is always absent.
	if birth, err := c.processBirth(99999); err != nil || birth != "" {
		t.Fatalf("absent birth = %q, %v", birth, err)
	}
}
