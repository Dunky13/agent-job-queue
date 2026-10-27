// The companion tap seed is a self-contained standard-library Go program.
// It reads only the fixed public app repository and never executes release code.
package main

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
)

const repository = "Dunky13/agent-job-queue"
const tapRepository = "Dunky13/homebrew-agent-job-queue"
const tapBranch = "tap"
const botName = "github-actions[bot]"
const botEmail = "41898282+github-actions[bot]@users.noreply.github.com"
const metadataLimit = 16384

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var tagPattern = regexp.MustCompile(`^build-([1-9][0-9]{0,8})-([0-9a-f]{7})$`)
var checksumPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)\.)*(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

type manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Repository    string `json:"repository"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Build         int    `json:"build"`
	Tag           string `json:"tag"`
	Asset         string `json:"asset"`
	SHA256        string `json:"sha256"`
}

func strictJSON(body []byte) (any, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("Invalid UTF-8 JSON metadata")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var parse func(int) (any, error)
	parse = func(depth int) (any, error) {
		if depth >= 64 {
			return nil, errors.New("JSON nesting exceeds its bound")
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
		return nil, errors.New("Invalid GitHub JSON metadata")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("Invalid trailing JSON metadata")
	}
	return value, nil
}
func asObject(value any) map[string]any { object, _ := value.(map[string]any); return object }
func asString(value any) string         { text, _ := value.(string); return text }
func asInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(string(number), 10, 64)
	return n, err == nil
}

func validateManifest(body []byte) (manifest, error) {
	if len(body) > metadataLimit {
		return manifest{}, errors.New("Release manifest exceeds its size limit")
	}
	value, err := strictJSON(body)
	if err != nil {
		return manifest{}, err
	}
	fields := asObject(value)
	if len(fields) != 8 {
		return manifest{}, errors.New("Unexpected Homebrew manifest fields")
	}
	for _, key := range []string{"schemaVersion", "repository", "version", "commit", "build", "tag", "asset", "sha256"} {
		if fields[key] == nil {
			return manifest{}, errors.New("Unexpected Homebrew manifest fields")
		}
	}
	var data manifest
	if err := json.Unmarshal(body, &data); err != nil {
		return manifest{}, errors.New("Invalid manifest field types")
	}
	if data.SchemaVersion != 1 || data.Repository != repository {
		return manifest{}, errors.New("Unsupported build identity source")
	}
	if !versionPattern.MatchString(data.Version) {
		return manifest{}, errors.New("Invalid version")
	}
	if !commitPattern.MatchString(data.Commit) {
		return manifest{}, errors.New("Invalid commit")
	}
	if data.Build < 1 || data.Build > 999999999 {
		return manifest{}, errors.New("Invalid build number")
	}
	if !checksumPattern.MatchString(data.SHA256) {
		return manifest{}, errors.New("Invalid sha256")
	}
	if data.Tag != fmt.Sprintf("build-%d-%.7s", data.Build, data.Commit) {
		return manifest{}, errors.New("Release tag does not match build and commit")
	}
	if data.Asset != fmt.Sprintf("Agent-Job-Queue-Installer-%s-universal.zip", data.Version) {
		return manifest{}, errors.New("Unexpected installer asset")
	}
	return data, nil
}
func renderCask(data manifest) (string, error) {
	body, _ := json.Marshal(data)
	if _, err := validateManifest(body); err != nil {
		return "", err
	}
	return fmt.Sprintf(caskTemplate, data.Version, data.Build, data.SHA256, repository, data.Tag, data.Asset, repository), nil
}
func expectedIdentity(tag, commit string) error {
	if (tag == "") != (commit == "") {
		return errors.New("Expected tag and commit must be supplied together")
	}
	if tag == "" {
		return nil
	}
	match := tagPattern.FindStringSubmatch(tag)
	if match == nil || !commitPattern.MatchString(commit) || match[2] != commit[:7] {
		return errors.New("Invalid expected release identity")
	}
	return nil
}

type refresher struct {
	ctx        context.Context
	root, seed string
	output     io.Writer
	command    func([]string, []byte) ([]byte, error)
	api        func(string) (any, error)
	graphql    func(string, map[string]any) (map[string]any, error)
	publish    func(manifest, string, string) error
}

func newRefresher(ctx context.Context, root, seed string, output io.Writer) *refresher {
	f := &refresher{ctx: ctx, root: root, seed: seed, output: output}
	f.command = f.runCommand
	f.api = f.request
	f.graphql = f.graphQL
	f.publish = f.publishCommit
	return f
}
func (f *refresher) runCommand(arguments []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(f.ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Dir = f.root
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	output := &boundedOutput{limit: 2 << 20}
	cmd.Stdout = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("Required external operation exceeded its deadline")
	}
	if err != nil {
		return nil, fmt.Errorf("Required %s operation failed", filepath.Base(arguments[0]))
	}
	return output.Bytes(), nil
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(value []byte) (int, error) {
	if len(value) > b.limit-b.Len() {
		return 0, errors.New("Output exceeds bound")
	}
	return b.Buffer.Write(value)
}
func (f *refresher) request(endpoint string) (any, error) {
	if !strings.HasPrefix(endpoint, "repos/"+repository+"/") && !strings.HasPrefix(endpoint, "repos/"+tapRepository+"/") {
		return nil, errors.New("Unexpected GitHub repository")
	}
	body, err := f.command([]string{"gh", "api", "--hostname", "github.com", endpoint}, nil)
	if err != nil {
		return nil, err
	}
	return strictJSON(body)
}
func (f *refresher) graphQL(query string, variables map[string]any) (map[string]any, error) {
	input, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	body, err := f.command([]string{"gh", "api", "--hostname", "github.com", "graphql", "--input", "-"}, input)
	if err != nil {
		return nil, err
	}
	value, err := strictJSON(body)
	if err != nil {
		return nil, err
	}
	response := asObject(value)
	if response["errors"] != nil || asObject(response["data"]) == nil {
		return nil, errors.New("GitHub GraphQL operation failed")
	}
	return asObject(response["data"]), nil
}

func (f *refresher) publishedReleases() ([]map[string]any, error) {
	var releases []map[string]any
	for page := 1; page <= 10; page++ {
		value, err := f.api(fmt.Sprintf("repos/%s/releases?per_page=100&page=%d", repository, page))
		if err != nil {
			return nil, err
		}
		batch, ok := value.([]any)
		if !ok || len(batch) > 100 {
			return nil, errors.New("Unexpected source release response")
		}
		for _, item := range batch {
			release := asObject(item)
			if release == nil {
				return nil, errors.New("Unexpected source release response")
			}
			releases = append(releases, release)
		}
		if len(batch) < 100 {
			return releases, nil
		}
	}
	return nil, errors.New("Public release listing exceeds its page bound; cannot prove the latest build")
}
func selectRelease(releases []map[string]any) (map[string]any, error) {
	var newest map[string]any
	latest := 0
	for _, release := range releases {
		if release["draft"] != false {
			continue
		}
		match := tagPattern.FindStringSubmatch(asString(release["tag_name"]))
		if match == nil {
			continue
		}
		assets, ok := release["assets"].([]any)
		if !ok {
			continue
		}
		hasManifest := false
		for _, asset := range assets {
			if asObject(asset)["name"] == "agent-job-queue.json" {
				hasManifest = true
			}
		}
		if !hasManifest {
			continue
		}
		build, _ := strconv.Atoi(match[1])
		if build > latest {
			newest = release
			latest = build
		}
	}
	if newest == nil {
		return nil, errors.New("No published app build with Homebrew metadata exists")
	}
	for _, release := range releases {
		match := tagPattern.FindStringSubmatch(asString(release["tag_name"]))
		if match == nil || release["draft"] != false || release["tag_name"] == newest["tag_name"] {
			continue
		}
		build, _ := strconv.Atoi(match[1])
		if build != latest {
			continue
		}
		assets, _ := release["assets"].([]any)
		for _, asset := range assets {
			if asObject(asset)["name"] == "agent-job-queue.json" {
				return nil, errors.New("Multiple public releases claim the latest build number")
			}
		}
	}
	return newest, nil
}
func (f *refresher) releaseByTag(tag string) (map[string]any, error) {
	value, err := f.api("repos/" + repository + "/releases/tags/" + tag)
	if err != nil {
		return nil, err
	}
	release := asObject(value)
	if release["draft"] != false || release["tag_name"] != tag {
		return nil, errors.New("Expected public release is missing or has a different identity")
	}
	return release, nil
}
func (f *refresher) verifySuccessor(older, newer manifest) error {
	if newer.Build <= older.Build || newer.Commit == older.Commit {
		return nil
	}
	value, err := f.api("repos/" + repository + "/compare/" + older.Commit + "..." + newer.Commit)
	if err != nil {
		return err
	}
	comparison := asObject(value)
	if comparison["status"] != "ahead" || asObject(comparison["merge_base_commit"])["sha"] != older.Commit {
		return errors.New("Newer build is not a verified descendant of the previous release")
	}
	return nil
}
func boundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, errors.New("Required metadata is not a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errors.New("Required metadata exceeds its size limit")
	}
	return body, nil
}
func (f *refresher) downloadRelease(release map[string]any, work string) (manifest, string, error) {
	tag := asString(release["tag_name"])
	if !tagPattern.MatchString(tag) {
		return manifest{}, "", errors.New("Invalid public release tag")
	}
	destination := filepath.Join(work, tag)
	if err := os.Mkdir(destination, 0700); err != nil {
		return manifest{}, "", err
	}
	download := func(name string) error {
		_, err := f.command([]string{"gh", "release", "download", tag, "--repo", repository, "--pattern", name, "--dir", destination}, nil)
		return err
	}
	if err := download("agent-job-queue.json"); err != nil {
		return manifest{}, "", err
	}
	body, err := boundedFile(filepath.Join(destination, "agent-job-queue.json"), metadataLimit)
	if err != nil {
		return manifest{}, "", err
	}
	data, err := validateManifest(body)
	if err != nil {
		return manifest{}, "", err
	}
	if release["draft"] != false || release["tag_name"] != data.Tag || release["target_commitish"] != data.Commit {
		return manifest{}, "", errors.New("Release identity does not match manifest")
	}
	assets, ok := release["assets"].([]any)
	if !ok {
		return manifest{}, "", errors.New("Unexpected source release assets")
	}
	names := map[string]int{}
	for _, asset := range assets {
		names[asString(asObject(asset)["name"])]++
	}
	if names["agent-job-queue.json"] != 1 || names[data.Asset] != 1 {
		return manifest{}, "", errors.New("Release does not contain unique expected metadata and installer assets")
	}
	if err := download(data.Asset); err != nil {
		return manifest{}, "", err
	}
	archive, err := os.Open(filepath.Join(destination, data.Asset))
	if err != nil {
		return manifest{}, "", err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, archive)
	_ = archive.Close()
	if err != nil {
		return manifest{}, "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != data.SHA256 {
		return manifest{}, "", errors.New("Downloaded installer checksum does not match manifest")
	}
	cask, err := renderCask(data)
	if err != nil {
		return manifest{}, "", err
	}
	caskPath := filepath.Join(destination, "agent-job-queue.rb")
	if err := os.WriteFile(caskPath, []byte(cask), 0600); err != nil {
		return manifest{}, "", err
	}
	if _, err := f.command([]string{"brew", "ruby", filepath.Join(f.seed, "test-cask.rb"), caskPath}, nil); err != nil {
		return manifest{}, "", err
	}
	return data, cask, nil
}
func (f *refresher) historicalCask(release map[string]any, work string) ([]byte, error) {
	assets, ok := release["assets"].([]any)
	if !ok {
		return nil, errors.New("Unexpected historical release assets")
	}
	var selected map[string]any
	for _, asset := range assets {
		item := asObject(asset)
		if item["name"] == "agent-job-queue.rb" {
			if selected != nil {
				return nil, errors.New("Historical public release must contain exactly one cask asset")
			}
			selected = item
		}
	}
	if selected == nil {
		return nil, errors.New("Historical public release must contain exactly one cask asset")
	}
	size, valid := asInteger(selected["size"])
	if !valid || size < 1 || size > metadataLimit {
		return nil, errors.New("Historical public cask exceeds its declared size limit")
	}
	digest := asString(selected["digest"])
	if selected["digest"] != nil && (!strings.HasPrefix(digest, "sha256:") || !checksumPattern.MatchString(strings.TrimPrefix(digest, "sha256:"))) {
		return nil, errors.New("Historical public cask has an unsupported asset digest")
	}
	tag := asString(release["tag_name"])
	if !tagPattern.MatchString(tag) {
		return nil, errors.New("Invalid public release tag")
	}
	destination := filepath.Join(work, tag, "published-cask")
	if err := os.Mkdir(destination, 0700); err != nil {
		return nil, err
	}
	if _, err := f.command([]string{"gh", "release", "download", tag, "--repo", repository, "--pattern", "agent-job-queue.rb", "--dir", destination}, nil); err != nil {
		return nil, err
	}
	body, err := boundedFile(filepath.Join(destination, "agent-job-queue.rb"), metadataLimit)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != size {
		return nil, errors.New("Historical public cask does not match its declared bounded size")
	}
	sum := sha256.Sum256(body)
	if digest != "" && digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, errors.New("Historical public cask asset digest does not match downloaded bytes")
	}
	return body, nil
}

func (f *refresher) publishCommit(data manifest, cask, expectedHead string) error {
	viewer, err := f.graphql("query { viewer { login } }", map[string]any{})
	if err != nil {
		return err
	}
	if asObject(viewer["viewer"])["login"] != botName {
		return errors.New("Automatic tap publication requires the repository's GitHub Actions token")
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	variables := map[string]any{"input": map[string]any{"branch": map[string]any{"repositoryNameWithOwner": tapRepository, "branchName": tapBranch}, "expectedHeadOid": expectedHead, "message": map[string]any{"headline": fmt.Sprintf("chore(cask): publish build %d", data.Build), "body": "Signed-off-by: " + botName + " <" + botEmail + ">"}, "fileChanges": map[string]any{"additions": []map[string]any{{"path": "Casks/agent-job-queue.rb", "contents": base64.StdEncoding.EncodeToString([]byte(cask))}, {"path": "Casks/agent-job-queue.json", "contents": base64.StdEncoding.EncodeToString(body)}}}}}
	result, err := f.graphql("mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }", variables)
	if err != nil {
		return err
	}
	sha := asString(asObject(asObject(result["createCommitOnBranch"])["commit"])["oid"])
	if !commitPattern.MatchString(sha) {
		return errors.New("Tap publication returned an invalid commit identity")
	}
	value, err := f.api("repos/" + tapRepository + "/commits/" + sha)
	if err != nil {
		return err
	}
	response := asObject(value)
	commit := asObject(response["commit"])
	author := asObject(commit["author"])
	if response["sha"] != sha || asObject(commit["verification"])["verified"] != true || author["name"] != botName || author["email"] != botEmail || !slices.Contains(strings.Split(asString(commit["message"]), "\n"), "Signed-off-by: "+botName+" <"+botEmail+">") {
		return fmt.Errorf("Published tap commit %s failed Verified signature or matching DCO verification", sha)
	}
	fmt.Fprintf(f.output, "Published verified, DCO-signed tap commit %s for %s.\n", sha, data.Tag)
	return nil
}

func (f *refresher) refresh(publish bool, expectedTag, expectedCommit string) error {
	if err := expectedIdentity(expectedTag, expectedCommit); err != nil {
		return err
	}
	headBytes, err := f.command([]string{"git", "rev-parse", "HEAD"}, nil)
	if err != nil {
		return err
	}
	head := strings.TrimSpace(string(headBytes))
	value, err := f.api("repos/" + tapRepository + "/git/ref/heads/" + tapBranch)
	if err != nil {
		return err
	}
	if !commitPattern.MatchString(head) || asObject(asObject(value)["object"])["sha"] != head {
		return errors.New("Tap checkout is stale; rerun refresh against the current distribution branch")
	}
	releases, err := f.publishedReleases()
	if err != nil {
		return err
	}
	latest, err := selectRelease(releases)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(f.root, "Casks/agent-job-queue.json")
	caskPath := filepath.Join(f.root, "Casks/agent-job-queue.rb")
	_, manifestErr := os.Stat(manifestPath)
	_, caskErr := os.Stat(caskPath)
	if manifestErr != nil && !errors.Is(manifestErr, os.ErrNotExist) {
		return manifestErr
	}
	if caskErr != nil && !errors.Is(caskErr, os.ErrNotExist) {
		return caskErr
	}
	if (manifestErr == nil) != (caskErr == nil) {
		return errors.New("Tap metadata and cask must both exist or both be absent")
	}
	var current *manifest
	if manifestErr == nil {
		body, err := boundedFile(manifestPath, metadataLimit)
		if err != nil {
			return err
		}
		item, err := validateManifest(body)
		if err != nil {
			return err
		}
		current = &item
	}
	work, err := os.MkdirTemp("", "agent-queue-tap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	type checkedRelease struct {
		data    manifest
		cask    string
		release map[string]any
	}
	cache := map[string]checkedRelease{}
	checked := func(tag string) (checkedRelease, error) {
		if result, ok := cache[tag]; ok {
			return result, nil
		}
		release, err := f.releaseByTag(tag)
		if err != nil {
			return checkedRelease{}, err
		}
		data, cask, err := f.downloadRelease(release, work)
		if err != nil {
			return checkedRelease{}, err
		}
		result := checkedRelease{data, cask, release}
		cache[tag] = result
		return result, nil
	}
	var requested checkedRelease
	if expectedTag != "" {
		requested, err = checked(expectedTag)
		if err != nil {
			return err
		}
		if requested.data.Commit != expectedCommit {
			return errors.New("Expected commit does not match its public release")
		}
	}
	newest, err := checked(asString(latest["tag_name"]))
	if err != nil {
		return err
	}
	data, cask := newest.data, newest.cask
	if expectedTag != "" {
		if data.Build < requested.data.Build {
			return errors.New("Latest public release is older than the requested delivery")
		}
		if data.Build == requested.data.Build && data != requested.data {
			return errors.New("Requested build has conflicting public release identities")
		}
		if err := f.verifySuccessor(requested.data, data); err != nil {
			return err
		}
	}
	if current != nil {
		published, err := checked(current.Tag)
		if err != nil {
			return err
		}
		if *current != published.data {
			return errors.New("Current tap metadata differs from its validated public release")
		}
		if current.Build > data.Build {
			return errors.New("Latest public release would downgrade the tap")
		}
		if current.Build == data.Build && *current != data {
			return errors.New("The same build number has conflicting release metadata")
		}
		installed, err := boundedFile(caskPath, metadataLimit)
		if err != nil {
			return err
		}
		if string(installed) != published.cask {
			historical, err := f.historicalCask(published.release, work)
			if err != nil {
				return err
			}
			if !bytes.Equal(installed, historical) {
				return errors.New("Current tap cask differs from its historical published release")
			}
		}
		if current.Build == data.Build {
			if string(installed) == cask {
				fmt.Fprintln(f.output, "Tap is already current; public metadata, installer and cask verified.")
			} else {
				fmt.Fprintln(f.output, "Published build verified; template change awaits a newer app release.")
			}
			return nil
		}
		if err := f.verifySuccessor(*current, data); err != nil {
			return err
		}
	}
	if publish {
		return f.publish(data, cask, head)
	}
	fmt.Fprintf(f.output, "Validated %s and installer checksum; publication disabled.\n", data.Tag)
	return nil
}

func publicationAllowed(environment map[string]string) bool {
	return environment["GITHUB_ACTIONS"] == "true" && environment["GITHUB_REPOSITORY"] == tapRepository && environment["GITHUB_REF"] == "refs/heads/"+tapBranch && slices.Contains([]string{"schedule", "workflow_dispatch"}, environment["GITHUB_EVENT_NAME"])
}
func run(arguments []string) error {
	flags := flag.NewFlagSet("tap-refresh", flag.ContinueOnError)
	publish := flags.Bool("publish", false, "Publish an API-signed expected-head commit")
	tag := flags.String("expected-tag", "", "Requested public release tag")
	commit := flags.String("expected-commit", "", "Requested full source commit")
	seed := flags.String("seed-dir", ".", "Reviewed seed directory containing test-cask.rb")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("Unexpected refresh arguments")
	}
	if err := expectedIdentity(*tag, *commit); err != nil {
		return err
	}
	if *publish {
		environment := map[string]string{}
		for _, name := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_REF", "GITHUB_EVENT_NAME"} {
			environment[name] = os.Getenv(name)
		}
		if !publicationAllowed(environment) {
			return errors.New("Automatic publication is restricted to tap schedule or dispatch workflows")
		}
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	seedPath, err := filepath.Abs(*seed)
	if err != nil {
		return err
	}
	return newRefresher(context.Background(), root, seedPath, os.Stdout).refresh(*publish, *tag, *commit)
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const caskTemplate = `cask "agent-job-queue" do
  version "%s,%d"
  sha256 "%s"

  url "https://github.com/%s/releases/download/%s/%s"
  name "Agent Job Queue"
  desc "Shared heavy-command queue with a macOS menu-bar app"
  homepage "https://github.com/%s"

  depends_on macos: :sequoia

  # Preserve quarantine so macOS can assess the notarized release.

  # Run the vendor's noninteractive installer before moving its source app.
  # This documented artifact runs outside the flight-step hook sandbox.
  installer script: {
    executable: "/bin/sh",
    args: ["-c", <<~'SCRIPT', "agent-job-queue-install", staged_path.to_s, version.csv.first, version.csv.second],
      set -eu
      work=$(/usr/bin/mktemp -d /private/tmp/agent-queue-install.XXXXXXXX)
      cleanup() {
        result=$?
        trap - EXIT
        /bin/rm -rf -- "$work" || exit 1
        exit "$result"
      }
      trap cleanup EXIT
      trap 'exit 1' HUP INT TERM
      installer="$work/Agent Job Queue Installer.app"
      /usr/bin/ditto --rsrc --extattr --qtn --acl "$1/Agent Job Queue Installer.app" "$installer"
      payload="$installer/Contents/Resources/payload"
      requirement='=anchor apple generic and certificate leaf[subject.OU] = "U9GXK89565" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized'
      /usr/bin/codesign --verify --strict --deep --all-architectures --check-notarization --test-requirement "$requirement and identifier \"io.went.agent-queue.installer\"" "$installer"
      /usr/bin/codesign --verify --strict --deep --all-architectures --check-notarization --test-requirement "$requirement and identifier \"io.went.agent-queue\"" "$payload/Agent Queue.app"
      /usr/bin/codesign --verify --strict --all-architectures --check-notarization --test-requirement "$requirement" "$payload/source/bin/agent-heavy"
      /usr/bin/codesign --verify --strict --all-architectures --check-notarization --test-requirement "$requirement" "$payload/source/bin/agent-remote"
      /usr/sbin/spctl --assess --type execute "$installer"
      for bundle in "$installer" "$payload/Agent Queue.app"; do
        [ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$bundle/Contents/Info.plist")" = "$2" ] &&
          [ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$bundle/Contents/Info.plist")" = "$3" ] || {
            echo 'The copied installer differs from the requested Homebrew release.' >&2
            exit 1
          }
      done
      "$payload/source/bin/agent-heavy" install --payload "$payload" --repair-and-continue
    SCRIPT
    sudo: false,
    must_succeed: true,
  }

  # Predecessor uninstall also runs during upgrades. Retain the independent
  # runtime and services; their owner performs verified replacement above.
  uninstall script: { executable: "/usr/bin/true", sudo: false, must_succeed: true }

  app "Agent Job Queue Installer.app", target: "#{Dir.home}/Applications/Agent Job Queue Installer.app"

  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/links/agent-heavy"
  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/links/agent-remote"

  caveats <<~EOS
    Installation and upgrades run automatically, including verified setup repairs.
    Compatible upgrades preserve ongoing jobs; older runtimes wait for the queue to drain.
    CLI links are ready on success.
    macOS may require background-service approval in Login Items settings.
    Homebrew uninstall retains the independent Agent Queue app, services and private data.
    To remove Agent Queue, use its explicit service-removal command before deleting the app.
    Release installer and app are Developer ID signed and notarized. Quarantine is retained.
  EOS
end
`
