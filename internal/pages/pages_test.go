package pages

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

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
	return map[string]any{"tag_name": data.Tag, "target_commitish": data.Commit, "draft": false, "published_at": testPublishedAt, "assets": []any{map[string]any{"name": manifestAsset}, map[string]any{"name": data.Asset}, map[string]any{"name": data.diskImage()}, map[string]any{"name": data.diskImage() + ".sha256"}}}
}
func typedRelease(data manifest) release {
	result, err := decode[release](jsonBytes(fixtureRelease(data)))
	if err != nil {
		panic(err)
	}
	return result
}

func fixturePublicMismatch() ed25519.PublicKey {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	return public
}

func signedComment() string {
	return "<!-- sparkle-signatures:\nedSignature: " + testSignature + "\nlength: 99\n-->\n"
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
		if _, err := loadPublicKey(write(strings.ReplaceAll(name, " ", "-"), body)); err == nil || !strings.Contains(err.Error(), "docs/pages-delivery.md") {
			t.Fatalf("accepted %s: %v", name, err)
		}
	}
	if _, err := loadPublicKey(filepath.Join(directory, "missing")); err == nil || !strings.Contains(err.Error(), "docs/pages-delivery.md") {
		t.Fatal("accepted a missing key file", err)
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
	t.Setenv(keyVariable, "TOP-SECRET-KEY-MATERIAL")
	d := newDelivery(context.Background(), io.Discard)
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
	t.Setenv("UNRELATED_SECRET", "other")
	has := func(environment []string, entry string) bool { return slices.Contains(environment, entry) }
	gh := childEnvironment("gh")
	if !has(gh, "GH_TOKEN=gh-token") || has(gh, keyVariable+"=sparkle-key") {
		t.Fatal("gh must receive the token and never the Sparkle key")
	}
	for _, tool := range []string{"/path/to/sign_update", "pbcopy"} {
		child := childEnvironment(tool)
		joined := strings.Join(child, "\n")
		if strings.Contains(joined, "gh-token") || strings.Contains(joined, "github-token") || strings.Contains(joined, "sparkle-key") || strings.Contains(joined, "UNRELATED") {
			t.Fatalf("%s inherited a secret or unrelated variable: %v", tool, child)
		}
	}
}

func TestSigningRejectsMissingKeyAndUnsignedFeed(t *testing.T) {
	x := newFixture(t)
	x.d.signKey = ""
	if _, err := x.d.signedAppcast(fixtureManifest(2), "x", t.TempDir()); err == nil || !strings.Contains(err.Error(), "docs/pages-delivery.md") {
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

func TestRefreshReleasePageBoundAndSelection(t *testing.T) {
	drafts := func() []byte {
		page := make([]any, 100)
		for index := range page {
			page[index] = map[string]any{"draft": true}
		}
		return jsonBytes(page)
	}
	t.Run("page two", func(t *testing.T) {
		d := newDelivery(context.Background(), io.Discard)
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
		d := newDelivery(context.Background(), io.Discard)
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
			d := newDelivery(context.Background(), io.Discard)
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
	if err := Run([]string{"prepare", "--sign-update", "sign_update"}, io.Discard); err == nil || !strings.Contains(err.Error(), "docs/pages-delivery.md") {
		t.Fatal("missing Sparkle key accepted", err)
	}
	t.Setenv(keyVariable, "key")
	missing := filepath.Join(t.TempDir(), "public-ed-key.txt")
	if err := Run([]string{"prepare", "--sign-update", "sign_update", "--public-key", missing}, io.Discard); err == nil || !strings.Contains(err.Error(), "public key file") {
		t.Fatal("publication without the Sparkle public key accepted", err)
	}
	if os.Getenv(keyVariable) != "" {
		t.Fatal("the Sparkle key stayed in the process environment")
	}
	if err := os.WriteFile(missing, []byte(base64.StdEncoding.EncodeToString(testPublic)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyVariable, "key")
	if err := Run([]string{"prepare", "--sign-update", "sign_update", "--public-key", missing}, io.Discard); err == nil || !strings.Contains(err.Error(), "expected release") {
		t.Fatal("publication without an expected release and artifact accepted", err)
	}
	t.Setenv("GITHUB_REF", "refs/heads/other")
	if err := Run([]string{"prepare", "--sign-update", "sign_update"}, io.Discard); err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatal("delivery outside main accepted", err)
	}
	for _, arguments := range [][]string{nil, {"publish"}, {"verify-live"}, {"prepare", "stray"}} {
		if err := Run(arguments, io.Discard); err == nil {
			t.Fatalf("accepted %v", arguments)
		}
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
	d := newDelivery(context.Background(), io.Discard)
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
