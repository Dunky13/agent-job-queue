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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testMoved = "cccccccccccccccccccccccccccccccccccccccc"
const testPublished = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testBot = "homebrew-sync[bot]"
const testEmail = "12345+homebrew-sync[bot]@users.noreply.github.com"

var testSignature = strings.Repeat("A", 86) + "=="

func fixtureManifest(build int) manifest {
	commit := strings.Repeat(strconv.Itoa(build), 40)
	body := []byte(fmt.Sprintf("archive-%d", build))
	sum := sha256.Sum256(body)
	return manifest{1, repository, "0.1.0", commit, build, fmt.Sprintf("build-%d-%.7s", build, commit), "Agent-Queue-0.1.0-universal.zip", hex.EncodeToString(sum[:])}
}
func jsonBytes(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
func fixtureRelease(data manifest) map[string]any {
	return map[string]any{"tag_name": data.Tag, "target_commitish": data.Commit, "draft": false, "assets": []any{map[string]any{"name": "agent-job-queue.json"}, map[string]any{"name": data.Asset}}}
}
func typedRelease(data manifest) release {
	result, err := decode[release](jsonBytes(fixtureRelease(data)))
	if err != nil {
		panic(err)
	}
	return result
}

func signedComment() string {
	return "<!-- sparkle-signatures:\nedSignature: " + testSignature + "\nlength: 99\n-->\n"
}

// deliveryFiles is the exact delivery the fixture expects for a build.
func deliveryFiles(t testing.TB, data manifest) files {
	t.Helper()
	cask, err := renderCask(data)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(data, "", "  ")
	archive := []byte(fmt.Sprintf("archive-%d", data.Build))
	feed := renderAppcast(data, int64(len(archive)), testArchiveSignature(archive))
	return files{caskPath: []byte(cask), manifestPath: append(body, '\n'), appcastPath: []byte(testSignFeed(feed))}
}

// A throwaway keypair stands in for the owner's key; no real key is committed.
var testPublic, testPrivate = func() (ed25519.PublicKey, ed25519.PrivateKey) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	return public, private
}()

func testArchiveSignature(archive []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(testPrivate, archive))
}

// testSignFeed signs the way Sparkle does: the comment length is the byte count it signs.
func testSignFeed(feed string) string {
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(testPrivate, []byte(feed)))
	return fmt.Sprintf("%s<!-- sparkle-signatures:\nedSignature: %s\nlength: %d\n-->\n", feed, signature, len(feed))
}

type fixture struct {
	t              *testing.T
	d              *delivery
	data           map[string]manifest
	source         map[string]map[string]any
	listed         []any
	head           string
	state          files            // main at head
	commits        map[string]files // content by commit
	inputs         []commitInput
	failMutations  int  // fail this many mutations
	moveOnFailure  bool // a concurrent commit lands when a mutation fails
	unrelated      bool
	badArchive     bool
	brewFailure    bool
	commitResponse func(map[string]any)
	artifactBuild  int // build in the notarized artifact; 0 follows the request
	tamperManifest bool
	tamperArchive  bool
	loseResponse   bool
	signCalls      [][]string
	signInputs     []string
	brewCalls      int
	output         bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	x := &fixture{t: t, data: map[string]manifest{}, source: map[string]map[string]any{}, head: testHead, state: files{}, commits: map[string]files{}}
	x.d = newDelivery(context.Background(), t.TempDir(), &x.output)
	x.d.signTool, x.d.signKey, x.d.bot, x.d.publicKey = "sign-update", "TEST-PRIVATE-KEY", bot{testBot, testEmail}, testPublic
	for _, build := range []int{1, 2, 3} {
		data := fixtureManifest(build)
		x.data[data.Tag] = data
		x.source[data.Tag] = fixtureRelease(data)
	}
	x.listed = []any{x.source[fixtureManifest(2).Tag], x.source[fixtureManifest(1).Tag]}
	x.d.api = func(endpoint string) ([]byte, error) {
		switch {
		case endpoint == "repos/"+repository+"/git/ref/heads/main":
			return jsonBytes(map[string]any{"object": map[string]any{"sha": x.head}}), nil
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
		case strings.HasPrefix(endpoint, "repos/"+repository+"/commits?sha="):
			return jsonBytes([]any{map[string]any{"sha": testPublished}}), nil
		case strings.HasPrefix(endpoint, "repos/"+repository+"/commits/"):
			sha := strings.TrimPrefix(endpoint, "repos/"+repository+"/commits/")
			response := map[string]any{"sha": sha, "parents": []any{map[string]any{"sha": testHead}}, "commit": map[string]any{"author": map[string]any{"name": testBot, "email": testEmail}, "verification": map[string]any{"verified": true}, "message": "chore\n\nSigned-off-by: " + testBot + " <" + testEmail + ">"}}
			if x.commitResponse != nil {
				x.commitResponse(response)
			}
			return jsonBytes(response), nil
		}
		return nil, fmt.Errorf("unexpected fixture API endpoint: %s", endpoint)
	}
	x.d.raw = func(endpoint string) ([]byte, error) {
		prefix := "repos/" + repository + "/contents/"
		path, ref, _ := strings.Cut(strings.TrimPrefix(endpoint, prefix), "?ref=")
		content := x.commits[ref]
		if ref == x.head {
			content = x.state
		}
		body, ok := content[path]
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
			body := []byte(fmt.Sprintf("archive-%d", data.Build))
			if name == manifestAsset {
				body = jsonBytes(data)
			} else if x.badArchive {
				body = append(body, "corrupt"...)
			}
			return nil, os.WriteFile(filepath.Join(destination, name), body, 0600)
		case len(arguments) >= 2 && slices.Equal(arguments[:2], []string{"brew", "ruby"}):
			x.brewCalls++
			body, err := os.ReadFile(arguments[len(arguments)-1])
			if err != nil || !bytes.HasPrefix(body, []byte(`cask "agent-job-queue"`)) || x.brewFailure {
				return nil, errors.New("fixture Homebrew DSL failure")
			}
			return nil, nil
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
	x.d.createCommit = func(input commitInput) (string, error) {
		x.inputs = append(x.inputs, input)
		if len(x.inputs) <= x.failMutations {
			if x.moveOnFailure {
				x.head = testMoved
			}
			return "", errors.New("fixture expected-head CAS rejected")
		}
		published := files{}
		for _, addition := range input.FileChanges.Additions {
			body, err := base64.StdEncoding.DecodeString(addition.Contents)
			if err != nil {
				return "", err
			}
			published[addition.Path] = body
		}
		x.commits[testPublished] = published
		if x.loseResponse {
			// The commit landed on main but the response never arrived.
			x.state, x.head = published, testMoved
			return "", errors.New("fixture lost the commit response")
		}
		return testPublished, nil
	}
	return x
}

// install puts a delivered build on main, as an earlier run would have.
func (x *fixture) install(data manifest) {
	x.state = deliveryFiles(x.t, data)
}

// artifact writes this run's notarized artifact for a build.
func (x *fixture) artifact(build int) string {
	directory := x.t.TempDir()
	data := fixtureManifest(build)
	archive := []byte(fmt.Sprintf("archive-%d", build))
	if x.tamperArchive {
		archive = append(archive, "x"...)
	}
	if x.tamperManifest {
		data.SHA256 = strings.Repeat("f", 64)
	}
	if err := os.WriteFile(filepath.Join(directory, manifestAsset), jsonBytes(data), 0600); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, data.Asset), archive, 0600); err != nil {
		x.t.Fatal(err)
	}
	return directory
}

// run drives a refresh. The artifact is the requested build, or the latest one.
func (x *fixture) run(publish bool, tag, commit string) error {
	build := x.artifactBuild
	if build == 0 {
		build = 2
		if data, ok := x.data[tag]; ok {
			build = data.Build
		}
	}
	artifact := ""
	if publish {
		artifact = x.artifact(build)
	}
	return x.d.refresh(request{tagCommit{tag, commit}, artifact, publish})
}

func TestRefreshSelectsLatestBuildAndPublishesOneSignedCommit(t *testing.T) {
	for _, scenario := range []string{"first-delivery", "dry-run", "exact-noop", "newer-than-main", "explicit-latest", "same-build-template-change"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			tag, commit, publish, commitsWanted := "", "", true, 1
			switch scenario {
			case "first-delivery":
				// Publication needs the expected identity, like the workflow supplies.
				tag, commit = fixtureManifest(2).Tag, fixtureManifest(2).Commit
			case "dry-run":
				publish, commitsWanted = false, 0
			case "exact-noop":
				x.install(fixtureManifest(2))
				tag, commit, commitsWanted = fixtureManifest(2).Tag, fixtureManifest(2).Commit, 0
			case "newer-than-main":
				x.install(fixtureManifest(1))
				tag, commit = fixtureManifest(2).Tag, fixtureManifest(2).Commit
			case "explicit-latest":
				tag, commit = fixtureManifest(2).Tag, fixtureManifest(2).Commit
			case "same-build-template-change":
				x.install(fixtureManifest(2))
				x.state[caskPath] = []byte(strings.Replace(string(x.state[caskPath]), "  auto_updates true\n", "", 1))
				tag, commit = fixtureManifest(2).Tag, fixtureManifest(2).Commit
			}
			if err := x.run(publish, tag, commit); err != nil {
				t.Fatal(err)
			}
			if len(x.inputs) != commitsWanted || x.brewCalls == 0 {
				t.Fatalf("mutations %d brew %d", len(x.inputs), x.brewCalls)
			}
			if !publish && len(x.signCalls) != 0 {
				t.Fatal("dry run signed")
			}
			if commitsWanted == 1 {
				if !sameFiles(x.commits[testPublished], deliveryFiles(t, fixtureManifest(2))) {
					t.Fatal("did not deliver the requested build as cask, manifest and signed feed")
				}
				input := x.inputs[0]
				if input.ExpectedHead != testHead || input.Branch.Name != "main" || input.Branch.Repository != repository || input.Message.Body != "Signed-off-by: "+testBot+" <"+testEmail+">" {
					t.Fatal(input)
				}
			}
		})
	}
}

// A run for an older build must not publish it: only the requested build has a
// notarized artifact to bind, and the newer run delivers the newer build.
func TestSupersededRunDeliversNothing(t *testing.T) {
	x := newFixture(t)
	if err := x.run(true, fixtureManifest(1).Tag, fixtureManifest(1).Commit); err != nil {
		t.Fatal(err)
	}
	if len(x.inputs) != 0 || len(x.signCalls) != 0 || !strings.Contains(x.output.String(), "Superseded by build 2") {
		t.Fatal("superseded run must report and deliver nothing", x.output.String())
	}
}

// A newer run lands on main while this run (the latest build when it started)
// is still signing: the release listing never showed build 3, but main holds it
// and it is a verified public release. The run must yield, not fail.
func TestRunYieldsWhenNewerRunLandedOnMainAfterSelection(t *testing.T) {
	x := newFixture(t)
	x.install(fixtureManifest(3))
	latest := fixtureManifest(2)
	if err := x.run(true, latest.Tag, latest.Commit); err != nil {
		t.Fatal(err)
	}
	if len(x.inputs) != 0 || !strings.Contains(x.output.String(), "Superseded: main already holds build 3") {
		t.Fatal("expected to yield to the newer main", x.output.String())
	}
	// A main build that is not a verified public release is still rejected.
	y := newFixture(t)
	y.install(fixtureManifest(3))
	delete(y.source, fixtureManifest(3).Tag)
	if err := y.run(true, latest.Tag, latest.Commit); err == nil || len(y.inputs) != 0 {
		t.Fatal("accepted a main build that is not a public release", err)
	}
}

// The commit landed but its response was lost: the retry finds identical
// content, verifies that commit and creates no second commit.
func TestLostCommitResponseIsRecoveredWithoutASecondCommit(t *testing.T) {
	latest := fixtureManifest(2)
	x := newFixture(t)
	x.loseResponse = true
	if err := x.run(true, latest.Tag, latest.Commit); err != nil {
		t.Fatal(err)
	}
	if len(x.inputs) != 1 || !strings.Contains(x.output.String(), "No commit created") {
		t.Fatal("expected one commit and a verified current report", len(x.inputs), x.output.String())
	}
	for _, scenario := range []string{"unverified", "wrong-author", "no-dco"} {
		y := newFixture(t)
		y.loseResponse = true
		y.commitResponse = func(response map[string]any) {
			commit := response["commit"].(map[string]any)
			switch scenario {
			case "unverified":
				commit["verification"] = map[string]any{"verified": false}
			case "wrong-author":
				commit["author"] = map[string]any{"name": "someone", "email": "someone@example.invalid"}
			case "no-dco":
				commit["message"] = "chore: no trailer"
			}
		}
		if err := y.run(true, latest.Tag, latest.Commit); err == nil || len(y.inputs) != 1 {
			t.Fatal("accepted an unverified recovered commit", scenario, err, len(y.inputs))
		}
	}
}

func TestSignaturesAreVerifiedAgainstThePublicKeyBeforeCommitting(t *testing.T) {
	latest := fixtureManifest(2)
	otherPublic, _, _ := ed25519.GenerateKey(nil)
	x := newFixture(t)
	x.d.publicKey = otherPublic // the secret does not match the committed public key
	err := x.run(true, latest.Tag, latest.Commit)
	if err == nil || !strings.Contains(err.Error(), "does not verify against the Sparkle public key") || len(x.inputs) != 0 {
		t.Fatal("a mismatched key must fail before committing", err, len(x.inputs))
	}
	y := newFixture(t)
	y.d.publicKey = nil
	if err := y.run(true, latest.Tag, latest.Commit); err == nil || len(y.inputs) != 0 {
		t.Fatal("delivery without a public key must fail", err)
	}
}

func TestVerifySignaturesRejectsTamperedContent(t *testing.T) {
	data := fixtureManifest(2)
	archive := []byte("archive-2")
	feed := renderAppcast(data, int64(len(archive)), testArchiveSignature(archive))
	good := testSignFeed(feed)
	if err := verifySignatures(testPublic, archive, []byte(good)); err != nil {
		t.Fatal(err)
	}
	// Sparkle writes the comment directly after the root element.
	if err := verifySignatures(testPublic, archive, []byte(testSignFeed(strings.TrimSuffix(feed, "\n")))); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() ([]byte, []byte){
		"archive bytes": func() ([]byte, []byte) { return []byte("archive-2x"), []byte(good) },
		"feed content": func() ([]byte, []byte) {
			return archive, []byte(strings.Replace(good, "Agent Queue updates", "Changed", 1))
		},
		"length field":     func() ([]byte, []byte) { return archive, []byte(strings.Replace(good, "length: ", "length: 1", 1)) },
		"missing comment":  func() ([]byte, []byte) { return archive, []byte(feed) },
		"wrong enclosure":  func() ([]byte, []byte) { return archive, []byte(testSignFeed(renderAppcast(data, 9, testSignature))) },
		"malformed feed":   func() ([]byte, []byte) { return archive, []byte("not xml") },
		"signature of nil": func() ([]byte, []byte) { return archive, nil },
	}
	for name, build := range cases {
		a, f := build()
		if err := verifySignatures(testPublic, a, f); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if err := verifySignatures(other, archive, []byte(good)); err == nil {
		t.Fatal("accepted a signature from another key")
	}
	if err := verifySignatures(nil, archive, []byte(good)); err == nil {
		t.Fatal("accepted a missing key")
	}
}

func TestLoadPublicKey(t *testing.T) {
	directory := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := base64.StdEncoding.EncodeToString(testPublic)
	key, err := loadPublicKey(write("good", good+"\n"))
	if err != nil || !key.Equal(testPublic) {
		t.Fatal(key, err)
	}
	for name, body := range map[string]string{"empty": "", "notbase64": "not base64!", "short": base64.StdEncoding.EncodeToString(testPublic[:16]), "two lines": good + "\n" + good + "\n", "private seed length plus": good + good} {
		if _, err := loadPublicKey(write(strings.ReplaceAll(name, " ", "-"), body)); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
			t.Fatalf("accepted %s: %v", name, err)
		}
	}
	if _, err := loadPublicKey(filepath.Join(directory, "missing")); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("accepted a missing key file", err)
	}
}

func TestRefreshExpectedIdentityBeforeIO(t *testing.T) {
	for _, pair := range [][2]string{{fixtureManifest(1).Tag, ""}, {"", fixtureManifest(1).Commit}, {"invalid", fixtureManifest(1).Commit}, {fixtureManifest(1).Tag, strings.Repeat("f", 40)}} {
		x := newFixture(t)
		x.d.api = func(string) ([]byte, error) { t.Fatal("invalid expected identity reached IO"); return nil, nil }
		if err := x.d.refresh(request{tagCommit{pair[0], pair[1]}, "", true}); err == nil {
			t.Fatal("accepted expected identity")
		}
	}
	x := newFixture(t)
	x.d.api = func(string) ([]byte, error) {
		t.Fatal("publication without identity or artifact reached IO")
		return nil, nil
	}
	for _, req := range []request{{publish: true}, {want: tagCommit{fixtureManifest(2).Tag, fixtureManifest(2).Commit}, publish: true}, {artifact: t.TempDir(), publish: true}} {
		if err := x.d.refresh(req); err == nil {
			t.Fatal("publication allowed without expected identity and artifact")
		}
	}
}

func TestRefreshRejectsConflictingStaleAndUnsafeState(t *testing.T) {
	for _, scenario := range []string{"missing-expected", "expected-full-commit", "expected-newer", "current-metadata-drift", "current-missing-release", "unpaired-manifest", "unpaired-cask", "archive-corrupt", "draft", "target", "duplicate-assets", "unrelated", "brew-failure", "invalid-current-manifest", "artifact-manifest-replaced", "artifact-archive-replaced", "artifact-other-build"} {
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
			case "current-metadata-drift":
				current := fixtureManifest(2)
				current.SHA256 = strings.Repeat("f", 64)
				x.install(current)
			case "current-missing-release":
				x.install(fixtureManifest(1))
				delete(x.source, fixtureManifest(1).Tag)
			case "unpaired-manifest":
				x.install(fixtureManifest(1))
				delete(x.state, caskPath)
			case "unpaired-cask":
				x.install(fixtureManifest(1))
				delete(x.state, manifestPath)
			case "invalid-current-manifest":
				x.install(fixtureManifest(1))
				x.state[manifestPath] = []byte("{}")
			case "archive-corrupt":
				x.badArchive = true
			case "draft":
				x.source[fixtureManifest(2).Tag]["draft"] = true
			case "target":
				x.source[fixtureManifest(2).Tag]["target_commitish"] = "main"
			case "duplicate-assets":
				assets := x.source[fixtureManifest(2).Tag]["assets"].([]any)
				x.source[fixtureManifest(2).Tag]["assets"] = append(assets, assets[0])
			case "unrelated":
				x.install(fixtureManifest(1))
				x.unrelated = true
			case "brew-failure":
				x.brewFailure = true
			case "artifact-manifest-replaced":
				x.tamperManifest = true
			case "artifact-archive-replaced":
				x.tamperArchive = true
			case "artifact-other-build":
				x.artifactBuild = 1
			}
			if err := x.run(true, tag, commit); err == nil {
				t.Fatal("unsafe delivery accepted")
			}
			if len(x.inputs) != 0 {
				t.Fatal("published an invalid delivery")
			}
			if strings.HasPrefix(scenario, "artifact") && len(x.signCalls) != 0 {
				t.Fatal("signed an artifact that does not match the public release")
			}
		})
	}
}

func TestAppcastMatchesGoldenFile(t *testing.T) {
	golden, err := os.ReadFile("testdata/appcast.golden.xml")
	if err != nil {
		t.Fatal(err)
	}
	data := fixtureManifest(7)
	got := renderAppcast(data, 12345678, testSignature)
	if got != string(golden) {
		t.Fatalf("appcast differs from the golden file:\n%s", got)
	}
}

func TestFeedValidationParsesXMLAndComparesSemantically(t *testing.T) {
	data := fixtureManifest(2)
	want := expectedItem(data, 9, testSignature)
	good := []byte(renderAppcast(data, 9, testSignature) + signedComment())
	if err := signedFeedMatches(good, want); err != nil {
		t.Fatal(err)
	}
	// Sparkle may reserialize: attribute order and whitespace must not matter.
	reserialized := strings.NewReplacer("\n    ", "\n", "></enclosure>", "/>").Replace(string(good))
	if err := signedFeedMatches([]byte(reserialized), want); err != nil {
		t.Fatal("rejected a reserialized feed", err)
	}
	rejected := map[string]string{
		"missing signature comment": renderAppcast(data, 9, testSignature),
		"wrong signature":           strings.Replace(string(good), "edSignature: "+testSignature, "edSignature: short", 1),
		"wrong length":              strings.Replace(string(good), `length="9"`, `length="10"`, 1),
		"wrong url":                 strings.Replace(string(good), "/download/"+data.Tag, "/download/other", 1),
		"wrong build":               strings.Replace(string(good), "<sparkle:version>2<", "<sparkle:version>3<", 1),
		"second item":               strings.Replace(string(good), "</channel>", "<item><link>x</link></item></channel>", 1),
		"not xml":                   "<rss><channel>",
		"wrong root":                strings.Replace(strings.Replace(string(good), "<rss", "<feed", 1), "</rss>", "</feed>", 1),
		"extra root":                string(good) + "<rss></rss>",
		"missing min system":        strings.Replace(string(good), "<sparkle:minimumSystemVersion>15.0</sparkle:minimumSystemVersion>", "", 1),
	}
	for name, body := range rejected {
		if err := signedFeedMatches([]byte(body), want); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	// Semantic comparison ignores signatures and whitespace but not content.
	other := []byte(renderAppcast(data, 9, strings.Repeat("B", 86)+"==") + "<!-- sparkle-signatures:\nedSignature: " + strings.Repeat("C", 86) + "==\n-->\n")
	if !sameFeed(good, other) || !sameFeed(good, []byte(reserialized)) {
		t.Fatal("signature or whitespace differences must not trigger a recommit")
	}
	if sameFeed(good, []byte(renderAppcast(fixtureManifest(3), 9, testSignature))) || sameFeed(good, []byte("not xml")) {
		t.Fatal("different feeds compared equal")
	}
}

func TestRenderedCaskUpdatesItselfAndRetainsQuarantine(t *testing.T) {
	cask, err := renderCask(fixtureManifest(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"\n  auto_updates true\n", "Agent-Queue-0.1.0-universal.zip\"", "\n  app \"Agent Queue.app\"\n", "Preserve quarantine so macOS can assess the notarized release."} {
		if !strings.Contains(cask, expected) {
			t.Fatalf("rendered cask is missing %q", expected)
		}
	}
	for _, unsafe := range []string{"preflight", "/usr/bin/xattr", "com.apple.quarantine", "postflight", "binary \""} {
		if strings.Contains(cask, unsafe) {
			t.Fatalf("rendered cask contains %s", unsafe)
		}
	}
}

// The key must reach the signer on stdin only, and neither it nor the GitHub
// token may reach the signer environment, using the real command runner.
func TestSignerReceivesKeyOnStdinNeverArgumentsOrEnvironment(t *testing.T) {
	directory := t.TempDir()
	tool := filepath.Join(directory, "sign_update")
	args, stdin, environment := filepath.Join(directory, "args"), filepath.Join(directory, "stdin"), filepath.Join(directory, "env")
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$@" >> %[1]q
cat >> %[2]q
env >> %[3]q
if [[ " $* " == *" -p "* ]]; then printf '%%s\n' %[4]q; else printf '<!-- sparkle-signatures:\nedSignature: %[4]s\n-->\n' >> "${@: -1}"; fi
`, args, stdin, environment, testSignature)
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "TOP-SECRET-GITHUB-TOKEN")
	t.Setenv("HOMEBREW_GITHUB_API_TOKEN", "TOP-SECRET-BREW-TOKEN")
	t.Setenv(keyVariable, "TOP-SECRET-KEY-MATERIAL")
	d := newDelivery(context.Background(), directory, io.Discard)
	d.signTool, d.signKey = tool, "TOP-SECRET-KEY-MATERIAL"
	archive := filepath.Join(directory, "archive.zip")
	if err := os.WriteFile(archive, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	appcast, err := d.signedAppcast(fixtureManifest(2), archive, directory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(appcast, "-->\n") || !strings.Contains(appcast, `length="7"`) || !strings.Contains(appcast, `sparkle:edSignature="`+testSignature+`"`) {
		t.Fatal(appcast)
	}
	argv, _ := os.ReadFile(args)
	input, _ := os.ReadFile(stdin)
	seen, _ := os.ReadFile(environment)
	if bytes.Contains(argv, []byte("TOP-SECRET")) || !strings.Contains(string(argv), "--ed-key-file\n-\n") || strings.Contains(string(argv), "\n-s\n") {
		t.Fatalf("key reached argv or the key file was not stdin: %s", argv)
	}
	if string(input) != "TOP-SECRET-KEY-MATERIAL\nTOP-SECRET-KEY-MATERIAL\n" {
		t.Fatalf("key not delivered on stdin for both signatures: %q", input)
	}
	if bytes.Contains(seen, []byte("TOP-SECRET")) {
		t.Fatal("a secret reached the signer environment")
	}
}

func TestChildEnvironmentIsLeastPrivilege(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv(keyVariable, "sparkle-key")
	t.Setenv("HOMEBREW_GITHUB_API_TOKEN", "brew-token")
	t.Setenv("HOMEBREW_NO_AUTO_UPDATE", "1")
	t.Setenv("UNRELATED_SECRET", "other")
	has := func(environment []string, entry string) bool { return slices.Contains(environment, entry) }
	gh := childEnvironment("gh")
	if !has(gh, "GH_TOKEN=gh-token") || has(gh, keyVariable+"=sparkle-key") {
		t.Fatal("gh must receive the token and never the Sparkle key")
	}
	for _, tool := range []string{"brew", "/path/to/sign_update"} {
		child := childEnvironment(tool)
		joined := strings.Join(child, "\n")
		if strings.Contains(joined, "gh-token") || strings.Contains(joined, "github-token") || strings.Contains(joined, "sparkle-key") || strings.Contains(joined, "brew-token") || strings.Contains(joined, "UNRELATED") {
			t.Fatalf("%s inherited a secret or unrelated variable: %v", tool, child)
		}
		if !has(child, "HOMEBREW_NO_AUTO_UPDATE=1") {
			t.Fatal("brew policy variables must survive")
		}
	}
}

func TestSigningRejectsMissingKeyAndUnsignedFeed(t *testing.T) {
	x := newFixture(t)
	x.d.signKey = ""
	if _, err := x.d.signedAppcast(fixtureManifest(2), "x", t.TempDir()); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("missing key accepted", err)
	}
	for _, signature := range []string{"", "short", testSignature + "extra"} {
		y := newFixture(t)
		archive := filepath.Join(t.TempDir(), "a.zip")
		_ = os.WriteFile(archive, []byte("a"), 0600)
		y.d.command = func([]string, []byte) ([]byte, error) { return []byte(signature), nil }
		if _, err := y.d.signedAppcast(fixtureManifest(2), archive, t.TempDir()); err == nil {
			t.Fatalf("accepted signature %q", signature)
		}
	}
	// A signer that leaves the feed untouched must not publish an unsigned feed.
	y := newFixture(t)
	archive := filepath.Join(t.TempDir(), "a.zip")
	_ = os.WriteFile(archive, []byte("a"), 0600)
	y.d.command = func(arguments []string, _ []byte) ([]byte, error) {
		if slices.Contains(arguments, "-p") {
			return []byte(testSignature), nil
		}
		return nil, nil
	}
	if _, err := y.d.signedAppcast(fixtureManifest(2), archive, t.TempDir()); err == nil {
		t.Fatal("accepted an unsigned feed")
	}
}

func TestPublicationRetriesWhenMainMovesAndStopsOtherwise(t *testing.T) {
	latest := fixtureManifest(2)
	t.Run("head moved", func(t *testing.T) {
		x := newFixture(t)
		x.failMutations, x.moveOnFailure = 1, true
		x.commitResponse = func(response map[string]any) { response["parents"] = []any{map[string]any{"sha": testMoved}} }
		if err := x.run(true, latest.Tag, latest.Commit); err != nil {
			t.Fatal(err)
		}
		if len(x.inputs) != 2 || x.inputs[0].ExpectedHead != testHead || x.inputs[1].ExpectedHead != testMoved {
			t.Fatalf("retry did not re-read main: %v", x.inputs)
		}
		if len(x.signCalls) != 2 {
			t.Fatal("retry signed again")
		}
	})
	t.Run("rejected with unchanged head", func(t *testing.T) {
		x := newFixture(t)
		x.failMutations = 1
		err := x.run(true, latest.Tag, latest.Commit)
		if err == nil || len(x.inputs) != 1 || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
			t.Fatal("permission failure must fail visibly with a docs pointer", err, len(x.inputs))
		}
	})
	t.Run("retries are bounded", func(t *testing.T) {
		x := newFixture(t)
		x.failMutations, x.moveOnFailure = 100, true
		calls := 0
		inner := x.d.api
		x.d.api = func(endpoint string) ([]byte, error) {
			if endpoint == "repos/"+repository+"/git/ref/heads/main" {
				calls++
				x.head = strings.Repeat(strconv.Itoa(calls%10), 40)
				return jsonBytes(map[string]any{"object": map[string]any{"sha": x.head}}), nil
			}
			return inner(endpoint)
		}
		if err := x.run(true, latest.Tag, latest.Commit); err == nil || len(x.inputs) != publishAttempts {
			t.Fatal("unbounded or silent retries", err, len(x.inputs))
		}
	})
}

func TestPublicationReadbackVerifiesCommit(t *testing.T) {
	latest := fixtureManifest(2)
	for _, scenario := range []string{"success", "signature", "author", "email", "dco", "identity", "parent", "content"} {
		t.Run(scenario, func(t *testing.T) {
			x := newFixture(t)
			x.commitResponse = func(response map[string]any) {
				commit := response["commit"].(map[string]any)
				switch scenario {
				case "signature":
					commit["verification"] = map[string]any{"verified": false}
				case "author":
					commit["author"] = map[string]any{"name": "other", "email": testEmail}
				case "email":
					commit["author"] = map[string]any{"name": testBot, "email": "other@example.invalid"}
				case "dco":
					commit["message"] = "Signed-off-by: Wrong <wrong@example.invalid>"
				case "identity":
					response["sha"] = testHead
				case "parent":
					response["parents"] = []any{map[string]any{"sha": testMoved}}
				}
			}
			err := x.run(true, latest.Tag, latest.Commit)
			if scenario == "content" {
				x.commits[testPublished][appcastPath] = []byte("tampered")
				err = x.d.verifyCommit(testPublished, testHead, deliveryFiles(t, latest))
			}
			if (err == nil) != (scenario == "success") {
				t.Fatal(scenario, err)
			}
			if len(x.inputs) != 1 {
				t.Fatal("a verification failure must not retry or recommit")
			}
		})
	}
}

func TestIdenticalMainContentCreatesNoCommitAfterVerifyingItsCommits(t *testing.T) {
	latest := fixtureManifest(2)
	x := newFixture(t)
	x.install(latest)
	if err := x.run(true, latest.Tag, latest.Commit); err != nil {
		t.Fatal(err)
	}
	if len(x.inputs) != 0 || !strings.Contains(x.output.String(), "No commit created") {
		t.Fatal("identical content must verify without committing")
	}
	for _, scenario := range []string{"unverified", "wrong-author", "no-dco"} {
		y := newFixture(t)
		y.install(latest)
		y.commitResponse = func(response map[string]any) {
			commit := response["commit"].(map[string]any)
			switch scenario {
			case "unverified":
				commit["verification"] = map[string]any{"verified": false}
			case "wrong-author":
				commit["author"] = map[string]any{"name": "someone", "email": "someone@example.invalid"}
			case "no-dco":
				commit["message"] = "chore: no trailer"
			}
		}
		if err := y.run(true, latest.Tag, latest.Commit); err == nil || len(y.inputs) != 0 {
			t.Fatal("unverified existing delivery accepted", scenario, err)
		}
	}
}

func TestRefreshReleasePageBoundAndSelection(t *testing.T) {
	drafts := func() []byte {
		page := make([]any, 100)
		for index := range page {
			page[index] = map[string]any{"draft": true}
		}
		return jsonBytes(page)
	}
	t.Run("page two", func(t *testing.T) {
		d := newDelivery(context.Background(), ".", io.Discard)
		calls := 0
		d.api = func(string) ([]byte, error) {
			calls++
			if calls == 1 {
				return drafts(), nil
			}
			return jsonBytes([]any{fixtureRelease(fixtureManifest(3))}), nil
		}
		releases, err := d.publishedReleases()
		if err != nil || calls != 2 {
			t.Fatal(err, calls)
		}
		if latest, err := selectRelease(releases); err != nil || latest.TagName != fixtureManifest(3).Tag {
			t.Fatal(latest, err)
		}
	})
	t.Run("incomplete pages", func(t *testing.T) {
		d := newDelivery(context.Background(), ".", io.Discard)
		d.api = func(string) ([]byte, error) { return drafts(), nil }
		if _, err := d.publishedReleases(); err == nil {
			t.Fatal("selected from truncated release history")
		}
	})
	t.Run("ambiguous latest", func(t *testing.T) {
		first := fixtureManifest(2)
		second := first
		second.Commit, second.Tag = strings.Repeat("f", 40), "build-2-fffffff"
		if _, err := selectRelease([]release{typedRelease(first), typedRelease(second)}); err == nil {
			t.Fatal("accepted ambiguous latest build")
		}
		if latest, err := selectRelease([]release{typedRelease(first), typedRelease(second), typedRelease(fixtureManifest(3))}); err != nil || latest.TagName != fixtureManifest(3).Tag {
			t.Fatal("rejected ambiguity below latest", err)
		}
	})
	t.Run("malformed page", func(t *testing.T) {
		for _, value := range []string{`{}`, `[true]`, `null`, `[{"draft":true,"draft":false}]`, `[] []`} {
			d := newDelivery(context.Background(), ".", io.Discard)
			d.api = func(string) ([]byte, error) { return []byte(value), nil }
			if _, err := d.publishedReleases(); err == nil {
				t.Fatalf("accepted malformed page %s", value)
			}
		}
	})
}

func TestStrictJSONDecoding(t *testing.T) {
	for _, body := range []string{`{"a":1,"a":2}`, `{"a":1} {}`, "{\"a\":\"\xff\"}", `{"a":`, strings.Repeat("[", 70) + strings.Repeat("]", 70)} {
		if _, err := decode[map[string]any]([]byte(body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	// GitHub adds fields, so unknown fields are tolerated in API responses.
	if value, err := decode[sha]([]byte(`{"sha":"x","new_field":true}`)); err != nil || value.SHA != "x" {
		t.Fatal(value, err)
	}
}

func TestStrictManifest(t *testing.T) {
	body := jsonBytes(fixtureManifest(1))
	for _, data := range [][]byte{[]byte(`[]`), []byte(`null`), []byte(`{"a":1,"a":2}`), append(body, []byte(" {}")...), []byte(strings.Replace(string(body), `"build":1`, `"build":true`, 1)), []byte(strings.Replace(string(body), repository, "other/project", 1)), []byte(strings.Replace(string(body), `"version":"0.1.0"`, `"version":"01.1.0"`, 1)), append(body, bytes.Repeat([]byte(" "), metadataLimit)...)} {
		if _, err := parseManifest(data); err == nil {
			t.Fatal("accepted invalid manifest")
		}
	}
	if _, err := parseManifest(body); err != nil {
		t.Fatal(err)
	}
}

func TestBotIdentityAndSignoff(t *testing.T) {
	if !(bot{testBot, testEmail}).valid() || (bot{"someone", testEmail}).valid() || (bot{testBot, "someone@example.invalid"}).valid() {
		t.Fatal("bot identity validation")
	}
	if got := (bot{testBot, testEmail}).signoff(); got != "Signed-off-by: "+testBot+" <"+testEmail+">" {
		t.Fatal(got)
	}
}

func TestPublicationEnvironmentGate(t *testing.T) {
	valid := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": repository, "GITHUB_REF": "refs/heads/main"}
	if !publicationAllowed(valid) {
		t.Fatal("valid main publication rejected")
	}
	for key := range valid {
		environment := map[string]string{}
		for field, value := range valid {
			environment[field] = value
		}
		environment[key] = "invalid"
		if publicationAllowed(environment) {
			t.Fatal("invalid publication environment accepted", key)
		}
	}
}

func TestRunRequiresPublicationConfigurationAndScrubsKey(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_REPOSITORY", repository)
	t.Setenv("GITHUB_REF", "refs/heads/main")
	t.Setenv(keyVariable, "")
	t.Setenv("TAP_BOT_NAME", testBot)
	t.Setenv("TAP_BOT_EMAIL", testEmail)
	if err := Run([]string{"--publish", "--sign-update", "sign_update"}, io.Discard); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("missing Sparkle key accepted", err)
	}
	t.Setenv(keyVariable, "key")
	t.Setenv("TAP_BOT_EMAIL", "someone@example.invalid")
	if err := Run([]string{"--publish", "--sign-update", "sign_update"}, io.Discard); err == nil || !strings.Contains(err.Error(), "docs/homebrew-delivery.md") {
		t.Fatal("non-bot identity accepted", err)
	}
	if os.Getenv(keyVariable) != "" {
		t.Fatal("the Sparkle key stayed in the process environment")
	}
	t.Setenv(keyVariable, "key")
	t.Setenv("TAP_BOT_EMAIL", testEmail)
	missing := filepath.Join(t.TempDir(), "public-ed-key.txt")
	if err := Run([]string{"--publish", "--sign-update", "sign_update", "--public-key", missing}, io.Discard); err == nil || !strings.Contains(err.Error(), "public key file") {
		t.Fatal("publication without the Sparkle public key accepted", err)
	}
	if err := os.WriteFile(missing, []byte(base64.StdEncoding.EncodeToString(testPublic)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyVariable, "key")
	if err := Run([]string{"--publish", "--sign-update", "sign_update", "--public-key", missing}, io.Discard); err == nil || !strings.Contains(err.Error(), "--artifact-dir") {
		t.Fatal("publication without an expected release and artifact accepted", err)
	}
}

// Opt-in: SPARKLE_SIGN_UPDATE names the real pinned sign_update. A throwaway key
// is generated here and never leaves memory, stdin or the test.
func TestRealSparkleSignerAcceptedAndDeterministic(t *testing.T) {
	tool := os.Getenv("SPARKLE_SIGN_UPDATE")
	if tool == "" {
		t.Skip("set SPARKLE_SIGN_UPDATE to the pinned sign_update to run")
	}
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	archive := filepath.Join(directory, "archive.zip")
	if err := os.WriteFile(archive, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	d := newDelivery(context.Background(), directory, io.Discard)
	d.signTool, d.signKey = tool, base64.StdEncoding.EncodeToString(private.Seed())
	first, err := d.signedAppcast(fixtureManifest(2), archive, directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.signedAppcast(fixtureManifest(2), archive, directory)
	if err != nil || first != second {
		t.Fatal("real signing must be deterministic so identical content never recommits", err)
	}
	if !sameFeed([]byte(first), []byte(renderAppcast(fixtureManifest(2), 7, "x"))) {
		t.Fatal("real signed feed must compare equal to the unsigned rendering")
	}
	// The real tool's byte range and comment format are what verifySignatures checks.
	if err := verifySignatures(private.Public().(ed25519.PublicKey), []byte("archive"), []byte(first)); err != nil {
		t.Fatal("real Sparkle signatures must verify with the matching public key", err)
	}
	if err := verifySignatures(testPublic, []byte("archive"), []byte(first)); err == nil {
		t.Fatal("real Sparkle signatures must not verify with another key")
	}
}
