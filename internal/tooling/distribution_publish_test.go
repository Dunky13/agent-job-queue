package tooling

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fake is an executable on PATH, so the tests exercise the actual shell
// script and its exit behavior. It cannot contact GitHub or use credentials.
const publishFakeGH = `
[ "$1" = api ] && [ "$2" = --hostname ] && [ "$3" = github.com ] || exit 99
shift 3
endpoint=$1; shift
method=GET input= fields= headers= pagination=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --method) method=$2; shift 2;;
    --input) input=$2; shift 2;;
    -F|-f) fields="$fields $2"; shift 2;;
    -H) headers="$headers $2"; shift 2;;
    --paginate|--slurp) pagination="$pagination $1"; shift;;
    *) exit 99;;
  esac
done
cd "$PUBLISH_TEST_ROOT"
printf '%s %s\n' "$method" "$endpoint" >> calls
case "$method $endpoint" in
  'GET repos/owner/project/immutable-releases')
    [ "${PUBLISH_FAIL:-}" != 'policy unreadable' ] || exit 91
    if [ "${PUBLISH_FAIL:-}" = 'policy disabled' ]; then printf '{"enabled":false}'; else printf '{"enabled":true}'; fi;;
  'GET repos/owner/project/releases?per_page=100')
    [ "$pagination" = ' --paginate --slurp' ] || exit 99
    [ "${PUBLISH_FAIL:-}" != list ] || exit 91
    if [ "${PUBLISH_FAIL:-}" = 'malformed list' ]; then printf '{}'; exit 0; fi
    if [ -f release.json ]; then jq -s '[.]' release.json; else printf '[[]]\n'; fi;;
  'GET repos/owner/project/git/matching-refs/tags/build-45-1111111')
    [ "${PUBLISH_FAIL:-}" != tag ] || exit 91
    if [ "${PUBLISH_FAIL:-}" = 'tag ambiguous' ]; then cat refs.json; exit 91; fi
    if [ "${PUBLISH_FAIL:-}" = 'tag disappears' ] && [ -f uploaded ]; then printf '[]'; exit 0; fi
    if [ "${PUBLISH_FAIL:-}" = 'tag moves' ] && [ -f uploaded ]; then
      jq 'map(.object.sha="2222222222222222222222222222222222222222")' refs.json
    else cat refs.json; fi;;
  'GET repos/owner/project/git/tags/'*) cat annotated.json;;
  'POST repos/owner/project/releases')
    [ ! -f release.json ] || exit 99
    jq -e '.draft == true and .prerelease == true' "$input" >/dev/null || exit 99
    jq '. + {id:70}' "$input" > release.json
    cat release.json
    [ "${PUBLISH_FAIL:-}" != 'create after' ] || exit 91;;
  'GET repos/owner/project/releases/70')
    [ "${PUBLISH_FAIL:-}" != read ] || exit 91
    if [ "${PUBLISH_FAIL:-}" = 'target changes' ]; then jq '.target_commitish="2222222222222222222222222222222222222222"' release.json; exit 0; fi
    cat release.json;;
  'GET repos/owner/project/releases/70/assets?per_page=100')
    [ "$pagination" = ' --paginate --slurp' ] || exit 99
    [ "${PUBLISH_FAIL:-}" != inventory ] || exit 91
    count=0
    if [ -f inventories ]; then count=$(cat inventories); fi
    count=$((count + 1)); printf '%s' "$count" > inventories
    if [ "${PUBLISH_FAIL:-}" = 'inventory changes' ] && [ "$count" = 3 ]; then
      jq '.[0].id=9999 | [.]' assets.json; exit 0
    fi
    jq '[.]' assets.json;;
  'POST https://uploads.github.com/repos/owner/project/releases/70/assets?name='*)
    jq -e '.draft == true' release.json >/dev/null || exit 99
    name=${endpoint##*name=}
    [ "$headers" = ' Content-Type: application/octet-stream' ] || exit 99
    jq -e --arg name "$name" 'all(.[]; .name != $name)' assets.json >/dev/null || exit 99
    [ "${PUBLISH_FAIL:-}" != 'upload before' ] || exit 91
    id=$(jq 'length + 100' assets.json)
    cp "$input" "remote/$name"
    size=$(wc -c < "$input" | tr -d ' ')
    jq --arg name "$name" --argjson id "$id" --argjson size "$size" \
      '. + [{id:$id,name:$name,size:$size,state:"uploaded"}]' assets.json > assets.new
    mv assets.new assets.json
    touch uploaded
    if [ "${PUBLISH_FAIL:-}" = 'malformed upload' ]; then printf '{}'; else jq '.[-1]' assets.json; fi
    [ "${PUBLISH_FAIL:-}" != 'upload after' ] || exit 91;;
  'GET repos/owner/project/releases/assets/'*)
    [ "$headers" = ' Accept: application/octet-stream' ] || exit 99
    [ "${PUBLISH_FAIL:-}" != download ] || exit 91
    id=${endpoint##*/}
    name=$(jq -r --argjson id "$id" '.[] | select(.id == $id) | .name' assets.json)
    [ -n "$name" ] || exit 99
    if [ "${PUBLISH_FAIL:-}" = corrupt ]; then tr 'a' 'z' < "remote/$name"; else cat "remote/$name"; fi;;
  'PATCH repos/owner/project/releases/70')
    [ "$fields" = ' draft=false prerelease=true make_latest=false' ] || exit 99
    [ "${PUBLISH_FAIL:-}" != publish ] || exit 91
    [ "$(jq length assets.json)" = 7 ] || exit 99
    jq '.draft=false | .immutable=true' release.json > release.new
    mv release.new release.json
    if [ "$(jq length refs.json)" = 0 ]; then
      printf '[{"ref":"refs/tags/build-45-1111111","object":{"type":"commit","sha":"1111111111111111111111111111111111111111"}}]\n' > refs.json
    fi
    cat release.json
    [ "${PUBLISH_FAIL:-}" != 'publish after' ] || exit 91;;
  *) exit 99;;
esac
`

func newPublishFixture(t *testing.T) checkpointFixture {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("publication fixture requires jq")
	}
	f := checkpointFixture{root: t.TempDir()}
	var err error
	f.script, err = filepath.Abs("../../scripts/distribution-publish.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tools", "dist", "remote"} {
		if err := os.Mkdir(filepath.Join(f.root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testTool(t, filepath.Join(f.root, "tools"), "gh", publishFakeGH)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "PATH" || strings.HasPrefix(name, "GH") || strings.HasPrefix(name, "BUILD_") || strings.HasPrefix(name, "PUBLISH_") {
			continue
		}
		f.env = append(f.env, entry)
	}
	f.env = append(f.env, "PATH="+filepath.Join(f.root, "tools")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_REPOSITORY=owner/project", "BUILD_SHA="+checkpointSHA, "BUILD_RUN_NUMBER=45",
		"BUILD_TAG=build-45-1111111", "BUILD_RUN_ID=123", "BUILD_RUN_ATTEMPT=1", "BUILD_ARTIFACT_ID=800", "BUILD_ARTIFACT_DIGEST=sha256:"+strings.Repeat("c", 64), "GH_TOKEN=fixture-only", "PUBLISH_TEST_ROOT="+f.root)
	f.writeJSON(t, "assets.json", []any{})
	f.writeJSON(t, "refs.json", []any{})
	for _, extension := range []string{"zip", "dmg"} {
		name := "Agent-Job-Queue-Installer-0.1.0-universal." + extension
		data := []byte("accepted " + extension + " bytes")
		f.publishWrite(t, "dist/"+name, data)
		f.publishWrite(t, "dist/"+name+".sha256", []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)))
	}
	f.writeJSON(t, "dist/agent-job-queue.json", map[string]any{"schemaVersion": 1, "repository": "owner/project", "version": "0.1.0", "commit": checkpointSHA, "build": 45, "tag": "build-45-1111111", "asset": "Agent-Job-Queue-Installer-0.1.0-universal.zip", "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte("accepted zip bytes")))})
	f.publishWrite(t, "dist/agent-job-queue.rb", []byte("accepted Homebrew cask\n"))
	var stages, logs []map[string]any
	for i, name := range []string{"payload", "installer", "dmg"} {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)
		completed := ""
		if i > 0 {
			extension := "zip"
			if name == "dmg" {
				extension = "dmg"
			}
			completed = fmt.Sprintf("%x", sha256.Sum256([]byte("accepted "+extension+" bytes")))
		}
		stages = append(stages, map[string]any{"stage": name, "phase": "finished", "submission_id": id, "archive_sha256": strings.Repeat("d", 64), "completed_sha256": completed})
		logs = append(logs, map[string]any{"jobId": id, "sha256": strings.Repeat("d", 64), "status": "Accepted"})
	}
	f.writeJSON(t, "dist/apple-notarization.json", map[string]any{"schema": 1, "metadata": map[string]any{
		"schema": 2, "repository": "owner/project", "source_sha": checkpointSHA, "version": "0.1.0", "team": signingTestTeam,
		"run_id": "123", "run_number": "45", "build_attempt": "1", "artifact_id": "800", "artifact_digest": "sha256:" + strings.Repeat("c", 64),
	}, "stages": stages, "logs": logs})
	return f
}

func (f checkpointFixture) publishWrite(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f checkpointFixture) publishCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func (f checkpointFixture) publish(t *testing.T, failure string, success bool, extra ...string) {
	t.Helper()
	env := append([]string{"PUBLISH_FAIL=" + failure}, extra...)
	output, err := f.run(t, env, "dist")
	if (err == nil) != success {
		t.Fatalf("publication success=%t, error=%v: %s", success, err, output)
	}
	if success && !strings.Contains(output, "Verified immutable prerelease build-45-1111111 at "+checkpointSHA) {
		t.Fatalf("missing verification result: %s", output)
	}
}

func (f checkpointFixture) publishDraft(t *testing.T) {
	t.Helper()
	f.publish(t, "publish", false)
	if strings.Contains(f.publishCalls(t), "PATCH") {
		// This request was deliberately rejected before any state change.
		f.changeJSON(t, "release.json", func(v map[string]any) {
			if v["draft"] != true {
				t.Fatal("failed publication exposed a draft")
			}
		})
	}
	f.publishWrite(t, "calls", nil)
}

func TestDistributionPublishDraftReadbackBeforeExposure(t *testing.T) {
	f := newPublishFixture(t)
	f.publish(t, "", true)
	calls := f.publishCalls(t)
	create := strings.Index(calls, "POST repos/owner/project/releases\n")
	upload := strings.Index(calls, "POST https://uploads.github.com/")
	readback := strings.Index(calls, "GET repos/owner/project/releases/assets/")
	publish := strings.Index(calls, "PATCH repos/owner/project/releases/70\n")
	if create < 0 || upload <= create || readback <= upload || publish <= readback {
		t.Fatalf("unsafe publication ordering:\n%s", calls)
	}
	before, after, _ := strings.Cut(calls, "PATCH repos/owner/project/releases/70\n")
	if strings.Count(before, "GET repos/owner/project/releases/assets/") != 7 || strings.Count(after, "GET repos/owner/project/releases/assets/") != 7 {
		t.Fatalf("did not read all seven exact assets before and after publication:\n%s", calls)
	}
}

func TestDistributionPublishResumesInterruptedDraft(t *testing.T) {
	for _, failure := range []string{"create after", "upload before", "upload after", "publish after"} {
		t.Run(failure, func(t *testing.T) {
			f := newPublishFixture(t)
			f.publish(t, failure, false)
			f.publish(t, "", true)
			calls := f.publishCalls(t)
			if strings.Count(calls, "POST repos/owner/project/releases\n") != 1 {
				t.Fatalf("recreated an interrupted draft:\n%s", calls)
			}
			// An upload that succeeded with a lost response must be downloaded,
			// rather than overwritten or submitted a second time.
			if failure == "upload after" && strings.Count(calls, "POST https://uploads.github.com/repos/owner/project/releases/70/assets?name=Agent-Job-Queue-Installer-0.1.0-universal.dmg\n") != 1 {
				t.Fatalf("re-uploaded retained immutable bytes:\n%s", calls)
			}
		})
	}
}

func TestDistributionPublishRejectsCorruptionAndProviderAmbiguity(t *testing.T) {
	for _, failure := range []string{"list", "malformed list", "tag", "tag ambiguous", "inventory", "read", "download", "corrupt", "malformed upload", "target changes", "inventory changes", "policy disabled", "policy unreadable"} {
		t.Run(failure, func(t *testing.T) {
			f := newPublishFixture(t)
			f.publish(t, failure, false)
			calls := f.publishCalls(t)
			if strings.Contains(calls, "PATCH") || ((failure == "list" || strings.HasPrefix(failure, "tag")) && strings.Contains(calls, "POST")) {
				t.Fatalf("ambiguous provider result allowed mutation or publication:\n%s", calls)
			}
		})
	}
}

func TestDistributionPublishRejectsConflictingDraftWithoutMutation(t *testing.T) {
	for _, variant := range []string{"target", "tag", "channel", "title", "draft candidate", "remote bytes", "extra asset", "duplicate name", "duplicate ID", "unfinished asset", "source manifest", "candidate changed", "checksum", "symlink", "hidden asset", "missing asset"} {
		t.Run(variant, func(t *testing.T) {
			f := newPublishFixture(t)
			f.publishDraft(t)
			switch variant {
			case "target", "channel", "title", "draft candidate":
				f.changeJSON(t, "release.json", func(v map[string]any) {
					switch variant {
					case "target":
						v["target_commitish"] = strings.Repeat("2", 40)
					case "channel":
						v["prerelease"] = false
					case "title":
						v["name"] = "Main build 46"
					case "draft candidate":
						v["body"] = "different candidate"
					}
				})
			case "tag":
				f.writeJSON(t, "refs.json", []any{map[string]any{"ref": "refs/tags/build-45-1111111", "object": map[string]any{"type": "commit", "sha": strings.Repeat("2", 40)}}})
			case "remote bytes":
				f.publishWrite(t, "remote/agent-job-queue.rb", []byte("altered bytes"))
			case "source manifest":
				f.changeJSON(t, "dist/agent-job-queue.json", func(v map[string]any) { v["commit"] = strings.Repeat("2", 40) })
			case "candidate changed":
				f.publishWrite(t, "dist/agent-job-queue.rb", []byte("new candidate"))
			case "checksum":
				f.publishWrite(t, "dist/Agent-Job-Queue-Installer-0.1.0-universal.zip.sha256", []byte("wrong checksum"))
			case "hidden asset":
				f.publishWrite(t, "dist/.private", []byte("private"))
			case "symlink", "missing asset":
				path := filepath.Join(f.root, "dist/agent-job-queue.rb")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if variant == "symlink" {
					if err := os.Symlink(filepath.Join(f.root, "remote/agent-job-queue.rb"), path); err != nil {
						t.Fatal(err)
					}
				}
			default:
				data, err := os.ReadFile(filepath.Join(f.root, "assets.json"))
				if err != nil {
					t.Fatal(err)
				}
				var assets []map[string]any
				if err := json.Unmarshal(data, &assets); err != nil {
					t.Fatal(err)
				}
				switch variant {
				case "extra asset":
					assets = append(assets, map[string]any{"id": 200, "name": "extra.txt", "size": 1, "state": "uploaded"})
				case "duplicate name":
					assets[1]["name"] = assets[0]["name"]
				case "duplicate ID":
					assets[1]["id"] = assets[0]["id"]
				case "unfinished asset":
					assets[0]["state"] = "starter"
				}
				f.writeJSON(t, "assets.json", assets)
			}
			f.publish(t, "", false)
			if calls := f.publishCalls(t); strings.Contains(calls, "POST") || strings.Contains(calls, "PATCH") {
				t.Fatalf("conflicting state was mutated:\n%s", calls)
			}
		})
	}
}

func TestDistributionPublishExistingPublicReleaseIsReadOnlyAndExact(t *testing.T) {
	for _, variant := range []string{"identical", "missing", "different", "extra", "tag missing", "mutable"} {
		t.Run(variant, func(t *testing.T) {
			f := newPublishFixture(t)
			f.publish(t, "", true)
			f.publishWrite(t, "calls", nil)
			switch variant {
			case "missing", "extra":
				data, err := os.ReadFile(filepath.Join(f.root, "assets.json"))
				if err != nil {
					t.Fatal(err)
				}
				var assets []map[string]any
				if err := json.Unmarshal(data, &assets); err != nil {
					t.Fatal(err)
				}
				if variant == "missing" {
					assets = assets[1:]
				} else {
					assets = append(assets, map[string]any{"id": 200, "name": "extra", "size": 1, "state": "uploaded"})
				}
				f.writeJSON(t, "assets.json", assets)
			case "different":
				f.publishWrite(t, "remote/agent-job-queue.rb", []byte("different bytes"))
			case "tag missing":
				f.writeJSON(t, "refs.json", []any{})
			case "mutable":
				f.changeJSON(t, "release.json", func(v map[string]any) { v["immutable"] = false })
			}
			// Public releases from the preceding publisher have no draft marker.
			f.changeJSON(t, "release.json", func(v map[string]any) { v["body"] = "Original release notes" })
			f.publish(t, "", variant == "identical")
			if calls := f.publishCalls(t); strings.Contains(calls, "POST") || strings.Contains(calls, "PATCH") {
				t.Fatalf("public release was mutated:\n%s", calls)
			}
		})
	}
}

func TestDistributionPublishRejectsUnboundAppleEvidenceBeforeProviderAccess(t *testing.T) {
	for _, variant := range []string{"source", "artifact", "phase", "uuid", "hash", "status", "completed"} {
		t.Run(variant, func(t *testing.T) {
			f := newPublishFixture(t)
			f.changeJSON(t, "dist/apple-notarization.json", func(v map[string]any) {
				metadata := v["metadata"].(map[string]any)
				stages := v["stages"].([]any)
				logs := v["logs"].([]any)
				switch variant {
				case "source":
					metadata["source_sha"] = strings.Repeat("e", 40)
				case "artifact":
					metadata["artifact_id"] = "801"
				case "phase":
					stages[1].(map[string]any)["phase"] = "submitted"
				case "uuid":
					logs[1].(map[string]any)["jobId"] = "00000000-0000-0000-0000-000000000009"
				case "hash":
					logs[1].(map[string]any)["sha256"] = strings.Repeat("e", 64)
				case "status":
					logs[1].(map[string]any)["status"] = "Rejected"
				case "completed":
					stages[1].(map[string]any)["completed_sha256"] = strings.Repeat("e", 64)
				}
			})
			f.publish(t, "", false)
			if calls := f.publishCalls(t); calls != "" {
				t.Fatalf("unbound Apple evidence reached GitHub: %s", calls)
			}
		})
	}
}

func TestDistributionPublishBindsPartialDraftToWholeCandidate(t *testing.T) {
	f := newPublishFixture(t)
	f.publish(t, "upload after", false)
	// This cask was not uploaded before the interruption. The draft's immutable
	// inventory must still reject it before uploading any new bytes.
	f.publishWrite(t, "dist/agent-job-queue.rb", []byte("changed unuploaded asset"))
	f.publishWrite(t, "calls", nil)
	f.publish(t, "", false)
	if calls := f.publishCalls(t); strings.Contains(calls, "POST") || strings.Contains(calls, "PATCH") {
		t.Fatalf("partial draft accepted a replacement candidate:\n%s", calls)
	}
}

func TestDistributionPublishVerifiesExistingTagIdentity(t *testing.T) {
	for _, variant := range []string{"lightweight", "annotated", "annotated wrong commit", "tag moves", "tag disappears"} {
		t.Run(variant, func(t *testing.T) {
			f := newPublishFixture(t)
			object := map[string]any{"type": "commit", "sha": checkpointSHA}
			if strings.HasPrefix(variant, "annotated") {
				object = map[string]any{"type": "tag", "sha": strings.Repeat("3", 40)}
				commit := checkpointSHA
				if variant == "annotated wrong commit" {
					commit = strings.Repeat("2", 40)
				}
				f.writeJSON(t, "annotated.json", map[string]any{"sha": strings.Repeat("3", 40), "object": map[string]any{"type": "commit", "sha": commit}})
			}
			f.writeJSON(t, "refs.json", []any{
				map[string]any{"ref": "refs/tags/build-45-1111111", "object": object},
				// The endpoint also returns prefix matches. They must never be
				// mistaken for the exact release ref or treated as duplicates.
				map[string]any{"ref": "refs/tags/build-45-1111111-extra", "object": map[string]any{"type": "commit", "sha": strings.Repeat("4", 40)}},
			})
			success := variant == "lightweight" || variant == "annotated"
			failure := ""
			if variant == "tag moves" || variant == "tag disappears" {
				failure = variant
			}
			f.publish(t, failure, success)
			if !success && strings.Contains(f.publishCalls(t), "PATCH") {
				t.Fatalf("changed tag permitted publication:\n%s", f.publishCalls(t))
			}
		})
	}
}
