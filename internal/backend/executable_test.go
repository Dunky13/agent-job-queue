package backend

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestExecutableSearchPreservesKernelErrorsAndWaitResults(t *testing.T) {
	directory := t.TempDir()
	fixtureExecutable(t, filepath.Join(directory, "denied", "fixture"), "#!/bin/sh\nprintf forbidden", 0610)
	fixtureExecutable(t, filepath.Join(directory, "second", "fixture"), "#!/bin/sh\nprintf second", 0700)
	var output bytes.Buffer
	err := runExecutableForEnvironment([]string{"fixture"}, directory, map[string]string{"PATH": "denied:second"}, &output, &output)
	if err != nil || output.String() != "second" {
		t.Fatalf("Kernel-denied first candidate blocked search: output=%q err=%v", output.String(), err)
	}
	for _, code := range []int{126, 127} {
		fixtureExecutable(t, filepath.Join(directory, "denied", "fixture"), "#!/bin/sh\nprintf first; exit "+strconv.Itoa(code), 0700)
		output.Reset()
		err = runExecutableForEnvironment([]string{"fixture"}, directory, map[string]string{"PATH": "denied:second"}, &output, &output)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != code || output.String() != "first" {
			t.Fatalf("Command exit caused replay: output=%q err=%v", output.String(), err)
		}
	}
	fixtureExecutable(t, filepath.Join(directory, "denied", "fixture"), "#!/bin/sh\nexit 0", 0600)
	for _, path := range []string{"missing:denied", "denied:missing"} {
		err = runExecutableForEnvironment([]string{"fixture"}, directory, map[string]string{"PATH": path}, &output, &output)
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("Permission error lost to missing candidate: %v", err)
		}
	}
	err = runExecutableForEnvironment([]string{"fixture"}, directory, map[string]string{"PATH": "missing"}, &output, &output)
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("Missing command must retain ENOENT: %v", err)
	}
	fixtureExecutable(t, filepath.Join(directory, "plain"), "exit 0", 0700)
	err = runExecutableForEnvironment([]string{"./plain"}, directory, map[string]string{}, &output, &output)
	if !errors.Is(err, syscall.ENOEXEC) {
		t.Fatalf("Unexpected shell fallback for executable without interpreter: %v", err)
	}
}

func TestExecutableSearchPreservesSymlinkParentTraversal(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "target", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target/child", filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	fixtureExecutable(t, filepath.Join(directory, "target", "fixture"), "#!/bin/sh\nprintf target", 0700)
	fixtureExecutable(t, filepath.Join(directory, "fixture"), "#!/bin/sh\nprintf wrong", 0700)
	var output bytes.Buffer
	if err := runExecutableForEnvironment([]string{"fixture"}, directory, map[string]string{"PATH": "link/.."}, &output, &output); err != nil || output.String() != "target" {
		t.Fatalf("PATH was cleaned before kernel traversal: output=%q err=%v", output.String(), err)
	}
}
