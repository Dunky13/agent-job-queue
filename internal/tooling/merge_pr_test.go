package tooling

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMaintainerSquashMerge(t *testing.T) {
	script, err := filepath.Abs("../../scripts/merge-pr.sh")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	canonical := "Signed-off-by: Marc Went <marc@went.io>\n"
	for _, test := range []struct {
		name, body, metadata, head, merged, verification string
		viewFails, mergeFails, verificationFails         bool
		wantMerge, wantSuccess                           bool
	}{
		{name: "primary identity", body: "Repair CI\n\n" + canonical, wantMerge: true, wantSuccess: true},
		{name: "other contributor preserved", body: "Repair CI\n\nSigned-off-by: Contributor <contributor@example.com>\n" + canonical, wantMerge: true, wantSuccess: true},
		{name: "missing signoff", body: "Repair CI\n"},
		{name: "noreply signoff", body: "Signed-off-by: Marc Went <Dunky13@users.noreply.github.com>\n"},
		{name: "duplicate signoff", body: canonical + canonical},
		{name: "extra alias", body: canonical + "Signed-off-by: Marc Went <Dunky13@users.noreply.github.com>\n"},
		{name: "case variant alias", body: canonical + "signed-off-by: MARC WENT <Dunky13@users.noreply.github.com>\n"},
		{name: "bot author", body: canonical, metadata: "dependabot[bot]\ndependabot[bot]\nmain\n" + head},
		{name: "changed author name", body: canonical, metadata: "Dunky13\nOther Name\nmain\n" + head},
		{name: "different base", body: canonical, metadata: "Dunky13\nMarc Went\nbootstrap\n" + head},
		{name: "head changed", body: canonical, metadata: "Dunky13\nMarc Went\nmain\n" + strings.Repeat("b", 40)},
		{name: "invalid head", body: canonical, head: "--admin"},
		{name: "lookup failure", body: canonical, viewFails: true},
		{name: "merge rejection", body: canonical, mergeFails: true, wantMerge: true},
		{name: "merge queued instead of completed", body: canonical, merged: "queued", wantMerge: true},
		{name: "merged noreply author", body: canonical, verification: "Marc Went\nDunky13@users.noreply.github.com\ntrue\n" + canonical, wantMerge: true},
		{name: "merged wrong author name", body: canonical, verification: "Other Name\nmarc@went.io\ntrue\n" + canonical, wantMerge: true},
		{name: "merged unverified signature", body: canonical, verification: "Marc Went\nmarc@went.io\nfalse\n" + canonical, wantMerge: true},
		{name: "merged missing certification", body: canonical, verification: "Marc Went\nmarc@went.io\ntrue\nMissing signoff", wantMerge: true},
		{name: "merged duplicate certification", body: canonical, verification: "Marc Went\nmarc@went.io\ntrue\n" + canonical + canonical, wantMerge: true},
		{name: "merged metadata lookup failure", body: canonical, verificationFails: true, wantMerge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			bodyPath := filepath.Join(root, "squash body.txt")
			if err := os.WriteFile(bodyPath, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			testTool(t, root, "gh", `case "$1 $2" in
"pr view")
  case "$*" in
    *state,mergeCommit*) printf '%s\n' "$MERGE_RESULT"; exit 0;;
  esac
  if [ "$MERGE_VIEW_FAILS" = true ]; then exit 17; fi
  printf '%s\n' "$MERGE_METADATA"
  ;;
"pr merge")
  printf '%s\n' "$@" > "$MERGE_CALLS"
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --body-file ]; then
      shift
      cat "$1" > "$MERGE_BODY"
    fi
    shift
  done
  if [ "$MERGE_FAILS" = true ]; then exit 18; fi
  ;;
api\ *)
  printf '%s\n' "$@" > "$MERGE_API_CALLS"
  if [ "$MERGE_VERIFICATION_FAILS" = true ]; then exit 19; fi
  printf '%s\n' "$MERGE_VERIFICATION"
  ;;
*) exit 99;;
esac
`)
			metadata := test.metadata
			if metadata == "" {
				metadata = "Dunky13\nMarc Went\nmain\n" + head
			}
			reviewedHead := test.head
			if reviewedHead == "" {
				reviewedHead = head
			}
			merged := test.merged
			if merged == "" {
				merged = strings.Repeat("b", 40)
			}
			verification := test.verification
			if verification == "" {
				verification = "Marc Went\nmarc@went.io\ntrue\n" + test.body
			}
			calls := filepath.Join(root, "calls")
			apiCalls := filepath.Join(root, "api-calls")
			copiedBody := filepath.Join(root, "merged-body")
			cmd := exec.Command("/bin/sh", script, "48", reviewedHead, bodyPath)
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+root,
				"MERGE_METADATA="+metadata, "MERGE_CALLS="+calls, "MERGE_BODY="+copiedBody,
				"MERGE_API_CALLS="+apiCalls,
				"MERGE_RESULT="+merged, "MERGE_VERIFICATION="+verification, "MERGE_VERIFICATION_FAILS="+strconv.FormatBool(test.verificationFails),
				"MERGE_VIEW_FAILS="+strconv.FormatBool(test.viewFails), "MERGE_FAILS="+strconv.FormatBool(test.mergeFails))
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantSuccess {
				t.Fatalf("success=%v want %v: %s", err == nil, test.wantSuccess, output)
			}
			args, err := os.ReadFile(calls)
			if !test.wantMerge {
				if !os.IsNotExist(err) {
					t.Fatalf("merge invoked after refusal: %s, %v", args, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(string(args)), "\n")
				if len(lines) != 12 || strings.Join(lines[:11], "\n") != "pr\nmerge\n48\n--repo\nDunky13/agent-job-queue\n--squash\n--author-email\nmarc@went.io\n--match-head-commit\n"+head+"\n--body-file" {
					t.Fatalf("unsafe merge arguments: %q", lines)
				}
				body, err := os.ReadFile(copiedBody)
				if err != nil || string(body) != test.body {
					t.Fatalf("squash body changed: %q, %v", body, err)
				}
				if test.wantSuccess {
					apiArgs, err := os.ReadFile(apiCalls)
					expected := "api\nrepos/Dunky13/agent-job-queue/commits/" + merged + "\n--jq\n.commit | [.author.name, .author.email, (.verification.verified | tostring), .message] | join(\"\\n\")\n"
					if err != nil || string(apiArgs) != expected {
						t.Fatalf("actual merged commit was not verified: %q, %v", apiArgs, err)
					}
				}
			}
			leftovers, err := filepath.Glob(filepath.Join(root, "agent-queue-merge.*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary merge files retained: %v, %v", leftovers, err)
			}
		})
	}
}
