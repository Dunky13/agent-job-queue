package backend

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const hookConcurrency = 4
const hookTimeout = 10 * time.Second
const hookOutputLimit = 64 * 1024
const hookInputLimit = 16 * 1024
const hookLogLimit = 1024 * 1024

// hookRunner starts plugin hooks off the scheduling path. dispatch never
// blocks: it either claims one of the bounded slots or counts a dropped event.
type hookRunner struct {
	g            *Gate
	slots        chan struct{}
	mu           sync.Mutex
	logMu        sync.Mutex
	drops        map[string]int
	running      sync.WaitGroup
	inspectGroup func(int) ([]Object, error)
}

func newHookRunner(g *Gate) *hookRunner {
	return &hookRunner{g: g, slots: make(chan struct{}, hookConcurrency), drops: map[string]int{}}
}

func (r *hookRunner) dropped(name string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drops[name]
}

func (r *hookRunner) wait() {
	if r != nil {
		r.running.Wait()
	}
}

func (r *hookRunner) dispatch(plugin queuePlugin, event string, payload Object) {
	if r == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil || len(data) > hookInputLimit {
		r.mu.Lock()
		r.drops[plugin.Name]++
		r.mu.Unlock()
		return
	}
	for _, relative := range plugin.Hooks[event] {
		select {
		case r.slots <- struct{}{}:
		default:
			r.mu.Lock()
			r.drops[plugin.Name]++
			r.mu.Unlock()
			continue
		}
		r.running.Add(1)
		go func(relative string) {
			defer r.running.Done()
			defer func() { <-r.slots }()
			r.run(plugin, relative, event, data)
		}(relative)
	}
}

// hookOutput keeps the first 64 KiB and discards the rest without failing the
// hook. It has no ReadFrom, so io.Copy cannot bypass the limit.
type hookOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (o *hookOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	room := o.limit - o.buffer.Len()
	if len(p) > room {
		o.truncated = true
		if room > 0 {
			o.buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return o.buffer.Write(p)
}

func hookEnvironment(g *Gate, plugin, event string) []string {
	home := g.ControlEnv["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "AGENT_QUEUE_EVENT=" + event, "AGENT_QUEUE_PLUGIN=" + plugin}
}

// verifyHook repeats the load-time checks, including the directories above
// the plugin, immediately before every run.
func verifyHook(g *Gate, plugin queuePlugin, relative string) (string, error) {
	root := pluginRoot(g)
	if filepath.Dir(plugin.Dir) != root {
		return "", errors.New("Hook plugin directory moved")
	}
	for _, directory := range []string{filepath.Dir(root), root, plugin.Dir} {
		if err := pluginDirectoryInfo(directory); err != nil {
			return "", err
		}
	}
	return validatePluginHookPath(plugin.Dir, relative)
}

func (r *hookRunner) run(plugin queuePlugin, relative, event string, input []byte) {
	path, err := verifyHook(r.g, plugin, relative)
	if err != nil {
		r.log(plugin.Name, event, relative, "skipped: "+err.Error(), nil)
		return
	}
	output := &hookOutput{limit: hookOutputLimit}
	completed, report, err := os.Pipe()
	if err != nil {
		r.log(plugin.Name, event, relative, "failed to create hook supervisor: "+err.Error(), nil)
		return
	}
	defer completed.Close()
	defer report.Close()
	release, finish, err := os.Pipe()
	if err != nil {
		r.log(plugin.Name, event, relative, "failed to create hook supervisor: "+err.Error(), nil)
		return
	}
	defer release.Close()
	defer finish.Close()
	// Keep an unreaped group leader until the last group signal. A hook can
	// exit before its descendants, and reaping it would allow its PGID to be
	// reused during the remaining deadline. The hook receives only its event
	// on stdin; the supervisor's status and release pipes are closed in it.
	cmd := exec.Command("/bin/sh", "-c", `"$1" 3>&- 4<&-
status=$?
printf '%s\n' "$status" >&3
IFS= read -r _ <&4
exit "$status"`, "agent-queue-hook", path)
	cmd.ExtraFiles = []*os.File{report, release}
	cmd.Dir = plugin.Dir
	cmd.Env = hookEnvironment(r.g, plugin.Name, event)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	deadline := time.NewTimer(hookTimeout)
	defer deadline.Stop()
	if err = cmd.Start(); err != nil {
		r.log(plugin.Name, event, relative, "failed to start: "+err.Error(), output)
		return
	}
	report.Close()
	release.Close()
	pgid := cmd.Process.Pid
	done := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(completed).ReadString('\n')
		code, parseErr := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || parseErr != nil || code < 0 || code > 255 {
			done <- "hook supervisor did not report an exit status"
			return
		}
		done <- fmt.Sprintf("exit %d", code)
	}()
	result := ""
	select {
	case result = <-done:
		inspected := make(chan bool, 1)
		go func() {
			inspect := r.inspectGroup
			if inspect == nil {
				inspect = ProcessGroupMembers
			}
			members, inspectErr := inspect(pgid)
			background := inspectErr != nil
			for _, member := range members {
				pid, _ := integer(member["pid"])
				background = background || pid != pgid
			}
			inspected <- background
		}()
		select {
		case background := <-inspected:
			if background {
				<-deadline.C
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		case <-deadline.C:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			// Join the bounded inspection after signalling, so slow process
			// discovery cannot extend a hook's lifetime or outlive this run.
			<-inspected
		}
	case <-deadline.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		result = "timed out after 10 seconds; process group terminated"
	}
	finish.Close()
	// Only now may Wait reap the supervisor and release its numerical PID.
	_ = cmd.Wait()
	completed.Close()
	r.log(plugin.Name, event, relative, result, output)
}

// log appends to a private per-plugin log, rotating one generation at 1 MiB.
func (r *hookRunner) log(plugin, event, relative, result string, output *hookOutput) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	directory := filepath.Join(r.g.State, "plugins", plugin)
	if privateDirectory(filepath.Join(r.g.State, "plugins")) != nil || privateDirectory(directory) != nil {
		return
	}
	var entry bytes.Buffer
	fmt.Fprintf(&entry, "%s %s %s: %s", time.Now().UTC().Format(time.RFC3339), event, relative, result)
	if output != nil {
		output.mu.Lock()
		if output.truncated {
			entry.WriteString(" (output truncated to 64 KiB)")
		}
		entry.WriteString("\n")
		entry.Write(output.buffer.Bytes())
		output.mu.Unlock()
		if entry.Len() > 0 && entry.Bytes()[entry.Len()-1] != '\n' {
			entry.WriteString("\n")
		}
	} else {
		entry.WriteString("\n")
	}
	path := filepath.Join(directory, "hooks.log")
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size()+int64(entry.Len()) > hookLogLimit {
		if os.Rename(path, path+".1") != nil {
			return
		}
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_APPEND|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || !fileOwned(info, true) || stat.Nlink != 1 {
		return
	}
	_, _ = file.Write(entry.Bytes())
}
