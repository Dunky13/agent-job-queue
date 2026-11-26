package pages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

const testPublishedAt = "2026-11-20T10:00:00Z"

func sum(body string) string {
	value := sha256.Sum256([]byte(body))
	return hex.EncodeToString(value[:])
}

// fixtureManifest hashes the same archive bytes the fixture serves.
func fixtureZIP(build int) string { return fmt.Sprintf("archive-%d", build) }
func fixtureDMG(build int) string { return fmt.Sprintf("dmg-%d", build) }

func fixtureChecked(build int) checkedRelease {
	return checkedRelease{data: fixtureManifest(build), dmgSHA256: sum(fixtureDMG(build)), publishedAt: testPublishedAt}
}

// fixtureLatest and fixtureFeed are what an earlier delivery of a build deployed.
func fixtureLatest(build int) latest { return newLatest(fixtureChecked(build)) }
func fixtureFeed(build int) string {
	archive := []byte(fixtureZIP(build))
	return testSignFeed(renderAppcast(fixtureManifest(build), int64(len(archive)), testArchiveSignature(archive)))
}
func fixtureSite(t testing.TB, build int) map[string][]byte {
	t.Helper()
	body, err := fixtureLatest(build).marshal()
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{latestName: body, appcastName: []byte(fixtureFeed(build))}
}

type fixture struct {
	t              *testing.T
	d              *delivery
	data           map[string]manifest
	source         map[string]map[string]any
	listed         []any
	site           map[string][]byte // the live site; nil serves 404 everywhere
	fetchErr       map[string]error  // by file name
	fetched        []string
	unrelated      bool
	badArchive     bool
	artifactBuild  int // build in the notarized artifact; 0 follows the request
	tamper         string
	signCalls      [][]string
	signInputs     []string
	output         bytes.Buffer
	siteDir        string
	outDir         string
	resultFile     string
	downloadsBuild map[int]int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	x := &fixture{t: t, data: map[string]manifest{}, source: map[string]map[string]any{}, fetchErr: map[string]error{}, downloadsBuild: map[int]int{}}
	x.siteDir = filepath.Join("testdata", "site")
	x.outDir = filepath.Join(t.TempDir(), "out")
	x.resultFile = filepath.Join(t.TempDir(), "result")
	x.d = newDelivery(context.Background(), &x.output)
	x.d.signTool, x.d.signKey, x.d.public = "sign-update", "TEST-PRIVATE-KEY", testPublic
	x.d.attempts, x.d.pause = 3, func() {}
	for _, build := range []int{1, 2, 3} {
		data := fixtureManifest(build)
		x.data[data.Tag] = data
		x.source[data.Tag] = fixtureRelease(data)
	}
	x.listed = []any{x.source[fixtureManifest(2).Tag], x.source[fixtureManifest(1).Tag]}
	x.d.api = func(endpoint string) ([]byte, error) {
		switch {
		case endpoint == "repos/"+repository+"/releases?per_page=100&page=1":
			return jsonBytes(x.listed), nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/compare/"):
			older, _, _ := strings.Cut(strings.TrimPrefix(endpoint, "repos/"+repository+"/compare/"), "...")
			if x.unrelated {
				older = strings.Repeat("f", 40)
			}
			return jsonBytes(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": older}}), nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/releases/tags/"):
			release, ok := x.source[strings.TrimPrefix(endpoint, "repos/"+repository+"/releases/tags/")]
			if !ok {
				return nil, errNotFound
			}
			return jsonBytes(release), nil
		}
		return nil, fmt.Errorf("unexpected fixture API endpoint: %s", endpoint)
	}
	x.d.fetch = func(url string) ([]byte, error) {
		name := strings.TrimPrefix(url, siteURL+"/")
		x.fetched = append(x.fetched, name)
		if err := x.fetchErr[name]; err != nil {
			return nil, err
		}
		body, ok := x.site[name]
		if !ok {
			return nil, errNotFound
		}
		return body, nil
	}
	x.d.command = func(arguments []string, input []byte) ([]byte, error) {
		switch {
		case len(arguments) >= 4 && slices.Equal(arguments[:3], []string{"gh", "release", "download"}):
			data, ok := x.data[arguments[3]]
			if !ok {
				return nil, errors.New("unknown fixture release download")
			}
			name := arguments[slices.Index(arguments, "--pattern")+1]
			destination := arguments[slices.Index(arguments, "--dir")+1]
			var body []byte
			switch name {
			case manifestAsset:
				body = jsonBytes(data)
			case data.Asset:
				body = []byte(fixtureZIP(data.Build))
				if x.badArchive {
					body = append(body, "corrupt"...)
				}
			case data.diskImage() + ".sha256":
				body = []byte(sum(fixtureDMG(data.Build)) + "  " + data.diskImage() + "\n")
			default:
				return nil, errors.New("unexpected fixture asset " + name)
			}
			x.downloadsBuild[data.Build]++
			return nil, os.WriteFile(filepath.Join(destination, name), body, 0600)
		case arguments[0] == "sign-update":
			x.signCalls = append(x.signCalls, arguments)
			x.signInputs = append(x.signInputs, string(input))
			path := arguments[len(arguments)-1]
			body, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if slices.Contains(arguments, "-p") {
				return []byte(testArchiveSignature(body) + "\n"), nil
			}
			return nil, os.WriteFile(path, []byte(testSignFeed(string(body))), 0600)
		}
		return nil, fmt.Errorf("unexpected fixture command %v", arguments)
	}
	return x
}

// artifact writes this run's notarized artifact for a build.
func (x *fixture) artifact(build int) string {
	directory := x.t.TempDir()
	data := fixtureManifest(build)
	zip, dmg, dmgSum := []byte(fixtureZIP(build)), []byte(fixtureDMG(build)), sum(fixtureDMG(build))
	switch x.tamper {
	case "manifest":
		data.SHA256 = strings.Repeat("f", 64)
	case "zip":
		zip = append(zip, "x"...)
	case "dmg":
		dmg = append(dmg, "x"...)
	case "dmg-checksum":
		dmgSum = strings.Repeat("f", 64)
	}
	files := map[string][]byte{manifestAsset: jsonBytes(data), data.Asset: zip, data.diskImage(): dmg, data.diskImage() + ".sha256": []byte(dmgSum + "  " + data.diskImage() + "\n")}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
			x.t.Fatal(err)
		}
	}
	return directory
}

// prepare drives the delivery. The artifact is the requested build, or build 2.
func (x *fixture) prepare(tag, commit string) error {
	build := x.artifactBuild
	if build == 0 {
		build = 2
		if data, ok := x.data[tag]; ok {
			build = data.Build
		}
	}
	return x.d.prepare(request{tagCommit{tag, commit}, x.artifact(build), x.siteDir, x.outDir, x.resultFile})
}
func (x *fixture) prepareLatest() error {
	latest := fixtureManifest(2)
	return x.prepare(latest.Tag, latest.Commit)
}

// deployed reports the deploy=... line the workflow reads.
func (x *fixture) deployed() string {
	body, _ := os.ReadFile(x.resultFile)
	return strings.TrimSpace(string(body))
}

func TestPrepareAssemblesTheSiteForTheLatestBuild(t *testing.T) {
	for _, scenario := range []string{"first-delivery", "older-live-build", "same-build-different-feed", "same-build-different-latest"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			switch scenario {
			case "older-live-build":
				x.site = fixtureSite(t, 1)
			case "same-build-different-feed":
				x.site = fixtureSite(t, 2)
				x.site[appcastName] = []byte(testSignFeed(renderAppcast(fixtureManifest(2), 99, testSignature)))
			case "same-build-different-latest":
				x.site = fixtureSite(t, 2)
				changed := fixtureLatest(2)
				changed.DMGSHA256 = strings.Repeat("e", 64)
				x.site[latestName], _ = changed.marshal()
			}
			if scenario == "same-build-different-latest" {
				// A live file that disagrees with its public release is a conflict, not a refresh.
				if err := x.prepareLatest(); err == nil {
					t.Fatal("accepted a live latest.json that differs from its release")
				}
				return
			}
			if err := x.prepareLatest(); err != nil {
				t.Fatal(err)
			}
			if x.deployed() != "deploy=true" || len(x.signCalls) != 2 {
				t.Fatal("expected one signing pair and a deployment", x.deployed(), len(x.signCalls))
			}
			for name, want := range map[string][]byte{latestName: fixtureSite(t, 2)[latestName], appcastName: []byte(fixtureFeed(2))} {
				got, err := os.ReadFile(filepath.Join(x.outDir, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s differs from the expected output: %v\n%s", name, err, got)
				}
			}
			for _, name := range []string{"index.html", filepath.Join("assets", "site.css")} {
				if _, err := os.Stat(filepath.Join(x.outDir, name)); err != nil {
					t.Fatal("homepage file was not copied", err)
				}
			}
			for _, input := range x.signInputs {
				if input != "TEST-PRIVATE-KEY\n" {
					t.Fatal("the key must reach the signer on stdin only", input)
				}
			}
		})
	}
}

// A run for an older build must not deploy it: only the requested build has a
// notarized artifact to bind, and the newer run deploys the newer build.
func TestSupersededByANewerPublicReleaseDeploysNothing(t *testing.T) {
	x := newFixture(t)
	older := fixtureManifest(1)
	if err := x.prepare(older.Tag, older.Commit); err != nil {
		t.Fatal(err)
	}
	if x.deployed() != "deploy=false" || len(x.signCalls) != 0 || len(x.fetched) != 0 || !strings.Contains(x.output.String(), "Superseded by build 2") {
		t.Fatal("superseded run must report and deploy nothing", x.output.String())
	}
	if _, err := os.Stat(x.outDir); err == nil {
		t.Fatal("a superseded run assembled a site")
	}
}

func TestLiveSiteDecidesSupersessionAndCurrency(t *testing.T) {
	t.Run("live site serves a newer build", func(t *testing.T) {
		x := newFixture(t)
		x.site = fixtureSite(t, 3) // a newer run landed after this run was selected
		if err := x.prepareLatest(); err != nil {
			t.Fatal(err)
		}
		if x.deployed() != "deploy=false" || len(x.signCalls) != 0 || !strings.Contains(x.output.String(), "Superseded: the live site already serves build 3") {
			t.Fatal(x.output.String())
		}
	})
	t.Run("live newer build is not a public release", func(t *testing.T) {
		x := newFixture(t)
		x.site = fixtureSite(t, 3)
		delete(x.source, fixtureManifest(3).Tag)
		if err := x.prepareLatest(); err == nil || len(x.signCalls) != 0 {
			t.Fatal("accepted a live build that is not public", err)
		}
	})
	t.Run("same build with identical feed content", func(t *testing.T) {
		x := newFixture(t)
		x.site = fixtureSite(t, 2)
		// Different signatures are not different content.
		archive := []byte(fixtureZIP(2))
		other := renderAppcast(fixtureManifest(2), int64(len(archive)), strings.Repeat("B", 86)+"==")
		x.site[appcastName] = []byte(other + "<!-- sparkle-signatures:\nedSignature: " + strings.Repeat("C", 86) + "==\nlength: 1\n-->\n")
		if err := x.prepareLatest(); err != nil {
			t.Fatal(err)
		}
		if x.deployed() != "deploy=false" || len(x.signCalls) != 0 || !strings.Contains(x.output.String(), "Already current") {
			t.Fatal(x.output.String())
		}
	})
	t.Run("same build with a missing live feed", func(t *testing.T) {
		x := newFixture(t)
		x.site = fixtureSite(t, 2)
		delete(x.site, appcastName)
		if err := x.prepareLatest(); err != nil || x.deployed() != "deploy=true" {
			t.Fatal("a missing live feed must be redeployed", err, x.deployed())
		}
	})
	t.Run("live older build must be an ancestor", func(t *testing.T) {
		x := newFixture(t)
		x.site = fixtureSite(t, 1)
		x.unrelated = true
		if err := x.prepareLatest(); err == nil || len(x.signCalls) != 0 {
			t.Fatal("accepted a delivery that does not descend from the live build", err)
		}
	})
	t.Run("live release is verified once", func(t *testing.T) {
		x := newFixture(t)
		x.site = fixtureSite(t, 1)
		if err := x.prepareLatest(); err != nil || x.downloadsBuild[1] != 3 {
			t.Fatal("expected one verified download set for the live build", err, x.downloadsBuild)
		}
	})
}

func TestLiveFetchFailuresAreNotTreatedAsEmpty(t *testing.T) {
	for name, scenario := range map[string]func(*fixture){
		"server error":  func(x *fixture) { x.fetchErr[latestName] = errors.New("The live site answered HTTP 500") },
		"network error": func(x *fixture) { x.fetchErr[latestName] = errors.New("The live site could not be read") },
		"malformed":     func(x *fixture) { x.site = map[string][]byte{latestName: []byte("{")} },
		"unknown field": func(x *fixture) {
			x.site = map[string][]byte{latestName: []byte(strings.Replace(string(fixtureSite(x.t, 2)[latestName]), `"build": 2,`, `"build": 2, "extra": 1,`, 1))}
		},
		"mismatched links": func(x *fixture) {
			x.site = map[string][]byte{latestName: []byte(strings.Replace(string(fixtureSite(x.t, 2)[latestName]), "/releases/tag/", "/releases/other/", 1))}
		},
		"feed error": func(x *fixture) {
			x.site = fixtureSite(x.t, 2)
			x.fetchErr[appcastName] = errors.New("The live site answered HTTP 502")
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newFixture(t)
			scenario(x)
			if err := x.prepareLatest(); err == nil || x.deployed() == "deploy=true" || len(x.signCalls) != 0 {
				t.Fatal("a failed live read must fail the job", err)
			}
		})
	}
	x := newFixture(t) // only a missing site counts as empty
	if err := x.prepareLatest(); err != nil || x.deployed() != "deploy=true" {
		t.Fatal("a 404 means no site yet", err)
	}
}

func TestPrepareRejectsConflictingStaleAndUnsafeState(t *testing.T) {
	for _, scenario := range []string{"missing-expected", "expected-full-commit", "expected-newer", "archive-corrupt", "draft", "target", "duplicate-assets", "missing-dmg-asset", "no-published-time", "live-conflicting-same-build", "artifact-manifest-replaced", "artifact-zip-replaced", "artifact-dmg-replaced", "artifact-dmg-checksum-replaced", "artifact-other-build", "no-site-directory", "unsigned-key-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			tag, commit := fixtureManifest(2).Tag, fixtureManifest(2).Commit
			switch scenario {
			case "missing-expected":
				tag, commit = fixtureManifest(3).Tag, fixtureManifest(3).Commit
				delete(x.source, tag)
			case "expected-full-commit":
				tag, commit = fixtureManifest(1).Tag, fixtureManifest(1).Commit[:7]+strings.Repeat("f", 33)
			case "expected-newer":
				tag, commit = fixtureManifest(3).Tag, fixtureManifest(3).Commit
			case "archive-corrupt":
				x.badArchive = true
			case "draft":
				x.source[fixtureManifest(2).Tag]["draft"] = true
			case "target":
				x.source[fixtureManifest(2).Tag]["target_commitish"] = "main"
			case "duplicate-assets":
				assets := x.source[fixtureManifest(2).Tag]["assets"].([]any)
				x.source[fixtureManifest(2).Tag]["assets"] = append(assets, assets[0])
			case "missing-dmg-asset":
				x.source[fixtureManifest(2).Tag]["assets"] = x.source[fixtureManifest(2).Tag]["assets"].([]any)[:2]
			case "no-published-time":
				delete(x.source[fixtureManifest(2).Tag], "published_at")
			case "live-conflicting-same-build":
				other := fixtureManifest(2)
				other.Commit = strings.Repeat("9", 40)
				x.site = fixtureSite(t, 2)
				live := fixtureLatest(2)
				live.Commit = other.Commit
				x.site[latestName], _ = live.marshal()
			case "artifact-manifest-replaced":
				x.tamper = "manifest"
			case "artifact-zip-replaced":
				x.tamper = "zip"
			case "artifact-dmg-replaced":
				x.tamper = "dmg"
			case "artifact-dmg-checksum-replaced":
				x.tamper = "dmg-checksum"
			case "artifact-other-build":
				x.artifactBuild = 1
			case "no-site-directory":
				x.siteDir = filepath.Join(t.TempDir(), "site")
			case "unsigned-key-mismatch":
				x.d.public = fixturePublicMismatch()
			}
			err := x.prepare(tag, commit)
			if err == nil || x.deployed() == "deploy=true" {
				t.Fatal("unsafe delivery accepted", err)
			}
			if strings.HasPrefix(scenario, "artifact") && len(x.signCalls) != 0 {
				t.Fatal("signed an artifact that does not match the public release")
			}
			if scenario == "no-site-directory" && !strings.Contains(err.Error(), "site directory") {
				t.Fatal("the missing site must be reported clearly", err)
			}
		})
	}
}

func TestPrepareRequiresIdentityArtifactAndPaths(t *testing.T) {
	x := newFixture(t)
	x.d.api = func(string) ([]byte, error) { t.Fatal("invalid request reached IO"); return nil, nil }
	good := tagCommit{fixtureManifest(2).Tag, fixtureManifest(2).Commit}
	for _, req := range []request{
		{},
		{want: tagCommit{good.Tag, ""}},
		{want: tagCommit{"", good.Commit}},
		{want: tagCommit{"invalid", good.Commit}},
		{want: tagCommit{good.Tag, strings.Repeat("f", 40)}},
		{want: good, siteDir: "s", outDir: "o"},
		{want: good, artifact: "a", outDir: "o"},
		{want: good, artifact: "a", siteDir: "s"},
	} {
		if err := x.d.prepare(req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
}

func TestLatestMatchesGoldenFileAndRejectsInvalidShapes(t *testing.T) {
	golden, err := os.ReadFile("testdata/latest.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := fixtureLatest(7).marshal()
	if err != nil || string(got) != string(golden) {
		t.Fatalf("latest.json differs from the golden file: %v\n%s", err, got)
	}
	if value, err := parseLatest(golden); err != nil || value != fixtureLatest(7) {
		t.Fatal(value, err)
	}
	good := string(golden)
	for name, body := range map[string]string{
		"unknown field":   strings.Replace(good, `"build": 7,`, `"build": 7, "extra": true,`, 1),
		"duplicate field": strings.Replace(good, `"build": 7,`, `"build": 7, "build": 7,`, 1),
		"missing field":   strings.Replace(good, `"dmgSha256"`, `"other"`, 1),
		"wrong schema":    strings.Replace(good, `"schemaVersion": 1`, `"schemaVersion": 2`, 1),
		"tag mismatch":    strings.Replace(good, `"build": 7`, `"build": 8`, 1),
		"bad time":        strings.Replace(good, testPublishedAt, "yesterday", 1),
		"non utc time":    strings.Replace(good, testPublishedAt, "2026-11-20T11:00:00+01:00", 1),
		"short checksum":  strings.Replace(good, `"zipSha256": "`, `"zipSha256": "00`, 1),
		"foreign url":     strings.Replace(good, "https://github.com/", "https://example.invalid/", 1),
		"trailing data":   good + "{}",
		"not an object":   "[]",
		"empty":           "",
	} {
		if _, err := parseLatest([]byte(body)); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestAssembleSiteAcceptsOnlyRegularFilesInsideTheSiteDirectory(t *testing.T) {
	generated := map[string][]byte{appcastName: []byte("feed"), latestName: []byte("{}")}
	build := func(t *testing.T, files map[string]string) string {
		root := filepath.Join(t.TempDir(), "site")
		for name, body := range files {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		return root
	}
	t.Run("homepage only", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		if err := assembleSite(build(t, map[string]string{"index.html": "<html>", "assets/a.css": "body{}", ".nojekyll": ""}), out, generated); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"index.html", "assets/a.css", ".nojekyll", appcastName, latestName} {
			if _, err := os.Stat(filepath.Join(out, name)); err != nil {
				t.Fatal(name, err)
			}
		}
	})
	t.Run("missing site directory", func(t *testing.T) {
		err := assembleSite(filepath.Join(t.TempDir(), "site"), filepath.Join(t.TempDir(), "out"), generated)
		if err == nil || !strings.Contains(err.Error(), "site directory") {
			t.Fatal(err)
		}
	})
	t.Run("site is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "site")
		_ = os.WriteFile(file, nil, 0o600)
		if err := assembleSite(file, filepath.Join(t.TempDir(), "out"), generated); err == nil {
			t.Fatal("accepted a file as the site directory")
		}
	})
	t.Run("site is a symlink", func(t *testing.T) {
		real := build(t, map[string]string{"index.html": "x"})
		link := filepath.Join(t.TempDir(), "site")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if err := assembleSite(link, filepath.Join(t.TempDir(), "out"), generated); err == nil {
			t.Fatal("followed a symlinked site directory")
		}
	})
	for name, prepare := range map[string]func(root string) error{
		"symlink to a file outside": func(root string) error {
			outside := filepath.Join(filepath.Dir(root), "secret.txt")
			_ = os.WriteFile(outside, []byte("secret"), 0o600)
			return os.Symlink(outside, filepath.Join(root, "link.txt"))
		},
		"symlink to a directory outside": func(root string) error { return os.Symlink(filepath.Dir(root), filepath.Join(root, "up")) },
		"dangling symlink":               func(root string) error { return os.Symlink("missing", filepath.Join(root, "dangling")) },
		"oversized file": func(root string) error {
			return os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, fileLimit+1), 0o644)
		},
		"hidden file":          func(root string) error { return os.WriteFile(filepath.Join(root, ".env"), []byte("x"), 0o644) },
		"hidden directory":     func(root string) error { return os.MkdirAll(filepath.Join(root, ".git"), 0o755) },
		"generated collision":  func(root string) error { return os.WriteFile(filepath.Join(root, appcastName), []byte("x"), 0o644) },
		"latest collision":     func(root string) error { return os.WriteFile(filepath.Join(root, latestName), []byte("x"), 0o644) },
		"nested collision":     func(root string) error { return nil }, // nested names are allowed: only the root names are generated
		"named pipe":           func(root string) error { return mkfifo(filepath.Join(root, "pipe")) },
		"too many files":       func(root string) error { return manyFiles(root, countLimit+1) },
		"exceeds total budget": func(root string) error { return bigFiles(root) },
	} {
		t.Run(name, func(t *testing.T) {
			root := build(t, map[string]string{"index.html": "x"})
			if err := prepare(root); err != nil {
				t.Skip("cannot build this fixture here:", err)
			}
			out := filepath.Join(t.TempDir(), "out")
			err := assembleSite(root, out, generated)
			if name == "nested collision" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted an unsafe site")
			}
		})
	}
	t.Run("output exists", func(t *testing.T) {
		out := t.TempDir()
		if err := assembleSite(build(t, map[string]string{"index.html": "x"}), out, generated); err == nil {
			t.Fatal("overwrote an existing output directory")
		}
	})
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

func manyFiles(root string, count int) error {
	for index := 0; index < count; index++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d", index)), nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}
func bigFiles(root string) error {
	for index := 0; index <= totalLimit/fileLimit; index++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("big%d", index)), make([]byte, fileLimit), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestVerifyLiveReadsTheDeployedSiteBack(t *testing.T) {
	deploy := func(t *testing.T, x *fixture) string {
		if err := x.prepareLatest(); err != nil {
			t.Fatal(err)
		}
		return x.outDir
	}
	artifact := func(x *fixture) string { return x.artifact(2) }
	t.Run("serves the deployed build", func(t *testing.T) {
		x := newFixture(t)
		out := deploy(t, x)
		x.site = fixtureSite(t, 2)
		if err := x.d.verifyLive(out, artifact(x)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("waits for propagation", func(t *testing.T) {
		x := newFixture(t)
		out := deploy(t, x)
		pauses := 0
		x.d.pause = func() {
			pauses++
			if pauses == 2 {
				x.site = fixtureSite(t, 2)
			}
		}
		x.site = fixtureSite(t, 1) // the old site is still served
		x.d.attempts = 5
		if err := x.d.verifyLive(out, artifact(x)); err != nil || pauses != 2 {
			t.Fatal(err, pauses)
		}
	})
	t.Run("a newer build is acceptable", func(t *testing.T) {
		x := newFixture(t)
		out := deploy(t, x)
		x.site = fixtureSite(t, 3)
		if err := x.d.verifyLive(out, artifact(x)); err != nil {
			t.Fatal(err)
		}
	})
	for name, serve := range map[string]func(x *fixture){
		"never deployed":    func(x *fixture) { x.site = nil },
		"old build forever": func(x *fixture) { x.site = fixtureSite(x.t, 1) },
		"feed differs":      func(x *fixture) { x.site = fixtureSite(x.t, 2); x.site[appcastName] = append(x.site[appcastName], ' ') },
		"feed of another key": func(x *fixture) {
			x.site = fixtureSite(x.t, 2)
			x.site[appcastName] = []byte(strings.Replace(string(x.site[appcastName]), "edSignature: ", "edSignature: B", 1))
		},
		"missing feed":   func(x *fixture) { x.site = fixtureSite(x.t, 2); delete(x.site, appcastName) },
		"latest differs": func(x *fixture) { x.site = fixtureSite(x.t, 2); x.site[latestName] = []byte("{}") },
		"fetch error": func(x *fixture) {
			x.site = fixtureSite(x.t, 2)
			x.fetchErr[latestName] = errors.New("The live site answered HTTP 503")
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newFixture(t)
			out := deploy(t, x)
			serve(x)
			err := x.d.verifyLive(out, artifact(x))
			if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
				t.Fatal("a mismatching live site must fail after bounded retries", err)
			}
		})
	}
	t.Run("assembled files are checked before reading the site", func(t *testing.T) {
		x := newFixture(t)
		out := deploy(t, x)
		original, err := os.ReadFile(filepath.Join(out, appcastName))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, appcastName), []byte(strings.Replace(string(original), "Agent Queue</title>", "Tampered</title>", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		x.fetched = nil
		if err := x.d.verifyLive(out, artifact(x)); err == nil || len(x.fetched) != 0 {
			t.Fatal("verified a feed that does not verify against the public key", err)
		}
		if err := os.WriteFile(filepath.Join(out, appcastName), original, 0o600); err != nil {
			t.Fatal(err)
		}
		x.tamper = "zip"
		if err := x.d.verifyLive(out, artifact(x)); err == nil || len(x.fetched) != 0 {
			t.Fatal("accepted an archive that differs from latest.json", err)
		}
	})
}

func TestLiveFetcherReadsBoundedNonCachedResponses(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery+"|"+r.Header.Get("Cache-Control"))
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("body"))
		case "/missing":
			http.NotFound(w, r)
		case "/huge":
			_, _ = w.Write(make([]byte, appcastLimit+1))
		default:
			http.Error(w, "broken", http.StatusBadGateway)
		}
	}))
	defer server.Close()
	fetch := liveFetcher(server.Client())
	if body, err := fetch(server.URL + "/ok"); err != nil || string(body) != "body" {
		t.Fatal(string(body), err)
	}
	if _, err := fetch(server.URL + "/missing"); !errors.Is(err, errNotFound) {
		t.Fatal("a 404 must be reported as not found", err)
	}
	for _, path := range []string{"/error", "/huge"} {
		if _, err := fetch(server.URL + path); err == nil || errors.Is(err, errNotFound) {
			t.Fatalf("%s must be a hard failure: %v", path, err)
		}
	}
	if len(queries) == 0 || !strings.HasPrefix(queries[0], "cache=") || !strings.HasSuffix(queries[0], "|no-cache") {
		t.Fatal("reads must bypass caches", queries)
	}
	if _, err := liveFetcher(nil)("http://127.0.0.1:1/unreachable"); err == nil || errors.Is(err, errNotFound) {
		t.Fatal("an unreachable site is a failure, not an empty site", err)
	}
}

func TestLiveRedirectsStayOnHTTPS(t *testing.T) {
	to := func(raw string) *http.Request {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	origin := []*http.Request{to("https://dunky13.github.io/agent-job-queue/appcast.xml")}
	if err := liveRedirects(to("https://dunky13.github.io/agent-job-queue/"), origin); err != nil {
		t.Fatalf("HTTPS redirect refused: %v", err)
	}
	if err := liveRedirects(to("http://dunky13.github.io/agent-job-queue/"), origin); err == nil {
		t.Fatal("redirect to plain HTTP accepted")
	}
	if err := liveRedirects(to("https://dunky13.github.io/"), append(origin, origin[0], origin[0])); err == nil {
		t.Fatal("third redirect accepted")
	}
}

func TestRepositorySiteAssembles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	generated := map[string][]byte{"appcast.xml": []byte("<rss/>"), "latest.json": []byte("{}")}
	if err := assembleSite(filepath.Join("..", "..", "site"), out, generated); err != nil {
		t.Fatalf("the homepage in site/ would fail delivery: %v", err)
	}
	for _, name := range []string{"index.html", "404.html", "appcast.xml", "latest.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatal(err)
		}
	}
}
