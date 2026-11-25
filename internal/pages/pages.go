// Package pages prepares and verifies the GitHub Pages deployment that serves
// the Sparkle feed, latest.json and the homepage after a verified release. It
// reads only the fixed public release repository and never executes release code.
package pages

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
)

const repository = backend.ReleaseRepository
const branch = "main"
const appcastName = "appcast.xml"
const latestName = "latest.json"
const manifestAsset = "agent-job-queue.json"
const metadataLimit = 16384
const appcastLimit = 65536
const docsPointer = "See docs/pages-delivery.md."
const keyVariable = "SPARKLE_ED_PRIVATE_KEY"

// siteURL is where Pages serves this project. Sparkle reads siteURL + "/appcast.xml".
const siteURL = "https://dunky13.github.io/agent-job-queue"

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var tagPattern = regexp.MustCompile(`^build-([1-9][0-9]{0,8})-([0-9a-f]{7})$`)
var signaturePattern = regexp.MustCompile(`^[A-Za-z0-9+/]{86}==$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

var errNotFound = errors.New("Resource was not found")

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

// diskImage names the DMG that ships beside the manifest's ZIP.
func (m manifest) diskImage() string { return strings.TrimSuffix(m.Asset, ".zip") + ".dmg" }

// tagCommit names a release by its tag and full source commit.
type tagCommit struct{ Tag, Commit string }

func (t tagCommit) validate() error {
	match := tagPattern.FindStringSubmatch(t.Tag)
	if match == nil || !commitPattern.MatchString(t.Commit) || match[2] != t.Commit[:7] {
		return errors.New("Invalid expected release identity")
	}
	return nil
}

// GitHub API shapes. GitHub adds fields freely, so unknown fields are ignored;
// duplicate fields and malformed JSON are rejected by decode.
type release struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	PublishedAt     string `json:"published_at"`
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
type comparison struct {
	Status          string `json:"status"`
	MergeBaseCommit sha    `json:"merge_base_commit"`
}

type delivery struct {
	ctx      context.Context
	output   io.Writer
	signTool string            // Sparkle sign_update
	signKey  string            // EdDSA private key, passed on stdin only
	public   ed25519.PublicKey // the key the app trusts
	command  func([]string, []byte) ([]byte, error)
	api      func(string) ([]byte, error)
	fetch    func(string) ([]byte, error) // live site reads; errNotFound for HTTP 404
	attempts int                          // live read-back attempts
	pause    func()
}

func newDelivery(ctx context.Context, output io.Writer) *delivery {
	d := &delivery{ctx: ctx, output: output, attempts: 18}
	d.command = d.runCommand
	d.api = func(endpoint string) ([]byte, error) { return d.gh(endpoint) }
	d.fetch = liveFetcher(nil)
	d.pause = func() { time.Sleep(10 * time.Second) }
	return d
}

// childEnvironment gives only gh the GitHub token. Every other tool, sign_update
// included, gets an explicit minimal environment, so neither the token nor the
// Sparkle key can reach them.
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

func (d *delivery) gh(endpoint string) ([]byte, error) {
	if !strings.HasPrefix(endpoint, "repos/"+repository+"/") {
		return nil, errors.New("Unexpected GitHub repository")
	}
	return d.command([]string{"gh", "api", "--hostname", "github.com", endpoint}, nil)
}
func get[T any](d *delivery, endpoint string) (T, error) {
	body, err := d.api(endpoint)
	if err != nil {
		var zero T
		return zero, err
	}
	return decode[T](body)
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

// numberedBuild returns the build of a published release carrying release metadata.
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
		return release{}, errors.New("No published app build with release metadata exists")
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

// checkedRelease is a public release verified against its manifest and checksums.
type checkedRelease struct {
	data        manifest
	zip         string
	dmgSHA256   string
	publishedAt string
}

// downloadRelease verifies a public release against its manifest, the ZIP
// checksum and the published DMG checksum file.
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
	published, err := time.Parse(time.RFC3339, value.PublishedAt)
	if err != nil {
		return checkedRelease{}, errors.New("Public release has no valid publication time")
	}
	image := data.diskImage()
	for _, name := range []string{manifestAsset, data.Asset, image, image + ".sha256"} {
		if value.count(name) != 1 {
			return checkedRelease{}, errors.New("Release does not contain unique expected metadata and app assets")
		}
	}
	if err := download(data.Asset); err != nil {
		return checkedRelease{}, err
	}
	archive := filepath.Join(destination, data.Asset)
	if err := verifyFileHash(archive, data.SHA256); err != nil {
		return checkedRelease{}, errors.New("App archive checksum does not match its manifest")
	}
	if err := download(image + ".sha256"); err != nil {
		return checkedRelease{}, err
	}
	dmgSHA256, err := readChecksum(filepath.Join(destination, image+".sha256"), image)
	if err != nil {
		return checkedRelease{}, err
	}
	return checkedRelease{data, archive, dmgSHA256, published.UTC().Format(time.RFC3339)}, nil
}

// readChecksum parses a `sha256sum` style file for exactly one named file.
func readChecksum(path, name string) (string, error) {
	body, err := boundedFile(path, 1024)
	if err != nil {
		return "", err
	}
	sum, found := strings.CutSuffix(string(body), "  "+name+"\n")
	if !found || !sha256Pattern.MatchString(sum) {
		return "", errors.New("Disk image checksum file is malformed")
	}
	return sum, nil
}

func verifyFileHash(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != want {
		return errors.New("Checksum mismatch")
	}
	return nil
}

// bindArtifact requires the notarized artifact of this run to carry exactly the
// public manifest, ZIP and DMG, so a replaced release asset is never signed.
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
	if err := verifyFileHash(filepath.Join(directory, artifact.Asset), artifact.SHA256); err != nil {
		return errors.New("The notarized artifact archive differs from the public release archive")
	}
	image := artifact.diskImage()
	sum, err := readChecksum(filepath.Join(directory, image+".sha256"), image)
	if err != nil || sum != public.dmgSHA256 || verifyFileHash(filepath.Join(directory, image), sum) != nil {
		return errors.New("The notarized artifact disk image differs from the public release disk image")
	}
	return nil
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
	path := filepath.Join(work, appcastName)
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
	result := selection{target: requested}
	if newest.data.Build > requested.data.Build {
		result.later = &newest.data
	}
	return result, nil
}

func publicationAllowed(environment map[string]string) bool {
	return environment["GITHUB_ACTIONS"] == "true" && environment["GITHUB_REPOSITORY"] == repository && environment["GITHUB_REF"] == "refs/heads/"+branch
}
