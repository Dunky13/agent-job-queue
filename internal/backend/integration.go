package backend

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

const integrationMaxBytes = 1024 * 1024
const integrationCodexMaxBytes = 32768
const integrationPrefix = "<!-- agent-job-queue:"
const integrationEnd = "<!-- agent-job-queue:end -->\n"

var integrationShellSafe = regexp.MustCompile(`^[a-zA-Z0-9_/@%+=:,.-]+$`)

func integrationHome() string { home, _ := os.UserHomeDir(); return home }

func IntegrationInstruction() string {
	command := filepath.Join(integrationHome(), ".local/bin/agent-heavy")
	if !integrationShellSafe.MatchString(command) {
		command = "'" + strings.ReplaceAll(command, "'", "'\"'\"'") + "'"
	}
	return "## Agent Job Queue\n\n" +
		"Use the shared queue across repositories and worktrees for heavy builds, broad test suites, " +
		"full type checks, browser suites, and expensive generators:\n\n" +
		"```sh\n" + command + " run --label 'Brief task description' -- <command> [arguments...]\n```\n\n" +
		"Run lightweight reads, edits, and small focused checks directly. Limit workers inside heavy commands " +
		"conservatively. Keep persistent servers and watch mode outside the queue and budget their memory separately.\n\n" +
		"Queue finite, noninteractive commands only; stdin receives EOF. Wrap an outer batch once and do not nest " +
		"queue wrappers. Retain the job/session ID and inspect progress with agent-heavy status. Diagnose queue failures " +
		"instead of bypassing the queue, clearing locks, or weakening checks. Do not change the operator's parallel " +
		"job limit or force-start jobs without explicit permission. Cancel only your own jobs.\n"
}

func integrationBlock(separator int) []byte {
	return []byte(fmt.Sprintf("<!-- agent-job-queue:begin v1 separator=%d -->\n", separator) + IntegrationInstruction() + integrationEnd)
}

func integrationManaged(data []byte) (int, int, bool, error) {
	if !bytes.Contains(data, []byte(integrationPrefix)) {
		return 0, 0, false, nil
	}
	for _, separator := range []int{0, 2} {
		block := integrationBlock(separator)
		if bytes.Count(data, []byte(integrationPrefix)) != 2 || bytes.Count(data, block) != 1 {
			continue
		}
		offset := bytes.Index(data, block)
		if separator > 0 && (offset < separator || !bytes.Equal(data[offset-separator:offset], []byte("\n\n"))) {
			break
		}
		return offset - separator, offset + len(block), true, nil
	}
	return 0, 0, false, errors.New("Managed instructions were edited or have duplicate/unbalanced markers; preserve your changes and resolve the block manually")
}

type integrationIdentity struct {
	device   uint64
	inode    uint64
	mode     uint32
	owner    uint32
	links    uint64
	size     int64
	modified unix.Timespec
	changed  unix.Timespec
}

func integrationStatIdentity(info *unix.Stat_t) integrationIdentity {
	return integrationIdentity{uint64(info.Dev), info.Ino, uint32(info.Mode), info.Uid, uint64(info.Nlink), info.Size, info.Mtim, info.Ctim}
}

func integrationDirectorySafe(descriptor int, final bool) error {
	var info unix.Stat_t
	if err := unix.Fstat(descriptor, &info); err != nil {
		return err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFDIR || (info.Uid != 0 && info.Uid != uint32(os.Getuid())) {
		return errors.New("Instructions have a non-owned or non-directory parent")
	}
	writable := uint32(info.Mode)&0022 != 0
	sharedTemporary := info.Uid == 0 && uint32(info.Mode)&unix.S_ISVTX != 0
	if writable && !sharedTemporary {
		return errors.New("Instructions have a group/world-writable parent")
	}
	if final && (info.Uid != uint32(os.Getuid()) || writable) {
		return errors.New("Instructions directory must be owned by you and not group/world-writable")
	}
	return nil
}

func integrationValidPath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return false
		}
	}
	return true
}

func integrationParent(path string, create bool) (*os.File, error) {
	if !integrationValidPath(path) {
		return nil, errors.New("Instruction paths must be absolute and must not contain '..'")
	}
	descriptor, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*os.File, error) { _ = unix.Close(descriptor); return nil, err }
	if err = integrationDirectorySafe(descriptor, false); err != nil {
		return closeOnError(err)
	}
	parts := strings.FieldsFunc(filepath.Dir(path), func(r rune) bool { return r == '/' })
	for index, part := range parts {
		child, openErr := unix.Openat(descriptor, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if err := unix.Mkdirat(descriptor, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return closeOnError(err)
			}
			child, openErr = unix.Openat(descriptor, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			return closeOnError(openErr)
		}
		if err := integrationDirectorySafe(child, index == len(parts)-1); err != nil {
			_ = unix.Close(child)
			return closeOnError(err)
		}
		_ = unix.Close(descriptor)
		descriptor = child
	}
	if len(parts) == 0 {
		if err := integrationDirectorySafe(descriptor, true); err != nil {
			return closeOnError(err)
		}
	}
	return os.NewFile(uintptr(descriptor), filepath.Dir(path)), nil
}

func integrationRead(parent *os.File, name string) ([]byte, *integrationIdentity, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	source := os.NewFile(uintptr(fd), name)
	defer source.Close()
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, nil, err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 {
		return nil, nil, errors.New("Instructions must be a regular, single-link file owned by you")
	}
	if uint32(info.Mode)&0022 != 0 {
		return nil, nil, errors.New("Instructions must not be group/world-writable")
	}
	if info.Size > integrationMaxBytes {
		return nil, nil, errors.New("Instructions exceed the 1 MiB safety limit")
	}
	before := integrationStatIdentity(&info)
	data, err := io.ReadAll(io.LimitReader(source, integrationMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > integrationMaxBytes {
		return nil, nil, errors.New("Instructions exceed the 1 MiB safety limit")
	}
	if err := unix.Fstat(fd, &info); err != nil {
		return nil, nil, err
	}
	if before != integrationStatIdentity(&info) {
		return nil, nil, errors.New("Instructions changed during inspection; retry after the other editor finishes")
	}
	return data, &before, nil
}

func integrationReadPath(path string) ([]byte, *integrationIdentity, error) {
	parent, err := integrationParent(path, false)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer parent.Close()
	return integrationRead(parent, filepath.Base(path))
}

func integrationTarget(runner string) (string, error) {
	variable, directory, filename := "CODEX_HOME", ".codex", "AGENTS.md"
	if runner == "claude" {
		variable, directory, filename = "CLAUDE_CONFIG_DIR", ".claude", "CLAUDE.md"
	} else if runner != "codex" {
		return "", errors.New("Unknown agent runner")
	}
	home, configured := os.LookupEnv(variable)
	if !configured {
		home = filepath.Join(integrationHome(), directory)
	}
	if !integrationValidPath(home) {
		return "", fmt.Errorf("%s must be an absolute directory without '..'", variable)
	}
	if runner == "codex" {
		override := filepath.Join(home, "AGENTS.override.md")
		content, info, err := integrationReadPath(override)
		if err != nil {
			return "", err
		}
		// Go's unicode.IsSpace matches Rust's White_Space property, including lossy UTF-8 handling.
		if info != nil && strings.TrimFunc(string(content), unicode.IsSpace) != "" {
			return override, nil
		}
	}
	return filepath.Join(home, filename), nil
}

func IntegrationSnapshot() Object {
	rows := []Object{}
	for _, runner := range []struct{ id, name string }{{"codex", "Codex"}, {"claude", "Claude Code"}} {
		row := Object{"id": runner.id, "name": runner.name, "path": nil, "configured": false, "error": nil}
		path, err := integrationTarget(runner.id)
		if err == nil {
			row["path"] = path
			var content []byte
			content, _, err = integrationReadPath(path)
			if err == nil {
				var managed bool
				_, _, managed, err = integrationManaged(content)
				row["configured"] = managed
				if runner.id == "codex" && managed && len(content) > integrationCodexMaxBytes {
					row["configured"] = false
					err = errors.New("Codex instructions exceed its default 32 KiB instruction budget")
				}
			}
		}
		if err != nil {
			row["error"] = err.Error()
		}
		rows = append(rows, row)
	}
	return Object{"runners": rows, "instruction": IntegrationInstruction()}
}

func integrationSameFile(parent *os.File, path string, data []byte, before *integrationIdentity) error {
	actual, current, err := integrationRead(parent, filepath.Base(path))
	if err != nil {
		return err
	}
	if (current == nil) != (before == nil) || (current != nil && *current != *before) || !bytes.Equal(actual, data) {
		return errors.New("Instructions changed concurrently; no replacement was made")
	}
	reopened, err := integrationParent(path, false)
	if err != nil {
		return err
	}
	defer reopened.Close()
	var first, second unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &first); err != nil {
		return err
	}
	if err := unix.Fstat(int(reopened.Fd()), &second); err != nil {
		return err
	}
	if first.Dev != second.Dev || first.Ino != second.Ino {
		return errors.New("Instructions directory changed concurrently; no replacement was made")
	}
	return nil
}

func integrationPrivateWrite(parent *os.File, name string, data []byte) error {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

// Hooks belong to the operation, so concurrency tests need no global mutable state.
type integrationHooks struct {
	write func(*os.File, string, []byte) error
}

func IntegrationChange(runner, action string) error {
	return integrationChange(runner, action, integrationHooks{write: integrationPrivateWrite})
}

func integrationChange(runner, action string, hooks integrationHooks) error {
	if action != "install" && action != "remove" {
		return errors.New("Unknown integration action")
	}
	path, err := integrationTarget(runner)
	if err != nil {
		return err
	}
	if action == "remove" {
		data, _, err := integrationReadPath(path)
		if err != nil {
			return err
		}
		_, _, managed, err := integrationManaged(data)
		if err != nil {
			return err
		}
		if !managed {
			return nil
		}
	}
	parent, err := integrationParent(path, action == "install")
	if err != nil {
		return err
	}
	defer parent.Close()
	lockFD, err := unix.Openat(int(parent.Fd()), ".agent-job-queue.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(lockFD), "agent integration lock")
	defer lock.Close()
	var info unix.Stat_t
	if err := unix.Fstat(lockFD, &info); err != nil {
		return err
	}
	if uint32(info.Mode)&unix.S_IFMT != unix.S_IFREG || info.Uid != uint32(os.Getuid()) || info.Nlink != 1 || uint32(info.Mode)&0077 != 0 {
		return errors.New("Unsafe agent integration lock")
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return errors.New("Another agent integration update is running; try again when it finishes")
		}
		return err
	}
	data, before, err := integrationRead(parent, filepath.Base(path))
	if err != nil {
		return err
	}
	start, end, managed, err := integrationManaged(data)
	if err != nil {
		return err
	}
	var replacement []byte
	if action == "install" {
		if managed {
			return nil
		}
		separator := 0
		if len(data) > 0 {
			separator = 2
		}
		replacement = append(append(append([]byte{}, data...), bytes.Repeat([]byte{'\n'}, separator)...), integrationBlock(separator)...)
	} else {
		if !managed {
			return nil
		}
		replacement = append(append([]byte{}, data[:start]...), data[end:]...)
	}
	if len(replacement) > integrationMaxBytes {
		return errors.New("Resulting instructions exceed the 1 MiB safety limit")
	}
	if action == "install" && runner == "codex" && len(replacement) > integrationCodexMaxBytes {
		return errors.New("Codex instructions would exceed its default 32 KiB instruction budget")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	token := hex.EncodeToString(random)
	temporary := ".agent-job-queue-" + token + ".tmp"
	defer unix.Unlinkat(int(parent.Fd()), temporary, 0)
	if err := integrationSameFile(parent, path, data, before); err != nil {
		return err
	}
	if before != nil {
		if err := hooks.write(parent, filepath.Base(path)+".agent-job-queue-backup-"+token, data); err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			return err
		}
	}
	if err := hooks.write(parent, temporary, replacement); err != nil {
		return err
	}
	if before != nil {
		if err := unix.Fchmodat(int(parent.Fd()), temporary, before.mode&07777, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
	}
	if err := integrationSameFile(parent, path, data, before); err != nil {
		return err
	}
	currentTarget, err := integrationTarget(runner)
	if err != nil {
		return err
	}
	if currentTarget != path {
		return errors.New("Agent instruction precedence changed concurrently; retry")
	}
	if err := unix.Renameat(int(parent.Fd()), temporary, int(parent.Fd()), filepath.Base(path)); err != nil {
		return err
	}
	return parent.Sync()
}

func IntegrationMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Agent integration: status, install, or remove is required")
		return 2
	}
	action, runner, jsonOutput := args[0], "", false
	if action != "status" && action != "install" && action != "remove" {
		fmt.Fprintln(os.Stderr, "Agent integration: unknown action")
		return 2
	}
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "--json":
			jsonOutput = true
		case "--runner":
			index++
			if index >= len(args) || (args[index] != "codex" && args[index] != "claude") {
				fmt.Fprintln(os.Stderr, "Agent integration: invalid --runner")
				return 2
			}
			runner = args[index]
		default:
			fmt.Fprintln(os.Stderr, "Agent integration: unknown argument")
			return 2
		}
	}
	if action != "status" && runner == "" {
		fmt.Fprintln(os.Stderr, "Agent integration: --runner is required for install/remove")
		return 2
	}
	if action != "status" {
		if err := IntegrationChange(runner, action); err != nil {
			fmt.Fprintf(os.Stderr, "Agent integration: %v\n", err)
			return 1
		}
	}
	state := IntegrationSnapshot()
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	exit := 0
	for _, row := range state["runners"].([]Object) {
		status := "Not configured by this app"
		if row["configured"] == true {
			status = "Configured by this app"
		}
		if row["error"] != nil {
			status = row["error"].(string)
			exit = 1
		}
		path := "Invalid instruction directory"
		if row["path"] != nil {
			path = row["path"].(string)
		}
		fmt.Printf("%s: %s\n  %s\n", row["name"], status, path)
	}
	return exit
}
