// Package tap publishes the Homebrew cask and the Sparkle appcast to this
// repository's main branch after a verified release. It reads only the fixed
// public release repository and never executes release code.
package tap

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
	"html"
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

const repository = backend.ReleaseRepository
const branch = "main"
const caskPath = "Casks/agent-job-queue.rb"
const manifestPath = "Casks/agent-job-queue.json"
const appcastPath = "appcast.xml"
const metadataLimit = 16384
const appcastLimit = 65536
const publishAttempts = 4
const docsPointer = "See docs/homebrew-delivery.md."

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var tagPattern = regexp.MustCompile(`^build-([1-9][0-9]{0,8})-([0-9a-f]{7})$`)
var signaturePattern = regexp.MustCompile(`^[A-Za-z0-9+/]{86}==$`)
var botNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*\[bot\]$`)
var botEmailPattern = regexp.MustCompile(`^[0-9]+\+[A-Za-z0-9][A-Za-z0-9-]*\[bot\]@users\.noreply\.github\.com$`)

var errNotFound = errors.New("GitHub resource was not found")

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

// parseManifest validates with the release schema the app shares.
func parseManifest(body []byte) (manifest, error) {
	if len(body) > metadataLimit {
		return manifest{}, errors.New("Release manifest exceeds its size limit")
	}
	value, err := backend.ValidateManifest(body)
	if err != nil {
		return manifest{}, err
	}
	return manifest{value.SchemaVersion, value.Repository, value.Version, *value.Commit, *value.Build, value.Tag, value.Asset, value.SHA256}, nil
}
func (m manifest) release() backend.Manifest {
	return backend.Manifest{BuildInfo: backend.BuildInfo{SchemaVersion: m.SchemaVersion, Repository: m.Repository, Version: m.Version, Commit: &m.Commit, Build: &m.Build}, Tag: m.Tag, Asset: m.Asset, SHA256: m.SHA256}
}
func renderCask(data manifest) (string, error) { return tooling.RenderCask(data.release()) }

// renderAppcast renders the unsigned single-item feed for the published ZIP.
func renderAppcast(data manifest, length int64, signature string) string {
	release := "https://github.com/" + repository + "/releases"
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>Agent Queue</title>
    <link>%[1]s</link>
    <description>Agent Queue updates</description>
    <language>en</language>
    <item>
      <title>Version %[2]s (%[3]d)</title>
      <link>%[1]s/tag/%[4]s</link>
      <sparkle:version>%[3]d</sparkle:version>
      <sparkle:shortVersionString>%[2]s</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>15.0</sparkle:minimumSystemVersion>
      <enclosure url="%[1]s/download/%[4]s/%[5]s" length="%[6]d" type="application/octet-stream" sparkle:edSignature="%[7]s"/>
    </item>
  </channel>
</rss>
`, release, html.EscapeString(data.Version), data.Build, html.EscapeString(data.Tag), html.EscapeString(data.Asset), length, signature)
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

type files map[string][]byte

type publisher struct {
	ctx      context.Context
	output   io.Writer
	checkDir string // directory holding test-cask.rb
	signTool string // Sparkle sign_update
	signKey  string // EdDSA private key, passed on stdin only
	botName  string
	botEmail string
	command  func([]string, []byte) ([]byte, error)
	api      func(string) (any, error)
	raw      func(string) ([]byte, error)
	graphql  func(string, map[string]any) (map[string]any, error)
}

func newPublisher(ctx context.Context, checkDir string, output io.Writer) *publisher {
	f := &publisher{ctx: ctx, checkDir: checkDir, output: output}
	f.command = f.runCommand
	f.api = f.request
	f.raw = f.rawFile
	f.graphql = f.graphQL
	return f
}
func (f *publisher) runCommand(arguments []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(f.ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	diagnostics := &boundedOutput{limit: 4096}
	cmd.Stderr = diagnostics
	output := &boundedOutput{limit: 2 << 20}
	cmd.Stdout = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("Required external operation exceeded its deadline")
	}
	if err != nil {
		// Only a GitHub 404 is classified; other output is never surfaced, so keys and tokens stay out of logs.
		if filepath.Base(arguments[0]) == "gh" && strings.Contains(diagnostics.String(), "HTTP 404") {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("Required %s operation failed", filepath.Base(arguments[0]))
	}
	return output.Bytes(), nil
}

func (f *publisher) request(endpoint string) (any, error) {
	if !strings.HasPrefix(endpoint, "repos/"+repository+"/") {
		return nil, errors.New("Unexpected GitHub repository")
	}
	body, err := f.command([]string{"gh", "api", "--hostname", "github.com", endpoint}, nil)
	if err != nil {
		return nil, err
	}
	return strictJSON(body)
}
func (f *publisher) rawFile(endpoint string) ([]byte, error) {
	if !strings.HasPrefix(endpoint, "repos/"+repository+"/") {
		return nil, errors.New("Unexpected GitHub repository")
	}
	return f.command([]string{"gh", "api", "--hostname", "github.com", "-H", "Accept: application/vnd.github.raw+json", endpoint}, nil)
}
func (f *publisher) graphQL(query string, variables map[string]any) (map[string]any, error) {
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
func (f *publisher) publishedReleases() ([]map[string]any, error) {
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
func (f *publisher) releaseByTag(tag string) (map[string]any, error) {
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
func (f *publisher) verifySuccessor(older, newer manifest) error {
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

type checkedRelease struct {
	data    manifest
	zip     string
	release map[string]any
}

// downloadRelease verifies a public release against its manifest and the ZIP checksum.
func (f *publisher) downloadRelease(release map[string]any, work string) (checkedRelease, error) {
	tag := asString(release["tag_name"])
	if !tagPattern.MatchString(tag) {
		return checkedRelease{}, errors.New("Invalid public release tag")
	}
	destination := filepath.Join(work, tag)
	if err := os.Mkdir(destination, 0700); err != nil {
		return checkedRelease{}, err
	}
	download := func(name string) error {
		_, err := f.command([]string{"gh", "release", "download", tag, "--repo", repository, "--pattern", name, "--dir", destination}, nil)
		return err
	}
	if err := download("agent-job-queue.json"); err != nil {
		return checkedRelease{}, err
	}
	body, err := boundedFile(filepath.Join(destination, "agent-job-queue.json"), metadataLimit)
	if err != nil {
		return checkedRelease{}, err
	}
	data, err := parseManifest(body)
	if err != nil {
		return checkedRelease{}, err
	}
	if release["draft"] != false || release["tag_name"] != data.Tag || release["target_commitish"] != data.Commit {
		return checkedRelease{}, errors.New("Release identity does not match manifest")
	}
	assets, ok := release["assets"].([]any)
	if !ok {
		return checkedRelease{}, errors.New("Unexpected source release assets")
	}
	names := map[string]int{}
	for _, asset := range assets {
		names[asString(asObject(asset)["name"])]++
	}
	if names["agent-job-queue.json"] != 1 || names[data.Asset] != 1 {
		return checkedRelease{}, errors.New("Release does not contain unique expected metadata and app assets")
	}
	if err := download(data.Asset); err != nil {
		return checkedRelease{}, err
	}
	archive := filepath.Join(destination, data.Asset)
	file, err := os.Open(archive)
	if err != nil {
		return checkedRelease{}, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	_ = file.Close()
	if err != nil {
		return checkedRelease{}, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != data.SHA256 {
		return checkedRelease{}, errors.New("Downloaded app archive checksum does not match manifest")
	}
	return checkedRelease{data, archive, release}, nil
}

// checkCask renders the cask and loads it through the real Homebrew DSL.
func (f *publisher) checkCask(cask, work string) error {
	path := filepath.Join(work, "agent-job-queue.rb")
	if err := os.WriteFile(path, []byte(cask), 0600); err != nil {
		return err
	}
	_, err := f.command([]string{"brew", "ruby", filepath.Join(f.checkDir, "test-cask.rb"), path}, nil)
	return err
}

// sign never receives the key as an argument: it is written to stdin only.
func (f *publisher) sign(arguments ...string) ([]byte, error) {
	if f.signKey == "" || f.signTool == "" {
		return nil, errors.New("The Sparkle signing tool and SPARKLE_ED_PRIVATE_KEY are required. " + docsPointer)
	}
	return f.command(append([]string{f.signTool, "--ed-key-file", "-"}, arguments...), []byte(f.signKey+"\n"))
}
func (f *publisher) signedAppcast(data manifest, archive, work string) (string, error) {
	output, err := f.sign("-p", archive)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(archive)
	if err != nil {
		return "", err
	}
	signature := strings.TrimSpace(string(output))
	if !signaturePattern.MatchString(signature) {
		return "", errors.New("The Sparkle signing tool returned an invalid archive signature")
	}
	unsigned := renderAppcast(data, info.Size(), signature)
	path := filepath.Join(work, appcastPath)
	if err := os.WriteFile(path, []byte(unsigned), 0600); err != nil {
		return "", err
	}
	if _, err := f.sign(path); err != nil {
		return "", err
	}
	signed, err := boundedFile(path, appcastLimit)
	if err != nil {
		return "", err
	}
	// Sparkle re-serializes the XML and appends its signature comment, so the
	// signed feed is checked for that signature and for the release identity.
	for _, required := range []string{"sparkle-signatures:", "edSignature: ", fmt.Sprintf("<sparkle:version>%d</sparkle:version>", data.Build), fmt.Sprintf(`sparkle:edSignature="%s"`, signature), fmt.Sprintf("/download/%s/%s\"", data.Tag, data.Asset)} {
		if !bytes.Contains(signed, []byte(required)) {
			return "", errors.New("The Sparkle signing tool did not produce the expected signed feed")
		}
	}
	return string(signed), nil
}

func (f *publisher) mainHead() (string, error) {
	value, err := f.api("repos/" + repository + "/git/ref/heads/" + branch)
	if err != nil {
		return "", err
	}
	sha := asString(asObject(asObject(value)["object"])["sha"])
	if !commitPattern.MatchString(sha) {
		return "", errors.New("Unexpected main branch reference")
	}
	return sha, nil
}

// published reads the three delivery files at one commit; absent files are omitted.
func (f *publisher) published(ref string) (files, error) {
	result := files{}
	for path, limit := range map[string]int{caskPath: metadataLimit, manifestPath: metadataLimit, appcastPath: appcastLimit} {
		body, err := f.raw("repos/" + repository + "/contents/" + path + "?ref=" + ref)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(body) > limit {
			return nil, fmt.Errorf("Published %s exceeds its size limit", path)
		}
		result[path] = body
	}
	return result, nil
}

func (f *publisher) commit(data manifest, want files, head string) (string, error) {
	signoff := "Signed-off-by: " + f.botName + " <" + f.botEmail + ">"
	var additions []map[string]any
	for _, path := range []string{caskPath, manifestPath, appcastPath} {
		additions = append(additions, map[string]any{"path": path, "contents": base64.StdEncoding.EncodeToString(want[path])})
	}
	variables := map[string]any{"input": map[string]any{"branch": map[string]any{"repositoryNameWithOwner": repository, "branchName": branch}, "expectedHeadOid": head, "message": map[string]any{"headline": fmt.Sprintf("chore(release): publish build %d", data.Build), "body": signoff}, "fileChanges": map[string]any{"additions": additions}}}
	result, err := f.graphql("mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }", variables)
	if err != nil {
		return "", err
	}
	sha := asString(asObject(asObject(result["createCommitOnBranch"])["commit"])["oid"])
	if !commitPattern.MatchString(sha) {
		return "", errors.New("Publication returned an invalid commit identity")
	}
	return sha, nil
}

// verifyCommit reads the new commit back by SHA: signature, author, DCO, parent and content.
func (f *publisher) verifyCommit(sha, parent string, want files) error {
	value, err := f.api("repos/" + repository + "/commits/" + sha)
	if err != nil {
		return err
	}
	response := asObject(value)
	commit := asObject(response["commit"])
	author := asObject(commit["author"])
	parents, _ := response["parents"].([]any)
	signoff := "Signed-off-by: " + f.botName + " <" + f.botEmail + ">"
	if response["sha"] != sha || asObject(commit["verification"])["verified"] != true || author["name"] != f.botName || author["email"] != f.botEmail || !slices.Contains(strings.Split(asString(commit["message"]), "\n"), signoff) {
		return fmt.Errorf("Published commit %s failed Verified signature or matching DCO verification", sha)
	}
	if len(parents) != 1 || asObject(parents[0])["sha"] != parent {
		return fmt.Errorf("Published commit %s does not extend the expected main head", sha)
	}
	got, err := f.published(sha)
	if err != nil {
		return err
	}
	if !sameFiles(got, want) {
		return fmt.Errorf("Published commit %s content differs from the verified delivery", sha)
	}
	fmt.Fprintf(f.output, "Published verified, DCO-signed commit %s on %s.\n", sha, branch)
	return nil
}
func sameFiles(a, b files) bool {
	if len(a) != len(b) {
		return false
	}
	for path, body := range a {
		if other, ok := b[path]; !ok || !bytes.Equal(body, other) {
			return false
		}
	}
	return true
}

// currentManifest returns the build already on main, or nil for a first delivery.
func currentManifest(state files) (*manifest, error) {
	body, hasManifest := state[manifestPath]
	_, hasCask := state[caskPath]
	if hasManifest != hasCask {
		return nil, errors.New("Cask metadata and cask must both exist or both be absent")
	}
	if !hasManifest {
		return nil, nil
	}
	item, err := parseManifest(body)
	return &item, err
}

func (f *publisher) refresh(publish bool, expectedTag, expectedCommit string) error {
	if err := expectedIdentity(expectedTag, expectedCommit); err != nil {
		return err
	}
	releases, err := f.publishedReleases()
	if err != nil {
		return err
	}
	latest, err := selectRelease(releases)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "agent-queue-tap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	cache := map[string]checkedRelease{}
	checked := func(tag string) (checkedRelease, error) {
		if result, ok := cache[tag]; ok {
			return result, nil
		}
		release, err := f.releaseByTag(tag)
		if err != nil {
			return checkedRelease{}, err
		}
		result, err := f.downloadRelease(release, work)
		if err != nil {
			return checkedRelease{}, err
		}
		cache[tag] = result
		return result, nil
	}
	var requested checkedRelease
	if expectedTag != "" {
		if requested, err = checked(expectedTag); err != nil {
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
	data := newest.data
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
	cask, err := renderCask(data)
	if err != nil {
		return err
	}
	if err := f.checkCask(cask, work); err != nil {
		return err
	}
	if !publish {
		fmt.Fprintf(f.output, "Validated %s, its app archive checksum and the cask; publication disabled.\n", data.Tag)
		return nil
	}
	appcast, err := f.signedAppcast(data, newest.zip, work)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	want := files{caskPath: []byte(cask), manifestPath: append(body, '\n'), appcastPath: []byte(appcast)}
	for attempt := 1; ; attempt++ {
		head, err := f.mainHead()
		if err != nil {
			return err
		}
		state, err := f.published(head)
		if err != nil {
			return err
		}
		current, err := currentManifest(state)
		if err != nil {
			return err
		}
		if current != nil {
			published, err := checked(current.Tag)
			if err != nil {
				return err
			}
			if *current != published.data {
				return errors.New("Current main metadata differs from its validated public release")
			}
			if current.Build > data.Build {
				return errors.New("Latest public release would downgrade the delivered build")
			}
			if current.Build == data.Build && *current != data {
				return errors.New("The same build number has conflicting release metadata")
			}
			if current.Build < data.Build {
				if err := f.verifySuccessor(*current, data); err != nil {
					return err
				}
			}
		}
		if sameFiles(state, want) {
			fmt.Fprintln(f.output, "Delivery is already current; release, cask and signed feed verified. No commit created.")
			return nil
		}
		sha, err := f.commit(data, want, head)
		if err == nil {
			return f.verifyCommit(sha, head, want)
		}
		moved, headErr := f.mainHead()
		if headErr != nil || moved == head || attempt >= publishAttempts {
			return errors.New("Publishing to main failed. Check the Homebrew-sync App installation, its contents write permission and its ruleset bypass. " + docsPointer)
		}
		fmt.Fprintf(f.output, "main moved to %s during publication; retrying (attempt %d of %d).\n", moved, attempt+1, publishAttempts)
	}
}

func publicationAllowed(environment map[string]string) bool {
	return environment["GITHUB_ACTIONS"] == "true" && environment["GITHUB_REPOSITORY"] == repository && environment["GITHUB_REF"] == "refs/heads/"+branch
}

func Run(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("queue-tap", flag.ContinueOnError)
	publish := flags.Bool("publish", false, "Sign, commit and verify the delivery on main")
	tag := flags.String("expected-tag", "", "Requested public release tag")
	commit := flags.String("expected-commit", "", "Requested full source commit")
	checkDir := flags.String("check-dir", "packaging/homebrew", "Directory containing test-cask.rb")
	signTool := flags.String("sign-update", "", "Path to Sparkle sign_update")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("Unexpected delivery arguments")
	}
	if err := expectedIdentity(*tag, *commit); err != nil {
		return err
	}
	f := newPublisher(context.Background(), *checkDir, output)
	if *publish {
		environment := map[string]string{}
		for _, name := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_REF"} {
			environment[name] = os.Getenv(name)
		}
		if !publicationAllowed(environment) {
			return errors.New("Publication is restricted to the distribution workflow on main")
		}
		f.signTool, f.signKey = *signTool, strings.TrimSpace(os.Getenv("SPARKLE_ED_PRIVATE_KEY"))
		f.botName, f.botEmail = os.Getenv("TAP_BOT_NAME"), os.Getenv("TAP_BOT_EMAIL")
		if f.signKey == "" || f.signTool == "" {
			return errors.New("SPARKLE_ED_PRIVATE_KEY and --sign-update are required to publish. " + docsPointer)
		}
		if !botNamePattern.MatchString(f.botName) || !botEmailPattern.MatchString(f.botEmail) {
			return errors.New("TAP_BOT_NAME and TAP_BOT_EMAIL must name the Homebrew-sync App bot. " + docsPointer)
		}
	}
	return f.refresh(*publish, *tag, *commit)
}

func Main(arguments []string) int {
	if err := Run(arguments, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
