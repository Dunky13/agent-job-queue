// Package delivery dispatches the trusted companion tap refresh and verifies its
// signed, immutable output independently. Downloaded release code is never run.
package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dunky13/agent-job-queue/internal/backend"
	"github.com/Dunky13/agent-job-queue/internal/tooling"
)

const TapRepository = "Dunky13/homebrew-agent-job-queue"
const branch = "tap"
const workflow = "refresh.yml"
const apiVersion = "2026-03-10"
const maxMetadata = 16384
const maxAPIResponse = 2097152
const maxTooling = 131072

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// RequestOptions deliberately separates dispatch permissions from read access.
type RequestOptions struct {
	Actions bool
	Data    any
	Raw     bool
	Limit   int
}
type GitHub interface {
	Request(context.Context, string, RequestOptions) ([]byte, error)
}

// Client uses gh's transport without allowing credentials or API bodies in errors.
type Client struct{ ActionsToken, SourceToken string }

func (c Client) Request(ctx context.Context, endpoint string, options RequestOptions) ([]byte, error) {
	if c.ActionsToken == "" || c.SourceToken == "" {
		return nil, errors.New("GH_TOKEN and SOURCE_GH_TOKEN must both be configured")
	}
	if !strings.HasPrefix(endpoint, "repos/"+TapRepository+"/") && !strings.HasPrefix(endpoint, "repos/"+backend.ReleaseRepository+"/") {
		return nil, errors.New("GitHub endpoint escaped the fixed delivery repositories")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.New("Homebrew synchronization exceeded its deadline")
	}
	limit := options.Limit
	if limit <= 0 {
		limit = maxAPIResponse
	}
	method, accept := "GET", "application/vnd.github+json"
	if options.Data != nil {
		method = "POST"
	}
	if options.Raw {
		accept = "application/octet-stream"
	}
	args := []string{"api", "--hostname", "github.com", "--method", method, "-H", "X-GitHub-Api-Version: " + apiVersion, "-H", "Accept: " + accept, endpoint}
	var input []byte
	var err error
	if options.Data != nil {
		input, err = json.Marshal(options.Data)
		if err != nil {
			return nil, errors.New("Invalid GitHub dispatch metadata")
		}
		args = append(args, "--input", "-")
	}
	opCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(opCtx, "gh", args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains([]string{"GH_TOKEN", "GITHUB_TOKEN", "GH_DEBUG", "SOURCE_GH_TOKEN", "GH_HOST"}, name) {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	token := c.SourceToken
	if options.Actions {
		token = c.ActionsToken
	}
	cmd.Env = append(cmd.Env, "GH_TOKEN="+token, "GH_HOST=github.com")
	// Bound memory and terminate gh when stdout exceeds the explicit response cap.
	output := &limitWriter{limit: limit + 1}
	cmd.Stdout = output
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("Homebrew synchronization exceeded its deadline")
	}
	if opCtx.Err() != nil {
		return nil, errors.New("GitHub API operation timed out during Homebrew synchronization")
	}
	if output.Len() > limit || output.exceeded {
		return nil, errors.New("GitHub metadata exceeds its size limit")
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("Required GitHub API operation failed (exit %d)", exit.ExitCode())
		}
		return nil, errors.New("GitHub CLI could not complete the required API operation")
	}
	return output.Bytes(), nil
}

type limitWriter struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (w *limitWriter) Write(value []byte) (int, error) {
	remaining := w.limit - w.Len()
	if len(value) > remaining {
		_, _ = w.Buffer.Write(value[:remaining])
		w.exceeded = true
		return 0, errors.New("output limit")
	}
	return w.Buffer.Write(value)
}

// parseJSON rejects duplicate fields at every nesting level and preserves integer
// identity instead of rounding workflow IDs and asset sizes through float64.
func parseJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("GitHub metadata is not valid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var parse func(int) (any, error)
	parse = func(depth int) (any, error) {
		if depth >= 64 {
			return nil, errors.New("GitHub JSON is too deeply nested")
		}
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				value := map[string]any{}
				for d.More() {
					keyToken, err := d.Token()
					if err != nil {
						return nil, err
					}
					key, ok := keyToken.(string)
					if !ok {
						return nil, errors.New("Invalid JSON key")
					}
					if _, exists := value[key]; exists {
						return nil, errors.New("Duplicate JSON field")
					}
					child, err := parse(depth + 1)
					if err != nil {
						return nil, err
					}
					value[key] = child
				}
				if _, err := d.Token(); err != nil {
					return nil, err
				}
				return value, nil
			case '[':
				value := make([]any, 0)
				for d.More() {
					child, err := parse(depth + 1)
					if err != nil {
						return nil, err
					}
					value = append(value, child)
				}
				if _, err := d.Token(); err != nil {
					return nil, err
				}
				return value, nil
			default:
				return nil, errors.New("Unexpected JSON delimiter")
			}
		}
		return token, nil
	}
	value, err := parse(0)
	if err != nil {
		return nil, errors.New("GitHub metadata is not valid JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("GitHub metadata is not valid JSON")
	}
	return value, nil
}

func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func text(value any) string           { result, _ := value.(string); return result }
func number(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	value64, err := strconv.ParseInt(string(number), 10, 64)
	return value64, err == nil
}
func positive(value any) (int64, bool) { n, ok := number(value); return n, ok && n > 0 }
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func equal(a, b backend.Manifest) bool {
	return a.BuildInfo.Equal(b.BuildInfo) && a.Tag == b.Tag && a.Asset == b.Asset && a.SHA256 == b.SHA256
}

func manifestBytes(body []byte) (backend.Manifest, error) {
	if len(body) > maxMetadata {
		return backend.Manifest{}, errors.New("Release manifest exceeds its size limit")
	}
	if _, err := parseJSON(body); err != nil {
		return backend.Manifest{}, err
	}
	manifest, err := backend.ValidateManifest(body)
	if err != nil {
		return backend.Manifest{}, errors.New("Release manifest has an invalid immutable identity")
	}
	return manifest, nil
}
func getJSON(ctx context.Context, github GitHub, endpoint string, options RequestOptions) (map[string]any, error) {
	body, err := github.Request(ctx, endpoint, options)
	if err != nil {
		return nil, err
	}
	value, err := parseJSON(body)
	if err != nil {
		return nil, err
	}
	data := object(value)
	if data == nil {
		return nil, errors.New("Unexpected GitHub metadata shape")
	}
	return data, nil
}

func verifyRelease(ctx context.Context, data backend.Manifest, github GitHub) error {
	release, err := getJSON(ctx, github, "repos/"+backend.ReleaseRepository+"/releases/tags/"+data.Tag, RequestOptions{})
	if err != nil {
		return err
	}
	if release["draft"] != false || release["tag_name"] != data.Tag || release["target_commitish"] != *data.Commit || text(release["published_at"]) == "" {
		return errors.New("Published source release does not match the requested immutable identity")
	}
	assets, ok := release["assets"].([]any)
	if !ok || len(assets) > 100 {
		return errors.New("Published source release has invalid assets")
	}
	selected := map[string]map[string]any{}
	for _, name := range []string{"agent-job-queue.json", data.Asset} {
		for _, value := range assets {
			asset := object(value)
			if asset["name"] == name {
				if selected[name] != nil {
					return errors.New("Published source release is missing a unique required asset")
				}
				selected[name] = asset
			}
		}
		asset := selected[name]
		if asset == nil {
			return errors.New("Published source release is missing a unique required asset")
		}
		_, validID := positive(asset["id"])
		_, validSize := positive(asset["size"])
		if !validID || !validSize || asset["state"] != "uploaded" {
			return errors.New("Published source release contains an invalid required asset")
		}
	}
	if selected[data.Asset]["digest"] != "sha256:"+data.SHA256 {
		return errors.New("Published installer asset checksum conflicts with its release manifest")
	}
	manifest := selected["agent-job-queue.json"]
	size, _ := positive(manifest["size"])
	if size > maxMetadata {
		return errors.New("Release manifest exceeds its size limit")
	}
	id, _ := positive(manifest["id"])
	body, err := github.Request(ctx, fmt.Sprintf("repos/%s/releases/assets/%d", backend.ReleaseRepository, id), RequestOptions{Raw: true, Limit: maxMetadata})
	if err != nil {
		return err
	}
	if manifest["digest"] != digest(body) {
		return errors.New("Published source manifest checksum does not match its asset metadata")
	}
	public, err := manifestBytes(body)
	if err != nil {
		return err
	}
	if int64(len(body)) != size || !equal(public, data) {
		return errors.New("Published source manifest conflicts with the requested immutable identity")
	}
	return nil
}

func runURL(id int64) string {
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d", TapRepository, id)
}
func dispatch(ctx context.Context, data backend.Manifest, github GitHub) (int64, error) {
	response, err := getJSON(ctx, github, "repos/"+TapRepository+"/actions/workflows/"+workflow+"/dispatches", RequestOptions{Actions: true, Data: map[string]any{"ref": branch, "inputs": map[string]string{"expected_tag": data.Tag, "expected_commit": *data.Commit}}})
	if err != nil {
		return 0, err
	}
	id, ok := positive(response["workflow_run_id"])
	if !ok {
		return 0, errors.New("GitHub dispatch did not return an exact workflow run identifier")
	}
	if response["run_url"] != fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d", TapRepository, id) || response["html_url"] != runURL(id) {
		return 0, errors.New("GitHub dispatch returned an unexpected workflow run identity")
	}
	return id, nil
}
func waitForRun(ctx context.Context, id int64, github GitHub, interval time.Duration) error {
	for {
		run, err := getJSON(ctx, github, fmt.Sprintf("repos/%s/actions/runs/%d", TapRepository, id), RequestOptions{Actions: true})
		if err != nil {
			return err
		}
		actual, ok := positive(run["id"])
		if !ok || actual != id || object(run["repository"])["full_name"] != TapRepository || run["head_branch"] != branch || run["event"] != "workflow_dispatch" || run["path"] != ".github/workflows/"+workflow {
			return errors.New("GitHub returned an unexpected tap workflow run")
		}
		status := text(run["status"])
		if status == "completed" {
			if run["conclusion"] != "success" {
				return fmt.Errorf("Tap workflow did not succeed; inspect %s", runURL(id))
			}
			return nil
		}
		if !slices.Contains([]string{"queued", "requested", "waiting", "pending", "in_progress"}, status) {
			return errors.New("Tap workflow returned an unknown execution status")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("Homebrew synchronization exceeded its deadline")
		case <-timer.C:
		}
	}
}
func tapHead(ctx context.Context, github GitHub) (string, error) {
	ref, err := getJSON(ctx, github, "repos/"+TapRepository+"/git/ref/heads/"+branch, RequestOptions{})
	if err != nil {
		return "", err
	}
	target := object(ref["object"])
	sha := text(target["sha"])
	if ref["ref"] != "refs/heads/"+branch || target["type"] != "commit" || !shaPattern.MatchString(sha) {
		return "", errors.New("Tap branch does not identify a valid immutable commit")
	}
	return sha, nil
}
func verifyCommit(ctx context.Context, sha string, github GitHub) error {
	response, err := getJSON(ctx, github, "repos/"+TapRepository+"/commits/"+sha, RequestOptions{})
	if err != nil {
		return err
	}
	commit := object(response["commit"])
	if response["sha"] != sha || commit == nil {
		return errors.New("Tap commit identity does not match the pinned branch head")
	}
	if object(commit["verification"])["verified"] != true {
		return errors.New("Tap commit does not have a verified GitHub signature")
	}
	author := object(commit["author"])
	name, email := text(author["name"]), text(author["email"])
	if name == "" || email == "" || len(name) > 256 || len(email) > 256 || strings.ContainsAny(name+email, "\r\n") || !slices.Contains(strings.Split(text(commit["message"]), "\n"), "Signed-off-by: "+name+" <"+email+">") {
		return errors.New("Tap commit is missing its author-matching DCO sign-off")
	}
	return nil
}
func tapFile(ctx context.Context, path, sha string, github GitHub, limit int) ([]byte, error) {
	item, err := getJSON(ctx, github, "repos/"+TapRepository+"/contents/"+path+"?ref="+sha, RequestOptions{Limit: limit * 2})
	if err != nil {
		return nil, err
	}
	size, ok := positive(item["size"])
	content := text(item["content"])
	if item["type"] != "file" || item["path"] != path || item["encoding"] != "base64" || !ok || size > int64(limit) || len(content) > limit*2 {
		return nil, errors.New("Pinned tap metadata is not a bounded regular file")
	}
	content = strings.Join(strings.Fields(content), "")
	body, err := base64.StdEncoding.Strict().DecodeString(content)
	if err != nil {
		return nil, errors.New("Pinned tap metadata has invalid content encoding")
	}
	if int64(len(body)) != size {
		return nil, errors.New("Pinned tap metadata size does not match its content")
	}
	return body, nil
}

// ToolingPaths lists only the reviewed portable companion-tap seed.
func ToolingPaths(root string) map[string]string {
	base := filepath.Join(root, "packaging/homebrew")
	return map[string]string{"refresh.go": filepath.Join(base, "refresh.go"), "refresh_test.go": filepath.Join(base, "refresh_test.go"), "test-cask.rb": filepath.Join(base, "test-cask.rb"), ".github/workflows/refresh.yml": filepath.Join(base, ".github/workflows/refresh.yml")}
}
func verifyTooling(ctx context.Context, github GitHub, files map[string]string) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for {
		if ctx.Err() != nil {
			return errors.New("Homebrew synchronization exceeded its deadline")
		}
		sha, err := tapHead(ctx, github)
		if err != nil {
			return err
		}
		if err := verifyCommit(ctx, sha, github); err != nil {
			return err
		}
		for _, name := range names {
			file, err := os.Open(files[name])
			if err != nil {
				return errors.New("Could not read required synchronization tooling")
			}
			expected, err := io.ReadAll(io.LimitReader(file, maxTooling+1))
			_ = file.Close()
			if err != nil {
				return errors.New("Could not read required synchronization tooling")
			}
			if len(expected) > maxTooling {
				return fmt.Errorf("Canonical tap tooling exceeds its size limit: %s", name)
			}
			actual, err := tapFile(ctx, name, sha, github, maxTooling)
			if err != nil {
				return err
			}
			if !bytes.Equal(expected, actual) {
				return fmt.Errorf("Tap tooling differs: %s; synchronize companion tap PR", name)
			}
		}
		current, err := tapHead(ctx, github)
		if err != nil {
			return err
		}
		if current == sha {
			return nil
		}
	}
}
func verifyAncestry(ctx context.Context, expected, current backend.Manifest, github GitHub) error {
	comparison, err := getJSON(ctx, github, fmt.Sprintf("repos/%s/compare/%s...%s?per_page=1", backend.ReleaseRepository, *expected.Commit, *current.Commit), RequestOptions{})
	if err != nil {
		return err
	}
	ahead, aok := number(comparison["ahead_by"])
	behind, bok := number(comparison["behind_by"])
	status := text(comparison["status"])
	if object(comparison["base_commit"])["sha"] != *expected.Commit || object(comparison["merge_base_commit"])["sha"] != *expected.Commit || !bok || behind != 0 || !aok || !(status == "ahead" && ahead > 0 || status == "identical" && ahead == 0) {
		return errors.New("Newer tap build does not descend from the requested source commit")
	}
	return nil
}

type Result struct {
	Status string `json:"status"`
	Build  int    `json:"build"`
	Commit string `json:"commit"`
}

func verifyTap(ctx context.Context, expected backend.Manifest, github GitHub) (Result, error) {
	for {
		if ctx.Err() != nil {
			return Result{}, errors.New("Homebrew synchronization exceeded its deadline")
		}
		sha, err := tapHead(ctx, github)
		if err != nil {
			return Result{}, err
		}
		if err := verifyCommit(ctx, sha, github); err != nil {
			return Result{}, err
		}
		body, err := tapFile(ctx, "Casks/agent-job-queue.json", sha, github, maxMetadata)
		if err != nil {
			return Result{}, err
		}
		current, err := manifestBytes(body)
		if err != nil {
			return Result{}, err
		}
		if *current.Build < *expected.Build {
			return Result{}, errors.New("Tap still points to an older build after its refresh completed")
		}
		if *current.Build == *expected.Build && !equal(current, expected) {
			return Result{}, errors.New("Tap contains conflicting metadata for the requested build number")
		}
		cask, err := tapFile(ctx, "Casks/agent-job-queue.rb", sha, github, maxMetadata)
		if err != nil {
			return Result{}, err
		}
		rendered, err := tooling.RenderCask(current)
		if err != nil {
			return Result{}, err
		}
		if string(cask) != rendered {
			return Result{}, errors.New("Pinned tap cask differs from the trusted source renderer")
		}
		if err := verifyRelease(ctx, current, github); err != nil {
			return Result{}, err
		}
		if *current.Build > *expected.Build {
			if err := verifyAncestry(ctx, expected, current, github); err != nil {
				return Result{}, err
			}
		}
		latest, err := tapHead(ctx, github)
		if err != nil {
			return Result{}, err
		}
		if latest == sha {
			status := "current"
			if !equal(current, expected) {
				status = "superseded"
			}
			return Result{status, *current.Build, sha}, nil
		}
	}
}

// Synchronize reports success only after exact-run and signed pinned readback.
func Synchronize(ctx context.Context, manifest []byte, github GitHub, files map[string]string, interval time.Duration, output io.Writer) (Result, error) {
	expected, err := manifestBytes(manifest)
	if err != nil {
		return Result{}, err
	}
	if err := verifyRelease(ctx, expected, github); err != nil {
		return Result{}, err
	}
	if err := verifyTooling(ctx, github, files); err != nil {
		return Result{}, err
	}
	id, err := dispatch(ctx, expected, github)
	if err != nil {
		return Result{}, err
	}
	fmt.Fprintf(output, "Dispatched tap refresh for build %d: %s\n", *expected.Build, runURL(id))
	if err := waitForRun(ctx, id, github, interval); err != nil {
		return Result{}, err
	}
	result, err := verifyTap(ctx, expected, github)
	if err != nil {
		return Result{}, err
	}
	fmt.Fprintf(output, "Verified Homebrew %s at build %d: https://github.com/%s/commit/%s\n", result.Status, result.Build, TapRepository, result.Commit)
	return result, nil
}

func Main(arguments []string) int {
	flags := flag.NewFlagSet("queue-delivery", flag.ContinueOnError)
	path := flags.String("manifest", "", "Required release manifest")
	timeout := flags.Int("timeout", 900, "Deadline in seconds (1 to 1800)")
	poll := flags.Int("poll-interval", 5, "Poll interval in seconds (1 to 60)")
	root := flags.String("source-root", ".", "Canonical source checkout")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *path == "" || *timeout < 1 || *timeout > 1800 || *poll < 1 || *poll > 60 {
		fmt.Fprintln(os.Stderr, "A manifest and bounded timeout/poll interval are required")
		return 2
	}
	file, err := os.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not read required synchronization metadata")
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMetadata+1))
	_ = file.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not read required synchronization metadata")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()
	client := Client{ActionsToken: os.Getenv("GH_TOKEN"), SourceToken: os.Getenv("SOURCE_GH_TOKEN")}
	if _, err := Synchronize(ctx, data, client, ToolingPaths(*root), time.Duration(*poll)*time.Second, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
