package backend

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// runExecutableForEnvironment matches subprocess's exec search. A failed Start
// cannot have run the command, so another PATH candidate is safe to try. Once a
// child starts, Wait's result is final, including command exit codes 126 and 127.
func runExecutableForEnvironment(argv []string, cwd string, environment map[string]string, stdout, stderr io.Writer) error {
	candidates := []string{argv[0]}
	if !strings.ContainsRune(argv[0], os.PathSeparator) {
		path, present := environment["PATH"]
		if !present {
			path = "/bin:/usr/bin"
		}
		candidates = nil
		for _, directory := range strings.Split(path, string(os.PathListSeparator)) {
			// Preserve empty entries and symlink/.. traversal. filepath.Join would
			// clean those paths before the kernel can resolve them relative to cwd.
			if directory == "" {
				directory = "."
			}
			candidates = append(candidates, directory+string(os.PathSeparator)+argv[0])
		}
	}
	var firstError, lastError error
	for _, candidate := range candidates {
		cmd := exec.Command(candidate, argv[1:]...)
		cmd.Args[0] = argv[0]
		cmd.Dir = cwd
		cmd.Env = envList(environment)
		cmd.Stdin = nil
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			lastError = err
			// Python retains the first error other than a missing candidate or
			// non-directory PATH component if every exec attempt fails.
			if firstError == nil && !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOTDIR) {
				firstError = err
			}
			continue
		}
		return cmd.Wait()
	}
	if firstError != nil {
		return firstError
	}
	return lastError
}
