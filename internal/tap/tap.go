// Package tap publishes the Homebrew cask and the Sparkle appcast to this
// repository's main branch after a verified release. It reads only the fixed
// public release repository and never executes release code.
package tap

import (
	"bytes"
	"context"
	"crypto/ed25519"
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
	"strconv"
	"strings"
	"time"

	"github.com/Dunky13/agent-job-queue/internal/backend"
	"github.com/Dunky13/agent-job-queue/internal/tooling"
)

const repository = backend.ReleaseRepository
const branch = "main"
const caskPath = "Casks/agent-job-queue.rb"
const manifestPath = "Casks/agent-job-queue.json"
const appcastPath = "appcast.xml"
const manifestAsset = "agent-job-queue.json"
const metadataLimit = 16384
const appcastLimit = 65536
const publishAttempts = 4
const docsPointer = "See docs/homebrew-delivery.md."
const keyVariable = "SPARKLE_ED_PRIVATE_KEY"

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

// tagCommit names a release by its tag and full source commit.
type tagCommit struct{ Tag, Commit string }

func (t tagCommit) validate() error {
	if (t.Tag == "") != (t.Commit == "") {
		return errors.New("Expected tag and commit must be supplied together")
	}
	if t.Tag == "" {
		return nil
	}
	match := tagPattern.FindStringSubmatch(t.Tag)
	if match == nil || !commitPattern.MatchString(t.Commit) || match[2] != t.Commit[:7] {
		return errors.New("Invalid expected release identity")
	}
	return nil
}

// bot is the Homebrew-sync App identity that authors and signs off commits.
type bot struct{ Name, Email string }

func (b bot) valid() bool {
	return botNamePattern.MatchString(b.Name) && botEmailPattern.MatchString(b.Email)
}
func (b bot) signoff() string { return "Signed-off-by: " + b.Name + " <" + b.Email + ">" }

// GitHub API shapes. GitHub adds fields freely, so unknown fields are ignored;
// duplicate fields and malformed JSON are rejected by decode.
type release struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Draft           *bool  `json:"draft"`
	Assets          []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func (r release) published() bool { return r.Draft != nil && !*r.Draft }
func (r release) count(name string) int {
	total := 0
	for _, asset := range r.Assets {
		if asset.Name == name {
			total++
		}
	}
	return total
}

type sha struct {
	SHA string `json:"sha"`
}
type branchRef struct {
	Object sha `json:"object"`
}
type comparison struct {
	Status          string `json:"status"`
	MergeBaseCommit sha    `json:"merge_base_commit"`
}
type commitResponse struct {
	SHA     string `json:"sha"`
	Parents []sha  `json:"parents"`
	Commit  struct {
		Author struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Verification struct {
			Verified bool `json:"verified"`
		} `json:"verification"`
		Message string `json:"message"`
	} `json:"commit"`
}

// Typed createCommitOnBranch input.
type commitInput struct {
	Branch struct {
		Repository string `json:"repositoryNameWithOwner"`
		Name       string `json:"branchName"`
	} `json:"branch"`
	ExpectedHead string `json:"expectedHeadOid"`
	Message      struct {
		Headline string `json:"headline"`
		Body     string `json:"body"`
	} `json:"message"`
	FileChanges struct {
		Additions []fileAddition `json:"additions"`
	} `json:"fileChanges"`
}
type fileAddition struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

type files map[string][]byte

type delivery struct {
	ctx          context.Context
	output       io.Writer
	checkDir     string            // directory holding test-cask.rb
	signTool     string            // Sparkle sign_update
	signKey      string            // EdDSA private key, passed on stdin only
	publicKey    ed25519.PublicKey // the key the app trusts
	bot          bot
	command      func([]string, []byte) ([]byte, error)
	api          func(string) ([]byte, error)
	raw          func(string) ([]byte, error)
	createCommit func(commitInput) (string, error)
}

func newDelivery(ctx context.Context, checkDir string, output io.Writer) *delivery {
	d := &delivery{ctx: ctx, checkDir: checkDir, output: output}
	d.command = d.runCommand
	d.api = func(endpoint string) ([]byte, error) { return d.gh(endpoint, false) }
	d.raw = func(endpoint string) ([]byte, error) { return d.gh(endpoint, true) }
	d.createCommit = d.graphqlCommit
	return d
}

// childEnvironment gives only gh the GitHub token. Every other tool, brew and
// sign_update included, gets an explicit minimal environment, so neither the
// token nor the Sparkle key can reach them.
func childEnvironment(tool string) []string {
	var result []string
	if filepath.Base(tool) == "gh" {
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, keyVariable+"=") {
				result = append(result, entry)
			}
		}
		return result
	}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "USER", "LOGNAME"} {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "HOMEBREW_") && !strings.Contains(name, "TOKEN") && !strings.Contains(name, "KEY") {
			result = append(result, entry)
		}
	}
	return result
}

func (d *delivery) runCommand(arguments []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Env = childEnvironment(arguments[0])
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

func (d *delivery) gh(endpoint string, raw bool) ([]byte, error) {
	if !strings.HasPrefix(endpoint, "repos/"+repository+"/") {
		return nil, errors.New("Unexpected GitHub repository")
	}
	arguments := []string{"gh", "api", "--hostname", "github.com"}
	if raw {
		arguments = append(arguments, "-H", "Accept: application/vnd.github.raw+json")
	}
	return d.command(append(arguments, endpoint), nil)
}
func get[T any](d *delivery, endpoint string) (T, error) {
	body, err := d.api(endpoint)
	if err != nil {
		var zero T
		return zero, err
	}
	return decode[T](body)
}

func (d *delivery) graphqlCommit(input commitInput) (string, error) {
	request, err := json.Marshal(map[string]any{"query": "mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }", "variables": map[string]any{"input": input}})
	if err != nil {
		return "", err
	}
	body, err := d.command([]string{"gh", "api", "--hostname", "github.com", "graphql", "--input", "-"}, request)
	if err != nil {
		return "", err
	}
	response, err := decode[struct {
		Errors json.RawMessage `json:"errors"`
		Data   struct {
			CreateCommitOnBranch struct {
				Commit struct {
					OID string `json:"oid"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
	}](body)
	if err != nil {
		return "", err
	}
	if len(response.Errors) != 0 && string(response.Errors) != "null" {
		return "", errors.New("GitHub GraphQL operation failed")
	}
	return response.Data.CreateCommitOnBranch.Commit.OID, nil
}

// publishedReleases lists public releases through a bounded paginated search.
func (d *delivery) publishedReleases() ([]release, error) {
	var releases []release
	for page := 1; page <= 10; page++ {
		body, err := d.api(fmt.Sprintf("repos/%s/releases?per_page=100&page=%d", repository, page))
		if err != nil {
			return nil, err
		}
		if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("[")) {
			return nil, errors.New("Unexpected source release response")
		}
		batch, err := decode[[]release](body)
		if err != nil || len(batch) > 100 {
			return nil, errors.New("Unexpected source release response")
		}
		releases = append(releases, batch...)
		if len(batch) < 100 {
			return releases, nil
		}
	}
	return nil, errors.New("Public release listing exceeds its page bound; cannot prove the latest build")
}

// numberedBuild returns the build of a published release carrying Homebrew metadata.
func numberedBuild(r release) (int, bool) {
	match := tagPattern.FindStringSubmatch(r.TagName)
	if match == nil || !r.published() || r.count(manifestAsset) == 0 {
		return 0, false
	}
	build, _ := strconv.Atoi(match[1])
	return build, true
}

func selectRelease(releases []release) (release, error) {
	var newest release
	latest := 0
	for _, r := range releases {
		if build, ok := numberedBuild(r); ok && build > latest {
			newest, latest = r, build
		}
	}
	if latest == 0 {
		return release{}, errors.New("No published app build with Homebrew metadata exists")
	}
	for _, r := range releases {
		if build, ok := numberedBuild(r); ok && build == latest && r.TagName != newest.TagName {
			return release{}, errors.New("Multiple public releases claim the latest build number")
		}
	}
	return newest, nil
}
func (d *delivery) releaseByTag(tag string) (release, error) {
	value, err := get[release](d, "repos/"+repository+"/releases/tags/"+tag)
	if err != nil {
		return release{}, err
	}
	if !value.published() || value.TagName != tag {
		return release{}, errors.New("Expected public release is missing or has a different identity")
	}
	return value, nil
}
func (d *delivery) verifySuccessor(older, newer manifest) error {
	if newer.Build <= older.Build || newer.Commit == older.Commit {
		return nil
	}
	value, err := get[comparison](d, "repos/"+repository+"/compare/"+older.Commit+"..."+newer.Commit)
	if err != nil {
		return err
	}
	if value.Status != "ahead" || value.MergeBaseCommit.SHA != older.Commit {
		return errors.New("Newer build is not a verified descendant of the previous release")
	}
	return nil
}

type checkedRelease struct {
	data manifest
	zip  string
}

// downloadRelease verifies a public release against its manifest and the ZIP checksum.
func (d *delivery) downloadRelease(value release, work string) (checkedRelease, error) {
	if !tagPattern.MatchString(value.TagName) {
		return checkedRelease{}, errors.New("Invalid public release tag")
	}
	destination := filepath.Join(work, value.TagName)
	if err := os.Mkdir(destination, 0700); err != nil {
		return checkedRelease{}, err
	}
	download := func(name string) error {
		_, err := d.command([]string{"gh", "release", "download", value.TagName, "--repo", repository, "--pattern", name, "--dir", destination}, nil)
		return err
	}
	if err := download(manifestAsset); err != nil {
		return checkedRelease{}, err
	}
	body, err := boundedFile(filepath.Join(destination, manifestAsset), metadataLimit)
	if err != nil {
		return checkedRelease{}, err
	}
	data, err := parseManifest(body)
	if err != nil {
		return checkedRelease{}, err
	}
	if !value.published() || value.TagName != data.Tag || value.TargetCommitish != data.Commit {
		return checkedRelease{}, errors.New("Release identity does not match manifest")
	}
	if value.count(manifestAsset) != 1 || value.count(data.Asset) != 1 {
		return checkedRelease{}, errors.New("Release does not contain unique expected metadata and app assets")
	}
	if err := download(data.Asset); err != nil {
		return checkedRelease{}, err
	}
	archive := filepath.Join(destination, data.Asset)
	if err := verifyArchive(archive, data); err != nil {
		return checkedRelease{}, err
	}
	return checkedRelease{data, archive}, nil
}

func verifyArchive(path string, data manifest) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != data.SHA256 {
		return errors.New("App archive checksum does not match its manifest")
	}
	return nil
}

// bindArtifact requires the notarized artifact of this run to carry exactly the
// public manifest, SHA-256 included, so a replaced release asset is never signed.
func bindArtifact(directory string, public checkedRelease) error {
	body, err := boundedFile(filepath.Join(directory, manifestAsset), metadataLimit)
	if err != nil {
		return err
	}
	artifact, err := parseManifest(body)
	if err != nil {
		return err
	}
	if artifact != public.data {
		return errors.New("The notarized artifact manifest differs from the public release manifest")
	}
	if err := verifyArchive(filepath.Join(directory, artifact.Asset), artifact); err != nil {
		return errors.New("The notarized artifact archive differs from the public release archive")
	}
	return nil
}

// checkCask loads the cask through the real Homebrew DSL.
func (d *delivery) checkCask(cask, work string) error {
	path := filepath.Join(work, "agent-job-queue.rb")
	if err := os.WriteFile(path, []byte(cask), 0600); err != nil {
		return err
	}
	_, err := d.command([]string{"brew", "ruby", filepath.Join(d.checkDir, "test-cask.rb"), path}, nil)
	return err
}

// sign never receives the key as an argument: it is written to stdin only.
func (d *delivery) sign(arguments ...string) ([]byte, error) {
	if d.signKey == "" || d.signTool == "" {
		return nil, errors.New("The Sparkle signing tool and " + keyVariable + " are required. " + docsPointer)
	}
	return d.command(append([]string{d.signTool, "--ed-key-file", "-"}, arguments...), []byte(d.signKey+"\n"))
}
func (d *delivery) signedAppcast(data manifest, archive, work string) (string, error) {
	output, err := d.sign("-p", archive)
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
	path := filepath.Join(work, appcastPath)
	if err := os.WriteFile(path, []byte(renderAppcast(data, info.Size(), signature)), 0600); err != nil {
		return "", err
	}
	if _, err := d.sign(path); err != nil {
		return "", err
	}
	// Sparkle re-serializes the XML and appends its signature comment.
	signed, err := boundedFile(path, appcastLimit)
	if err != nil {
		return "", err
	}
	if err := signedFeedMatches(signed, expectedItem(data, info.Size(), signature)); err != nil {
		return "", err
	}
	return string(signed), nil
}

func (d *delivery) mainHead() (string, error) {
	value, err := get[branchRef](d, "repos/"+repository+"/git/ref/heads/"+branch)
	if err != nil {
		return "", err
	}
	if !commitPattern.MatchString(value.Object.SHA) {
		return "", errors.New("Unexpected main branch reference")
	}
	return value.Object.SHA, nil
}

// published reads the three delivery files at one commit; absent files are omitted.
func (d *delivery) published(ref string) (files, error) {
	result := files{}
	for path, limit := range map[string]int{caskPath: metadataLimit, manifestPath: metadataLimit, appcastPath: appcastLimit} {
		body, err := d.raw("repos/" + repository + "/contents/" + path + "?ref=" + ref)
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

func (d *delivery) commit(data manifest, want files, head string) (string, error) {
	var input commitInput
	input.Branch.Repository, input.Branch.Name = repository, branch
	input.ExpectedHead = head
	input.Message.Headline = fmt.Sprintf("chore(release): publish build %d", data.Build)
	input.Message.Body = d.bot.signoff()
	for _, path := range []string{caskPath, manifestPath, appcastPath} {
		input.FileChanges.Additions = append(input.FileChanges.Additions, fileAddition{path, base64.StdEncoding.EncodeToString(want[path])})
	}
	oid, err := d.createCommit(input)
	if err != nil {
		return "", err
	}
	if !commitPattern.MatchString(oid) {
		return "", errors.New("Publication returned an invalid commit identity")
	}
	return oid, nil
}

// verifySigned checks a commit's Verified signature, App author and DCO trailer,
// and its parent when one is expected.
func (d *delivery) verifySigned(oid, parent string) error {
	value, err := get[commitResponse](d, "repos/"+repository+"/commits/"+oid)
	if err != nil {
		return err
	}
	author := value.Commit.Author
	signed := false
	for _, line := range strings.Split(value.Commit.Message, "\n") {
		signed = signed || line == d.bot.signoff()
	}
	if value.SHA != oid || !value.Commit.Verification.Verified || author.Name != d.bot.Name || author.Email != d.bot.Email || !signed {
		return fmt.Errorf("Published commit %s failed Verified signature or matching DCO verification", oid)
	}
	if parent != "" && (len(value.Parents) != 1 || value.Parents[0].SHA != parent) {
		return fmt.Errorf("Published commit %s does not extend the expected main head", oid)
	}
	return nil
}

// verifyCommit reads a new commit back by SHA, including the exact content.
func (d *delivery) verifyCommit(oid, parent string, want files) error {
	if err := d.verifySigned(oid, parent); err != nil {
		return err
	}
	got, err := d.published(oid)
	if err != nil {
		return err
	}
	if !sameFiles(got, want) {
		return fmt.Errorf("Published commit %s content differs from the verified delivery", oid)
	}
	fmt.Fprintf(d.output, "Published verified, DCO-signed commit %s on %s.\n", oid, branch)
	return nil
}

// verifyExisting covers a lost commit response: identical files already on main
// are accepted only when the commits that last changed them are themselves verified.
func (d *delivery) verifyExisting(head string) error {
	seen := map[string]bool{}
	for _, path := range []string{caskPath, manifestPath, appcastPath} {
		latest, err := get[[]sha](d, "repos/"+repository+"/commits?sha="+head+"&path="+path+"&per_page=1")
		if err != nil || len(latest) != 1 || !commitPattern.MatchString(latest[0].SHA) {
			return errors.New("Could not identify the commit that last changed " + path)
		}
		if seen[latest[0].SHA] {
			continue
		}
		seen[latest[0].SHA] = true
		if err := d.verifySigned(latest[0].SHA, ""); err != nil {
			return err
		}
	}
	fmt.Fprintln(d.output, "Delivery is already current; release, cask, feed and their signed commits verified. No commit created.")
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

// sameDelivery compares the cask and manifest exactly and the feed semantically.
func sameDelivery(state, want files) bool {
	feed, ok := state[appcastPath]
	return ok && bytes.Equal(state[caskPath], want[caskPath]) && bytes.Equal(state[manifestPath], want[manifestPath]) && len(state) == len(want) && sameFeed(feed, want[appcastPath])
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

// session holds one run's scratch directory and verified-release cache.
type session struct {
	d     *delivery
	work  string
	cache map[string]checkedRelease
}

func (s *session) checked(tag string) (checkedRelease, error) {
	if result, ok := s.cache[tag]; ok {
		return result, nil
	}
	value, err := s.d.releaseByTag(tag)
	if err != nil {
		return checkedRelease{}, err
	}
	result, err := s.d.downloadRelease(value, s.work)
	if err != nil {
		return checkedRelease{}, err
	}
	s.cache[tag] = result
	return result, nil
}

// selection is the verified build this run delivers; later is a newer public
// build when this run is superseded.
type selection struct {
	target checkedRelease
	later  *manifest
}

// selectBuild verifies the requested release and the latest public build.
func (s *session) selectBuild(want tagCommit) (selection, error) {
	releases, err := s.d.publishedReleases()
	if err != nil {
		return selection{}, err
	}
	top, err := selectRelease(releases)
	if err != nil {
		return selection{}, err
	}
	newest, err := s.checked(top.TagName)
	if err != nil {
		return selection{}, err
	}
	result := selection{target: newest}
	if want.Tag == "" {
		return result, nil
	}
	requested, err := s.checked(want.Tag)
	if err != nil {
		return selection{}, err
	}
	if requested.data.Commit != want.Commit {
		return selection{}, errors.New("Expected commit does not match its public release")
	}
	if newest.data.Build < requested.data.Build {
		return selection{}, errors.New("Latest public release is older than the requested delivery")
	}
	if newest.data.Build == requested.data.Build && newest.data != requested.data {
		return selection{}, errors.New("Requested build has conflicting public release identities")
	}
	if err := s.d.verifySuccessor(requested.data, newest.data); err != nil {
		return selection{}, err
	}
	result.target = requested
	if newest.data.Build > requested.data.Build {
		result.later = &newest.data
	}
	return result, nil
}

// rejectOrYield compares main's current build with the one being delivered. It
// reports true when main already holds a newer build, which a concurrent newer
// run delivered after this run was selected. That build was just verified as a
// public release above, so it is never treated as a regression.
func (s *session) rejectOrYield(current *manifest, sel selection) (bool, error) {
	if current == nil {
		return false, nil
	}
	published, err := s.checked(current.Tag)
	if err != nil {
		return false, err
	}
	if *current != published.data {
		return false, errors.New("Current main metadata differs from its validated public release")
	}
	data := sel.target.data
	switch {
	case current.Build > data.Build:
		return true, nil
	case current.Build == data.Build && *current != data:
		return false, errors.New("The same build number has conflicting release metadata")
	case current.Build < data.Build:
		return false, s.d.verifySuccessor(*current, data)
	}
	return false, nil
}

// build renders and checks the cask, then signs the ZIP and the feed.
func (s *session) build(target checkedRelease) (files, error) {
	data := target.data
	cask, err := renderCask(data)
	if err != nil {
		return nil, err
	}
	if err := s.d.checkCask(cask, s.work); err != nil {
		return nil, err
	}
	appcast, err := s.d.signedAppcast(data, target.zip, s.work)
	if err != nil {
		return nil, err
	}
	archive, err := os.ReadFile(target.zip)
	if err != nil {
		return nil, err
	}
	if err := verifySignatures(s.d.publicKey, archive, []byte(appcast)); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, err
	}
	return files{caskPath: []byte(cask), manifestPath: append(body, '\n'), appcastPath: []byte(appcast)}, nil
}

// deliver commits the files, re-reading main and retrying when it moved.
func (s *session) deliver(sel selection, want files) error {
	d := s.d
	data := sel.target.data
	for attempt := 1; ; attempt++ {
		head, err := d.mainHead()
		if err != nil {
			return err
		}
		state, err := d.published(head)
		if err != nil {
			return err
		}
		current, err := currentManifest(state)
		if err != nil {
			return err
		}
		yield, err := s.rejectOrYield(current, sel)
		if err != nil {
			return err
		}
		if yield {
			fmt.Fprintf(d.output, "Superseded: main already holds build %d, newer than build %d.\n", current.Build, data.Build)
			return nil
		}
		if sameDelivery(state, want) {
			return d.verifyExisting(head)
		}
		oid, err := d.commit(data, want, head)
		if err == nil {
			return d.verifyCommit(oid, head, want)
		}
		moved, headErr := d.mainHead()
		if headErr != nil || moved == head || attempt >= publishAttempts {
			return errors.New("Publishing to main failed. Check the Homebrew-sync App installation, its contents write permission and its ruleset bypass. " + docsPointer)
		}
		fmt.Fprintf(d.output, "main moved to %s during publication; retrying (attempt %d of %d).\n", moved, attempt+1, publishAttempts)
	}
}

type request struct {
	want     tagCommit
	artifact string // directory of this run's notarized artifact
	publish  bool
}

func (d *delivery) refresh(req request) error {
	if err := req.want.validate(); err != nil {
		return err
	}
	if req.publish && (req.want.Tag == "" || req.artifact == "") {
		return errors.New("Publication requires --expected-tag, --expected-commit and --artifact-dir")
	}
	work, err := os.MkdirTemp("", "agent-queue-tap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	s := &session{d: d, work: work, cache: map[string]checkedRelease{}}
	sel, err := s.selectBuild(req.want)
	if err != nil {
		return err
	}
	if sel.later != nil {
		fmt.Fprintf(d.output, "Superseded by build %d (%s); this run delivers nothing and the newer run publishes it.\n", sel.later.Build, sel.later.Tag)
		return nil
	}
	if req.artifact != "" {
		if err := bindArtifact(req.artifact, sel.target); err != nil {
			return err
		}
	}
	if !req.publish {
		cask, err := renderCask(sel.target.data)
		if err != nil {
			return err
		}
		if err := d.checkCask(cask, work); err != nil {
			return err
		}
		fmt.Fprintf(d.output, "Validated %s, its app archive checksum and the cask; publication disabled.\n", sel.target.data.Tag)
		return nil
	}
	want, err := s.build(sel.target)
	if err != nil {
		return err
	}
	return s.deliver(sel, want)
}

func publicationAllowed(environment map[string]string) bool {
	return environment["GITHUB_ACTIONS"] == "true" && environment["GITHUB_REPOSITORY"] == repository && environment["GITHUB_REF"] == "refs/heads/"+branch
}

func Run(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("queue-tap", flag.ContinueOnError)
	publish := flags.Bool("publish", false, "Sign, commit and verify the delivery on main")
	tag := flags.String("expected-tag", "", "Requested public release tag")
	commit := flags.String("expected-commit", "", "Requested full source commit")
	artifact := flags.String("artifact-dir", "", "Directory of this run's notarized artifact")
	checkDir := flags.String("check-dir", "packaging/homebrew", "Directory containing test-cask.rb")
	signTool := flags.String("sign-update", "", "Path to Sparkle sign_update")
	publicKey := flags.String("public-key", "packaging/sparkle/public-ed-key.txt", "File holding the base64 Sparkle public key the app trusts")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("Unexpected delivery arguments")
	}
	d := newDelivery(context.Background(), *checkDir, output)
	if *publish {
		environment := map[string]string{}
		for _, name := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_REF"} {
			environment[name] = os.Getenv(name)
		}
		if !publicationAllowed(environment) {
			return errors.New("Publication is restricted to the distribution workflow on main")
		}
		d.signTool, d.signKey = *signTool, strings.TrimSpace(os.Getenv(keyVariable))
		// The key travels on signer stdin only, so no child inherits it.
		_ = os.Unsetenv(keyVariable)
		d.bot = bot{os.Getenv("TAP_BOT_NAME"), os.Getenv("TAP_BOT_EMAIL")}
		if d.signKey == "" || d.signTool == "" {
			return errors.New(keyVariable + " and --sign-update are required to publish. " + docsPointer)
		}
		if !d.bot.valid() {
			return errors.New("TAP_BOT_NAME and TAP_BOT_EMAIL must name the Homebrew-sync App bot. " + docsPointer)
		}
		key, err := loadPublicKey(*publicKey)
		if err != nil {
			return err
		}
		d.publicKey = key
	}
	return d.refresh(request{tagCommit{*tag, *commit}, *artifact, *publish})
}

func Main(arguments []string) int {
	if err := Run(arguments, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
