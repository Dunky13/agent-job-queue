package pages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const fileLimit = 8 << 20
const totalLimit = 64 << 20
const countLimit = 1000

// latest is the machine-readable description of the current build that the
// homepage reads; its field order is the published order.
type latest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
	Build         int    `json:"build"`
	Commit        string `json:"commit"`
	Tag           string `json:"tag"`
	PublishedAt   string `json:"publishedAt"`
	ReleaseURL    string `json:"releaseUrl"`
	DMGURL        string `json:"dmgUrl"`
	DMGSHA256     string `json:"dmgSha256"`
	ZIPURL        string `json:"zipUrl"`
	ZIPSHA256     string `json:"zipSha256"`
}

func newLatest(value checkedRelease) latest {
	data := value.data
	return latest{1, data.Version, data.Build, data.Commit, data.Tag, value.publishedAt,
		releasesURL + "/tag/" + data.Tag,
		releasesURL + "/download/" + data.Tag + "/" + data.diskImage(), value.dmgSHA256,
		releasesURL + "/download/" + data.Tag + "/" + data.Asset, data.SHA256}
}

func (l latest) marshal() ([]byte, error) {
	body, err := json.MarshalIndent(l, "", "  ")
	return append(body, '\n'), err
}

// parseLatest reads a latest.json strictly: no unknown or duplicate fields, and
// every derived field must agree with the build identity.
func parseLatest(body []byte) (latest, error) {
	if len(body) > metadataLimit {
		return latest{}, errors.New("latest.json exceeds its size limit")
	}
	if err := checkJSON(body); err != nil {
		return latest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var value latest
	if err := decoder.Decode(&value); err != nil {
		return latest{}, errors.New("latest.json does not match its schema")
	}
	match := tagPattern.FindStringSubmatch(value.Tag)
	published, timeErr := time.Parse(time.RFC3339, value.PublishedAt)
	if value.SchemaVersion != 1 || match == nil || strconv.Itoa(value.Build) != match[1] || !commitPattern.MatchString(value.Commit) || match[2] != value.Commit[:7] ||
		timeErr != nil || published.UTC().Format(time.RFC3339) != value.PublishedAt || !sha256Pattern.MatchString(value.DMGSHA256) || !sha256Pattern.MatchString(value.ZIPSHA256) || value.Version == "" {
		return latest{}, errors.New("latest.json has invalid fields")
	}
	asset := "Agent-Queue-" + value.Version + "-universal"
	if value.ReleaseURL != releasesURL+"/tag/"+value.Tag || value.DMGURL != releasesURL+"/download/"+value.Tag+"/"+asset+".dmg" || value.ZIPURL != releasesURL+"/download/"+value.Tag+"/"+asset+".zip" {
		return latest{}, errors.New("latest.json links do not match its release")
	}
	return value, nil
}

// liveRedirects follows at most two redirects and only over HTTPS.
func liveRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("Too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("Refusing a redirect away from HTTPS")
	}
	return nil
}

// liveFetcher reads the deployed site, bypassing caches, with bounded sizes.
func liveFetcher(client *http.Client) func(string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: liveRedirects}
	}
	return func(url string) ([]byte, error) {
		request, err := http.NewRequest(http.MethodGet, url+"?cache="+strconv.FormatInt(time.Now().UnixNano(), 36), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Cache-Control", "no-cache")
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("The live site could not be read")
		}
		defer response.Body.Close()
		switch {
		case response.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		case response.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("The live site answered HTTP %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, appcastLimit+1))
		if err != nil || len(body) > appcastLimit {
			return nil, errors.New("The live site response is unreadable or too large")
		}
		return body, nil
	}
}

// live is the build the deployed site currently serves; nil means no site yet.
func (d *delivery) live() (*latest, error) {
	body, err := d.fetch(siteURL + "/" + latestName)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value, err := parseLatest(body)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// assembleSite copies the homepage files and adds the feed and latest.json.
// Only regular files inside siteDir are accepted: symlinks, special files,
// oversized files and name collisions with generated files are refused.
func assembleSite(siteDir, outDir string, generated map[string][]byte) error {
	info, err := os.Lstat(siteDir)
	if err != nil || !info.IsDir() {
		return errors.New("The site directory " + siteDir + " is missing or not a directory; the homepage must exist before delivery")
	}
	if err := os.Mkdir(outDir, 0o755); err != nil {
		return errors.New("The output directory must not exist yet")
	}
	total, count := 0, 0
	err = filepath.WalkDir(siteDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(siteDir, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if !filepath.IsLocal(relative) {
			return errors.New("Site path escapes the site directory: " + relative)
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") && name != ".nojekyll" {
			return errors.New("Hidden site files are not published: " + relative)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return errors.New("Symbolic links are not published: " + relative)
		}
		target := filepath.Join(outDir, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return errors.New("Only regular files are published: " + relative)
		}
		if _, reserved := generated[filepath.ToSlash(relative)]; reserved {
			return errors.New("The site must not contain the generated file " + relative)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		stat, err := file.Stat()
		if err != nil || !stat.Mode().IsRegular() || stat.Size() > fileLimit {
			return errors.New("Site file is not a regular file within the size bound: " + relative)
		}
		count++
		total += int(stat.Size())
		if count > countLimit || total > totalLimit {
			return errors.New("The site exceeds its file count or size bound")
		}
		body, err := io.ReadAll(io.LimitReader(file, fileLimit+1))
		if err != nil || len(body) > fileLimit {
			return errors.New("Site file exceeds its size bound: " + relative)
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		return err
	}
	for name, body := range generated {
		if err := os.WriteFile(filepath.Join(outDir, name), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

type request struct {
	want       tagCommit
	artifact   string // directory of this run's notarized artifact
	siteDir    string // homepage files from the automation commit
	outDir     string // assembled site, uploaded by the workflow
	resultFile string // receives deploy=true or deploy=false
}

func (d *delivery) result(req request, deploy bool) error {
	if req.resultFile == "" {
		return nil
	}
	file, err := os.OpenFile(req.resultFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "deploy=%t\n", deploy)
	return err
}

// vetLive compares the live site with the build being delivered. It reports
// "superseded" when the site already serves a newer build, and fails on a live
// build that is not a verified public release.
func (s *session) vetLive(live *latest, data manifest) (superseded bool, err error) {
	if live == nil {
		return false, nil
	}
	published, err := s.checked(live.Tag)
	if err != nil {
		return false, err
	}
	if *live != newLatest(published) {
		return false, errors.New("The live latest.json differs from its validated public release")
	}
	switch {
	case live.Build > data.Build:
		return true, nil
	case live.Build == data.Build && published.data != data:
		return false, errors.New("The same build number has conflicting release metadata")
	case live.Build < data.Build:
		return false, s.d.verifySuccessor(published.data, data)
	}
	return false, nil
}

// alreadyCurrent reports whether the live feed and latest.json already describe
// this build, ignoring every signature.
func (d *delivery) alreadyCurrent(live *latest, value checkedRelease) (bool, error) {
	if live == nil || *live != newLatest(value) {
		return false, nil
	}
	body, err := d.fetch(siteURL + "/" + appcastName)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Stat(value.zip)
	if err != nil {
		return false, err
	}
	return sameFeed(body, []byte(renderAppcast(value.data, info.Size(), ""))), nil
}

// prepare verifies the release, signs the feed and assembles the site to deploy.
func (d *delivery) prepare(req request) error {
	if err := req.want.validate(); err != nil {
		return err
	}
	if req.artifact == "" || req.siteDir == "" || req.outDir == "" {
		return errors.New("Delivery requires --expected-tag, --expected-commit, --artifact-dir, --site-dir and --output-dir")
	}
	work, err := os.MkdirTemp("", "agent-queue-pages-")
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
		fmt.Fprintf(d.output, "Superseded by build %d (%s); this run deploys nothing and the newer run publishes it.\n", sel.later.Build, sel.later.Tag)
		return d.result(req, false)
	}
	target := sel.target
	if err := bindArtifact(req.artifact, target); err != nil {
		return err
	}
	live, err := d.live()
	if err != nil {
		return err
	}
	superseded, err := s.vetLive(live, target.data)
	if err != nil {
		return err
	}
	if superseded {
		fmt.Fprintf(d.output, "Superseded: the live site already serves build %d, newer than build %d. Nothing deployed.\n", live.Build, target.data.Build)
		return d.result(req, false)
	}
	current, err := d.alreadyCurrent(live, target)
	if err != nil {
		return err
	}
	if current {
		fmt.Fprintf(d.output, "Already current: the live site serves build %d with identical feed content. Nothing deployed.\n", target.data.Build)
		return d.result(req, false)
	}
	appcast, err := d.signedAppcast(target.data, target.zip, work)
	if err != nil {
		return err
	}
	archive, err := os.ReadFile(target.zip)
	if err != nil {
		return err
	}
	if err := verifySignatures(d.public, archive, []byte(appcast)); err != nil {
		return err
	}
	body, err := newLatest(target).marshal()
	if err != nil {
		return err
	}
	if err := assembleSite(req.siteDir, req.outDir, map[string][]byte{appcastName: []byte(appcast), latestName: body}); err != nil {
		return err
	}
	fmt.Fprintf(d.output, "Assembled the site for %s with its signed feed and latest.json.\n", target.data.Tag)
	return d.result(req, true)
}

// verifyLive reads the deployed site back until it serves the assembled files.
// A newer build deployed by a later run is fine; anything else is retried for a
// bounded time to allow for Pages propagation, then fails.
func (d *delivery) verifyLive(outDir, artifact string) error {
	wantLatest, err := boundedFile(filepath.Join(outDir, latestName), metadataLimit)
	if err != nil {
		return err
	}
	expected, err := parseLatest(wantLatest)
	if err != nil {
		return err
	}
	wantFeed, err := boundedFile(filepath.Join(outDir, appcastName), appcastLimit)
	if err != nil {
		return err
	}
	archive, err := os.ReadFile(filepath.Join(artifact, "Agent-Queue-"+expected.Version+"-universal.zip"))
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != expected.ZIPSHA256 {
		return errors.New("The artifact archive does not match the deployed latest.json")
	}
	if err := verifySignatures(d.public, archive, wantFeed); err != nil {
		return err
	}
	var last error
	for attempt := 1; attempt <= d.attempts; attempt++ {
		if attempt > 1 {
			d.pause()
		}
		superseded, err := d.checkLive(expected, wantLatest, wantFeed, archive)
		if err == nil {
			if superseded {
				fmt.Fprintf(d.output, "The live site already serves a newer build than %s; nothing to verify.\n", expected.Tag)
			} else {
				fmt.Fprintf(d.output, "Live site verified: %s serves %s with a feed that verifies against the app key.\n", siteURL, expected.Tag)
			}
			return nil
		}
		last = err
	}
	return fmt.Errorf("The live site did not serve the deployed build after %d attempts: %w", d.attempts, last)
}

func (d *delivery) checkLive(expected latest, wantLatest, wantFeed, archive []byte) (bool, error) {
	body, err := d.fetch(siteURL + "/" + latestName)
	if err != nil {
		return false, err
	}
	live, err := parseLatest(body)
	if err != nil {
		return false, err
	}
	if live.Build > expected.Build {
		return true, nil
	}
	if live != expected {
		return false, errors.New("The live latest.json does not name the deployed build")
	}
	feed, err := d.fetch(siteURL + "/" + appcastName)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(feed, wantFeed) {
		return false, errors.New("The live appcast.xml differs from the deployed feed")
	}
	return false, verifySignatures(d.public, archive, feed)
}
