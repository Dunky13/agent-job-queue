package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("git %v: %v\n%s", args, e, out)
	}
}

// gitRepository creates a private repository with one commit and a subdirectory.
func gitRepository(t *testing.T) string {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "commit", "-q", "--allow-empty", "-m", "fixture")
	if e = os.MkdirAll(filepath.Join(root, "sub", "deep"), 0700); e != nil {
		t.Fatal(e)
	}
	return root
}

// observeIn observes a family the way the controller does: from metadata that
// names only a directory.
func observeIn(t *testing.T, store *ProfileStore, name, directory, title, executable string) Object {
	t.Helper()
	identity := similarityIdentity(name)
	description := profileSimilarityDescription(Object{"title": title, "project": filepath.Base(directory), "executable": executable, "directory": directory}, map[string]profileIdentity{})
	if e := store.Observe(identity, description); e != nil {
		t.Fatal(e)
	}
	return identity
}

func matchID(t *testing.T, store *ProfileStore, identity Object) string {
	t.Helper()
	match, e := store.Match(identity)
	if e != nil {
		t.Fatal(e)
	}
	return text(match["id"])
}

func storedClass(t *testing.T, store *ProfileStore, identity Object) (profileClass, bool) {
	t.Helper()
	var class profileClass
	var found bool
	if e := store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT * FROM learning_classification WHERE family=?", identity["family"])
		class, found = profileStoredClass(row)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	return class, found
}

// observeClassified observes a family the way a submission does: with the
// repository, tool and operation already known.
func observeClassified(t *testing.T, store *ProfileStore, name, project, title, executable, repository string) Object {
	t.Helper()
	identity := similarityIdentity(name)
	description := Object{"title": title, "executable": executable, "learningTool": profileDefault(profileToolOf(executable)), "learningOperation": profileDefault(profileOperation(title))}
	if project != "" {
		description["project"] = project
	}
	if repository != "" {
		description["learningRepository"] = profileDigest(repository)
	}
	if e := store.Observe(identity, description); e != nil {
		t.Fatal(e)
	}
	return identity
}

func familyHasLegacyRow(t *testing.T, store *ProfileStore, identity Object) bool {
	t.Helper()
	found := false
	if e := store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT 1 FROM learning_similarity WHERE family=?", identity["family"])
		found = row != nil
		return e
	}); e != nil {
		t.Fatal(e)
	}
	return found
}

func TestProfileClassifyTable(t *testing.T) {
	const sentinel = "SENTINEL_SECRET_TOKEN"
	for _, entry := range []struct {
		argv            []string
		title           string
		tool, operation string
	}{
		{[]string{"go", "test", "./internal/first"}, "Go tests", "go", "tests"},
		{[]string{"go", "test", "-race", "./..."}, "Tests", "go", "race-tests"},
		{[]string{"env", "GOMAXPROCS=2", "go", "test", "./x"}, "Anything", "go", "tests"},
		{[]string{"fnm", "exec", "--using=26", "--", "pnpm", "test"}, "Anything", "javascript", "tests"},
		{[]string{"./scripts/check-all.sh"}, "check-all.sh", "shell", "checks"},
		{[]string{"/repo/run-build.sh"}, "Build", "shell", "build"},
		{[]string{"env", "FOO=1", "./tool"}, "Lint", "other", "lint"},
		{[]string{"env", "FOO=1", "mystery-tool"}, "Lint", "other", "lint"},
		{[]string{"env", "X=1", "xcodebuild", "build"}, "Build", "other", "build"},
		{[]string{"env", "X=1", "go", "test", "./x"}, "Tests", "go", "tests"},
		{[]string{"fnm", "exec", "--", "mystery-tool"}, "Tests", "other", "tests"},
		{[]string{"fnm", "exec", "--", "node", "x"}, "Tests", "javascript", "tests"},
		{[]string{"fnm", "use", "20"}, "Tests", "other", "tests"},
		{[]string{"bash", "/private/batch.sh"}, "Issue 19 browser regression tests", "shell", "browser-tests"},
		{[]string{"xcodebuild", "build"}, "Build", "other", "build"},
		{[]string{"mystery"}, "Unclassified batch 123", "other", "other"},
		{[]string{"bash", "-c", "go test ./internal/a"}, "Shell batch", "shell", "tests"},
		{[]string{"sh", "-lc", "cd web && " + sentinel + "=1 npm run lint"}, "Shell batch", "shell", "lint"},
		{[]string{"bash", "-ec", "go test -race ./... ; go test -race ./..."}, "Shell batch", "shell", "race-tests"},
		{[]string{"bash", "-c", "go test ./... && go vet ./..."}, "Shell batch", "shell", "other"},
		{[]string{"bash", "-euo", "pipefail", "-c", "go test ./internal/a"}, "Shell batch", "shell", "tests"},
		{[]string{"bash", "-o", "pipefail", "-c", "go build ./..."}, "Shell batch", "shell", "build"},
		{[]string{"bash", "-eu", "-o", "pipefail", "-c", "go build ./..."}, "Shell batch", "shell", "build"},
		{[]string{"bash", "-c", "test -f foo && echo yes"}, "Shell batch", "shell", "other"},
		{[]string{"bash", "-c", "[ -f test ] && echo yes"}, "Shell batch", "shell", "other"},
		{[]string{"bash", "-c", "test -f .env && go build ./..."}, "Shell batch", "shell", "build"},
		{[]string{"bash", "-c", "echo " + sentinel}, "Shell batch", "shell", "other"},
		{[]string{"bash", "script.sh", "-c", "go test"}, "Shell batch", "shell", "other"},
		{[]string{"bash", "-c", "opaque"}, "Build and test all modules", "shell", "other"},
		{[]string{"bash", "-c", "for d in a b; do go test ./$d; done"}, "Shell batch", "shell", "tests"},
		{[]string{"python3.14", "-c", "opaque"}, "Issue 20 regression tests", "python", "tests"},
	} {
		_, executable := SafeSummary(entry.argv)
		tool, operation := profileClassify(entry.argv, entry.title, executable)
		if tool != entry.tool || operation != entry.operation {
			t.Errorf("%v / %s: got %s %s, want %s %s", entry.argv, entry.title, tool, operation, entry.tool, entry.operation)
		}
		if !(profileClass{"", tool, operation}).valid() {
			t.Errorf("%v: classification %s %s is outside the vocabulary", entry.argv, tool, operation)
		}
		encoded, _ := json.Marshal(Object{"tool": tool, "operation": operation})
		if strings.Contains(string(encoded), sentinel) {
			t.Errorf("%v: arguments reached the classification", entry.argv)
		}
	}
}

func TestProfileInlineProgramIsBounded(t *testing.T) {
	padding := strings.Repeat("echo padding; ", profileInlineLimit)
	if got := profileInlineOperation([]string{"bash", "-c", padding + "go test ./..."}); got != "" {
		t.Fatalf("A command beyond the scan bound was classified: %q", got)
	}
	if got := profileInlineOperation([]string{"bash", "-c", "go test ./a; " + padding}); got != "tests" {
		t.Fatalf("The bounded prefix was not scanned: %q", got)
	}
}

func TestProfileVocabularyMatchesLegacyKinds(t *testing.T) {
	for _, tool := range profileTools {
		for _, operation := range profileOperations {
			legal := tool != "other" && operation != "other"
			if validProfileSimilarityKind(tool+":"+operation) != legal {
				t.Errorf("%s:%s: legacy validity disagrees with the classification vocabulary", tool, operation)
			}
		}
	}
}

func TestProfileClassificationNeverStoresRawArguments(t *testing.T) {
	const sentinel = "SENTINEL_SECRET_TOKEN"
	g := testGate(t)
	argv := []string{"bash", "-c", "API_KEY=" + sentinel + " go test ./..."}
	description, e := MakeDescription(g, argv, t.TempDir(), "")
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(description)
	if strings.Contains(string(encoded), sentinel) || description["learningOperation"] != "full-tests" || description["learningTool"] != "shell" {
		t.Fatal("The description leaked the program or missed its operation", description)
	}
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	identity := similarityIdentity("secret")
	if e = store.Observe(identity, profileSimilarityDescription(description, map[string]profileIdentity{})); e != nil {
		t.Fatal(e)
	}
	database, _ := os.ReadFile(store.path)
	if bytes.Contains(database, []byte(sentinel)) {
		t.Fatal("The database retained inline program text")
	}
	if class, ok := storedClass(t, store, identity); !ok || class.operation != "full-tests" || class.tool != "shell" {
		t.Fatal("The inline operation was not stored", class)
	}
}

func TestProfileDescriptionSurvivesTaskDescription(t *testing.T) {
	g := testGate(t)
	description, e := MakeDescription(g, []string{"bash", "-c", "go test ./..."}, t.TempDir(), "")
	if e != nil {
		t.Fatal(e)
	}
	token := strings.Repeat("a", 32)
	if e = CacheDescription(g, token, description); e != nil {
		t.Fatal(e)
	}
	g.DescriptionCache = map[string]Object{}
	described, e := DescribeTask(g, Object{"token": token}, "running")
	if e != nil {
		t.Fatal(e)
	}
	if _, leaked := described["learningOperation"]; leaked {
		t.Fatal("The status description exposes the classification")
	}
	stored, e := ReadPrivate(filepath.Join(g.Metadata, token+".json"))
	if e != nil || stored["learningOperation"] != "full-tests" || stored["learningTool"] != "shell" {
		t.Fatal("A status poll discarded the submission-time classification", stored, e)
	}
}

func TestProfileRepositoryIdentityIgnoresDirectoryAndWorktree(t *testing.T) {
	repository := gitRepository(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, repository, "worktree", "add", "-q", worktree, "-b", "linked")
	worktree, _ = filepath.EvalSymlinks(worktree)
	scopeRoot, repositoryRoot := profileDirectoryIdentity(repository)
	scopeSub, repositorySub := profileDirectoryIdentity(filepath.Join(repository, "sub"))
	scopeLinked, repositoryLinked := profileDirectoryIdentity(worktree)
	if repositoryRoot == "" || repositoryRoot != repositorySub || repositoryRoot != repositoryLinked {
		t.Fatal("Subdirectories or linked worktrees changed the repository", repositoryRoot, repositorySub, repositoryLinked)
	}
	if scopeRoot == scopeSub || scopeRoot != scopeLinked || scopeRoot != profileDirectoryScope(repository) {
		t.Fatal("The v1 scope changed", scopeRoot, scopeSub, scopeLinked)
	}
	if _, repositoryOther := profileDirectoryIdentity(gitRepository(t)); repositoryOther == repositoryRoot {
		t.Fatal("Two repositories share an identity")
	}
	if scope, repository := profileDirectoryIdentity(t.TempDir()); scope == "" || repository != "" {
		t.Fatal("A plain directory has a repository", scope, repository)
	}
	if scope, repository := profileDirectoryIdentity("relative"); scope != "" || repository != "" {
		t.Fatal("A relative path was identified")
	}
}

func TestProfileGroupsSubdirectoriesAndSeparatesEverythingElse(t *testing.T) {
	store := fixtureProfileStore(t)
	repository, other := gitRepository(t), gitRepository(t)
	plain := t.TempDir()
	root := observeIn(t, store, "root", repository, "Go tests", "go")
	sub := observeIn(t, store, "sub", filepath.Join(repository, "sub"), "Go tests", "go")
	deep := observeIn(t, store, "deep", filepath.Join(repository, "sub", "deep"), "Go tests", "go")
	if matchID(t, store, root) != matchID(t, store, sub) || matchID(t, store, root) != matchID(t, store, deep) {
		t.Fatal("Subdirectories of one repository do not share a group")
	}
	apart := map[string]Object{
		"another repository":  observeIn(t, store, "other repository", other, "Go tests", "go"),
		"another operation":   observeIn(t, store, "build", repository, "Go build", "go"),
		"another tool":        observeIn(t, store, "pnpm", repository, "Tests", "pnpm"),
		"not a repository":    observeIn(t, store, "plain", plain, "Go tests", "go"),
		"second plain":        observeIn(t, store, "plain two", plain, "Go tests", "go"),
		"unrecognized tool":   observeIn(t, store, "xcode", repository, "Tests", "xcodebuild"),
		"second unrecognized": observeIn(t, store, "xcode two", repository, "Tests", "xcodebuild"),
		"other operation":     observeIn(t, store, "other one", repository, "Mystery batch", "go"),
		"other in a repo":     observeIn(t, store, "other elsewhere", other, "Mystery batch", "go"),
	}
	seen := map[string]string{matchID(t, store, root): "grouped"}
	for name, identity := range apart {
		id := matchID(t, store, identity)
		if prior, duplicate := seen[id]; duplicate {
			t.Errorf("%s shares a group with %s", name, prior)
		}
		seen[id] = name
	}
	if class, ok := storedClass(t, store, apart["not a repository"]); !ok || class.repository != "" || class.tool != "go" || class.operation != "tests" {
		t.Fatal("A plain directory family was not classified for display", class, ok)
	}
	if class, ok := storedClass(t, store, apart["unrecognized tool"]); !ok || class.tool != "other" || class.operation != "tests" {
		t.Fatal("An unrecognized tool was not stored", class, ok)
	}
	if class, _ := storedClass(t, store, apart["other operation"]); class.operation != "other" {
		t.Fatal("An unrecognized operation was not stored as other", class)
	}
	if second := observeIn(t, store, "other two", repository, "Mystery batch", "go"); matchID(t, store, second) != matchID(t, store, apart["other operation"]) {
		t.Fatal("Workloads with an unknown operation do not share within a repository and tool")
	}
}

func TestProfileRepositoryFillsLaterButNeverChanges(t *testing.T) {
	store := fixtureProfileStore(t)
	repository, other := gitRepository(t), gitRepository(t)
	identity := observeIn(t, store, "first", t.TempDir(), "Go tests", "go")
	if class, ok := storedClass(t, store, identity); !ok || class.repository != "" {
		t.Fatal("Unverified family has a repository", class)
	}
	description := profileSimilarityDescription(Object{"title": "Go build", "project": "p", "executable": "go", "directory": repository}, map[string]profileIdentity{})
	if e := store.Observe(identity, description); e != nil {
		t.Fatal(e)
	}
	class, _ := storedClass(t, store, identity)
	if class.repository == "" || class.operation != "tests" {
		t.Fatal("A later observation did not fill the repository only", class)
	}
	filled := class.repository
	description = profileSimilarityDescription(Object{"title": "Go tests", "project": "p", "executable": "go", "directory": other}, map[string]profileIdentity{})
	if e := store.Observe(identity, description); e != nil {
		t.Fatal(e)
	}
	if class, _ = storedClass(t, store, identity); class.repository != filled {
		t.Fatal("An observed repository was overwritten")
	}
}

func TestProfileOperatorCorrectionsSurviveRepositoryGrouping(t *testing.T) {
	store := fixtureProfileStore(t)
	repository := gitRepository(t)
	a := observeIn(t, store, "a", repository, "Go tests", "go")
	b := observeIn(t, store, "b", filepath.Join(repository, "sub"), "Go tests", "go")
	revision, _ := store.Revision()
	if _, e := store.Split(matchID(t, store, a), text(a["family"]), revision); e != nil {
		t.Fatal(e)
	}
	c := observeIn(t, store, "c", filepath.Join(repository, "sub", "deep"), "Go tests", "go")
	if matchID(t, store, a) == matchID(t, store, b) || matchID(t, store, a) == matchID(t, store, c) {
		t.Fatal("Automatic grouping reopened an operator split")
	}
	// A pinned profile is neither a source nor a target of a later merge.
	pinned := matchID(t, store, a)
	revision, _ = store.Revision()
	if e := store.Edit(pinned, revision, "Mine", 4*GiB, nil); e != nil {
		t.Fatal(e)
	}
	d := observeIn(t, store, "d", repository, "Go tests", "go")
	if matchID(t, store, d) == pinned || matchID(t, store, d) == matchID(t, store, b) || matchID(t, store, d) != matchID(t, store, c) {
		t.Fatal("A pinned profile joined an automatic group", matchID(t, store, d))
	}
}

func TestProfileCompatibleWithOldClients(t *testing.T) {
	store := fixtureProfileStore(t)
	repository := gitRepository(t)
	observeIn(t, store, "tests", repository, "Go tests", "go")
	observeIn(t, store, "other", repository, "Mystery batch", "go")
	observeIn(t, store, "unrecognized", repository, "Tests", "xcodebuild")
	columns := map[string]int{"learning_state": 2, "learning_profiles": 7, "learning_members": 6, "learning_resets": 2, "learning_retained": 7, "learning_similarity": 3, "learning_similarity_state": 2}
	if e := store.connection(true, func(c profileSQL) error {
		for table, want := range columns {
			rows, e := profileRows(c, "PRAGMA table_info("+table+")")
			if e != nil || len(rows) != want {
				t.Errorf("%s changed shape: %d columns, want %d (%v)", table, len(rows), want, e)
			}
		}
		rows, e := profileRows(c, "SELECT * FROM learning_similarity")
		if e != nil {
			return e
		}
		for _, row := range rows {
			if !profileHash.MatchString(text(row["scope"])) || !validProfileSimilarityKind(text(row["kind"])) {
				t.Errorf("An old client would reject %v", row)
			}
		}
		other := similarityIdentity("other")
		if row, e := profileRow(c, "SELECT 1 FROM learning_similarity WHERE family=?", other["family"]); e != nil || row != nil {
			t.Error("A family classified other has a similarity row", e)
		}
		// Old clients insert exactly three values and group with the old plan.
		if e = profileExec(c, "INSERT OR IGNORE INTO learning_similarity VALUES (?,?,?)", profileDigest("old family"), profileDigest("old scope"), "go:tests"); e != nil {
			return e
		}
		rows, e = profileRows(c, "SELECT * FROM learning_similarity")
		for _, row := range rows {
			if !validProfileSimilarityKind(text(row["kind"])) {
				t.Errorf("An invalid kind appeared: %v", row)
			}
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestProfileOldClientQueuePlanLoadsNewDatabase(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	for _, entry := range []struct{ name, title, executable string }{{"a", "Go tests", "go"}, {"b", "Mystery batch", "go"}, {"c", "Tests", "xcodebuild"}} {
		observeIn(t, store, entry.name, repository, entry.title, entry.executable)
	}
	if _, e = LearningRegroup(g, false, nil); e != nil {
		t.Fatal("The preview rejected a database holding new rows", e)
	}
	if e = store.connection(false, func(c profileSQL) error {
		rows, e := profileRows(c, "SELECT kind FROM learning_similarity")
		for _, row := range rows {
			if !validProfileSimilarityKind(text(row["kind"])) {
				t.Errorf("Old clients would reject %v", row)
			}
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestProfileRegroupBackfillAliasesDeletedWorktreesAndKeepsAmbiguousUnknown(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, repository, "worktree", "add", "-q", worktree, "-b", "linked")
	worktree, _ = filepath.EvalSymlinks(worktree)
	if e = os.MkdirAll(filepath.Join(worktree, "sub"), 0700); e != nil {
		t.Fatal(e)
	}
	liveScope, liveRepository := profileDirectoryIdentity(filepath.Join(repository, "sub"))
	deletedScope, _ := profileDirectoryIdentity(filepath.Join(worktree, "sub"))
	if liveScope != deletedScope {
		t.Fatal("Fixture: the worktree should share its relative scope")
	}
	// Three old families: the live checkout, a deleted worktree sharing its
	// scope, and one in a directory that never verified.
	live, retired, stranger := similarityIdentity("live"), similarityIdentity("retired"), similarityIdentity("stranger")
	for i, identity := range []Object{live, retired, stranger} {
		if e = store.Observe(identity, Object{"title": "Go regression tests", "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		recordProfile(t, store, fmt.Sprintf("%032x", i+1), identity, GiB, 1, true, profileZero(), 0)
	}
	if e = store.connection(true, func(c profileSQL) error {
		for _, entry := range []struct {
			identity Object
			scope    string
		}{{live, liveScope}, {retired, deletedScope}, {stranger, profileDigest("elsewhere")}} {
			if e := profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", entry.identity["family"], entry.scope, "go:tests"); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(g.Metadata, fmt.Sprintf("%032x.json", 1)), Object{"title": "Go regression tests", "project": "fixture", "executable": "go", "directory": filepath.Join(repository, "sub")})
	runGit(t, repository, "worktree", "remove", "--force", worktree)
	preview, e := LearningRegroup(g, false, nil)
	if e != nil || metric(preview["profilesBefore"]) != 3 || metric(preview["profilesAfter"]) != 2 || metric(preview["familiesGrouped"]) != 2 {
		t.Fatal("The preview missed the deleted worktree", preview, e)
	}
	saved, e := LearningRegroup(g, true, Object{"revision": preview["revision"]})
	if e != nil {
		t.Fatal(e)
	}
	catalog, _ := store.Catalog(0, 100, "")
	if metric(catalog["total"]) != metric(preview["profilesAfter"]) || metric(saved["revision"]) != metric(preview["revision"])+1 {
		t.Fatal("Preview and apply disagree", preview, catalog)
	}
	for identity, want := range map[string]string{text(live["family"]): liveRepository, text(retired["family"]): liveRepository, text(stranger["family"]): ""} {
		class, ok := storedClass(t, store, Object{"family": identity})
		if !ok || class.repository != want || class.tool != "go" || class.operation != "tests" {
			t.Errorf("%s: classified %+v", identity[:8], class)
		}
	}
	if matchID(t, store, live) != matchID(t, store, retired) || matchID(t, store, live) == matchID(t, store, stranger) {
		t.Fatal("Aliased families did not group, or the stranger joined")
	}
	if e := store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT COUNT(*) AS n FROM learning_members WHERE family NOT IN (SELECT family FROM learning_classification)")
		if e == nil && metric(row["n"]) != 0 {
			t.Errorf("%v members have no classification", row["n"])
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

// A family's stored scope can differ from the one its checkout yields today, and
// running in two subdirectories of one repository is not a disagreement.
func TestProfileRegroupLearnsRepositoryFromStoredScopeAndSubdirectories(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	if e = os.MkdirAll(filepath.Join(repository, "sub"), 0700); e != nil {
		t.Fatal(e)
	}
	_, want := profileDirectoryIdentity(repository)
	seed, retired, roaming := similarityIdentity("seed"), similarityIdentity("retired"), similarityIdentity("roaming")
	description := Object{"title": "Go tests", "project": "fixture", "executable": "go"}
	stale := profileDigest("a scope no checkout yields today")
	for i, identity := range []Object{seed, retired, roaming} {
		if e = store.Observe(identity, description); e != nil {
			t.Fatal(e)
		}
		recordProfile(t, store, fmt.Sprintf("%032x", i+1), identity, GiB, 1, true, profileZero(), 0)
	}
	recordProfile(t, store, fmt.Sprintf("%032x", 4), roaming, GiB, 1, true, profileZero(), 0)
	recordProfile(t, store, fmt.Sprintf("%032x", 5), roaming, GiB, 1, true, profileZero(), 0)
	if e = store.connection(true, func(c profileSQL) error {
		for _, identity := range []Object{seed, retired} {
			if e := profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", identity["family"], stale, "go:tests"); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	for token, directory := range map[int]string{1: repository, 3: repository, 4: filepath.Join(repository, "sub"), 5: repository} {
		mustWrite(t, filepath.Join(g.Metadata, fmt.Sprintf("%032x.json", token)), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": directory})
	}
	if _, e = LearningRegroup(g, true, Object{"revision": 0}); e != nil {
		t.Fatal(e)
	}
	for name, identity := range map[string]Object{"seed": seed, "retired": retired, "roaming": roaming} {
		if class, ok := storedClass(t, store, identity); !ok || class.repository != want {
			t.Errorf("%s: classified %+v", name, class)
		}
	}
	if matchID(t, store, seed) != matchID(t, store, retired) || matchID(t, store, seed) != matchID(t, store, roaming) {
		t.Fatal("Families of one repository did not share a group")
	}
	// Two directories are not a conflict, but they name no single legacy scope,
	// and a later file agreeing with the first cannot bring one back.
	if familyHasLegacyRow(t, store, roaming) {
		t.Fatal("A family seen in two directories received a legacy scope")
	}
}

func TestProfileAliasAmbiguityAndConflicts(t *testing.T) {
	aliases := map[string]string{}
	profileAddAlias(aliases, "scope", "repository-a")
	profileAddAlias(aliases, "scope", "repository-a")
	if aliases["scope"] != "repository-a" {
		t.Fatal("A consistent alias was lost")
	}
	profileAddAlias(aliases, "scope", "repository-b")
	profileAddAlias(aliases, "scope", "repository-a")
	if aliases["scope"] != "" {
		t.Fatal("An ambiguous alias resolved to a repository")
	}
	profileAddAlias(aliases, "", "repository-a")
	profileAddAlias(aliases, "plain", "")
	if _, ok := aliases[""]; ok || len(aliases) != 1 {
		t.Fatal("An unverified directory taught an alias", aliases)
	}
	history := &profileHistory{descriptions: map[string]Object{}, aliases: map[string]string{"known": "repo", "doubtful": ""}, repositories: map[string]string{"moved": "repo", "both": ""}}
	member := func(family string) Object { return Object{"family": family, "title": "Go tests", "executable": "go"} }
	for _, entry := range []struct {
		family, scope, want string
	}{{"a", "known", "repo"}, {"b", "doubtful", ""}, {"c", "unseen", ""}, {"moved", "unseen", "repo"}, {"both", "doubtful", ""}} {
		class := profileBackfillClass(member(entry.family), "go:tests", entry.scope, history)
		if class.repository != entry.want || class.tool != "go" || class.operation != "tests" {
			t.Errorf("%s: %+v", entry.family, class)
		}
	}
	// Operation: the legacy kind, then the title, then the executable's tool.
	if class := profileBackfillClass(Object{"family": "x", "title": "Build", "executable": "make"}, "go:lint", "", history); class.tool != "go" || class.operation != "lint" {
		t.Error("The legacy kind did not win", class)
	}
	if class := profileBackfillClass(Object{"family": "x", "title": "Build", "executable": "make"}, "", "", history); class.tool != "make" || class.operation != "build" {
		t.Error("The title and executable were ignored", class)
	}
	if class := profileBackfillClass(Object{"family": "x", "title": "Shell batch", "executable": "bash"}, "", "", history); class.tool != "shell" || class.operation != "other" {
		t.Error("Only the member decides tool and operation", class)
	}
}

func TestProfileMetadataNamingTwoRepositoriesLeavesFamilyUnshared(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	first, second := gitRepository(t), gitRepository(t)
	peer := observeIn(t, store, "peer", first, "Go tests", "go")
	identity := similarityIdentity("moved")
	if e = store.Observe(identity, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
		t.Fatal(e)
	}
	// The third file agrees with the first; one other repository still decides.
	for i, directory := range []string{first, second, first} {
		token := fmt.Sprintf("%032x", i+1)
		recordProfile(t, store, token, identity, GiB, 1, true, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, token+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": directory})
	}
	if _, e = LearningRegroup(g, true, Object{"revision": 0}); e != nil {
		t.Fatal(e)
	}
	if class, ok := storedClass(t, store, identity); !ok || class.repository != "" || class.operation != "tests" {
		t.Fatal("Metadata naming two repositories produced a repository", class, ok)
	}
	if matchID(t, store, identity) == matchID(t, store, peer) {
		t.Fatal("A family in two repositories shared a group")
	}
}

func TestProfileBackfillGateAndExplicitRegroup(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	version := func() float64 {
		var v float64
		if e := store.connection(false, func(c profileSQL) error {
			row, e := profileRow(c, "SELECT version FROM learning_similarity_state WHERE id=1")
			v = metric(row["version"])
			return e
		}); e != nil {
			t.Fatal(e)
		}
		return v
	}
	repository := gitRepository(t)
	for i := 0; i < 2; i++ {
		identity := similarityIdentity(fmt.Sprint(i))
		if e = store.Observe(identity, Object{"title": "Go regression tests", "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		token := fmt.Sprintf("%032x", i+1)
		recordProfile(t, store, token, identity, GiB, 1, true, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, token+".json"), Object{"title": "Go regression tests", "project": "fixture", "executable": "go", "directory": filepath.Join(repository, []string{"sub", "sub/deep"}[i])})
	}
	// Pretend an earlier controller finished only the first backfill.
	if e = store.connection(true, func(c profileSQL) error {
		return profileExec(c, "UPDATE learning_similarity_state SET version=1 WHERE id=1")
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = LearningRegroup(g, true, Object{"revision": 0}); e != nil || version() != 1 {
		t.Fatal("An explicit regroup marked the controller backfill complete", version(), e)
	}
	if e = store.backfillSimilarity(g); e != nil || version() != 2 {
		t.Fatal("The controller did not mark version 2", version(), e)
	}
	// A rerun would classify every registered member. Register one it could reach.
	late := similarityIdentity("late")
	if e = store.Observe(late, Object{}); e != nil {
		t.Fatal(e)
	}
	if e = store.backfillSimilarity(g); e != nil || version() != 2 {
		t.Fatal(e)
	}
	if _, ok := storedClass(t, store, late); ok {
		t.Fatal("A completed backfill ran again")
	}
}

func TestProfileRegroupRejectsStaleRevisionWithoutWriting(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	identity := similarityIdentity("x")
	if e = store.Observe(identity, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
		t.Fatal(e)
	}
	recordProfile(t, store, strings.Repeat("1", 32), identity, GiB, 1, true, profileZero(), 0)
	mustWrite(t, filepath.Join(g.Metadata, strings.Repeat("1", 32)+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": repository})
	before, _ := os.ReadFile(store.path)
	if _, e = LearningRegroup(g, true, Object{"revision": 5}); e == nil {
		t.Fatal("A stale revision was applied")
	}
	// The outer check races with other writers; the transaction must repeat it.
	if _, e = store.regroup(&profileHistory{descriptions: map[string]Object{}, aliases: map[string]string{}}, 5, true); e == nil {
		t.Fatal("A stale revision was applied inside the transaction")
	}
	if preview, e := LearningRegroup(g, false, nil); e != nil || preview["applied"] != false {
		t.Fatal(preview, e)
	}
	if after, _ := os.ReadFile(store.path); !bytes.Equal(before, after) {
		t.Fatal("A rejected or preview regroup wrote the database")
	}
}

func TestProfileRegistryEvictionClearsClassification(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := observeIn(t, store, "evictable", gitRepository(t), "Go tests", "go")
	if e := store.connection(true, func(c profileSQL) error {
		for i := 0; i < maxRegisteredFamilies; i++ {
			family := profileDigest([]any{"filler", i})
			if e := profileExec(c, "INSERT INTO learning_profiles VALUES (?,NULL,NULL,NULL,1,0,?)", family, 1e12); e != nil {
				return e
			}
			if e := profileExec(c, "INSERT INTO learning_members VALUES (?,?,NULL,NULL,NULL,?)", family, family, 1e12); e != nil {
				return e
			}
		}
		if e := profileExec(c, "UPDATE learning_members SET updated=1 WHERE family=?", identity["family"]); e != nil {
			return e
		}
		slot, e := profileRegistrySlot(c)
		if e != nil || !slot {
			t.Fatal("No slot was freed", slot, e)
		}
		for _, table := range []string{"learning_members", "learning_similarity", "learning_classification"} {
			if row, e := profileRow(c, "SELECT 1 FROM "+table+" WHERE family=?", identity["family"]); e != nil || row != nil {
				t.Errorf("%s kept an evicted family (%v)", table, e)
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestProfileReadonlyOldDatabaseWithoutClassificationNeverMigrates(t *testing.T) {
	store := fixtureProfileStore(t)
	identity := observeIn(t, store, "old", gitRepository(t), "Go tests", "go")
	if e := store.connection(true, func(c profileSQL) error {
		return profileExec(c, "DROP TABLE learning_classification")
	}); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(store.path)
	readonly, e := NewProfileStore(store.state, true)
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := readonly.CatalogFiltered(0, 100, "", ProfileFilter{Operation: "tests"})
	if e != nil || metric(catalog["total"]) != 0 {
		t.Fatal("Unclassified families matched an operation filter", catalog, e)
	}
	if hierarchy, e := readonly.Hierarchy(10); e != nil || len(hierarchy["projects"].([]Object)) != 1 {
		t.Fatal("The hierarchy needs the classification table", hierarchy, e)
	}
	if _, e = readonly.Detail(text(identity["family"]), ""); e != nil {
		t.Fatal(e)
	}
	if after, _ := os.ReadFile(store.path); !bytes.Equal(before, after) {
		t.Fatal("A read migrated the database")
	}
}

func TestProfileCatalogFiltersAndHierarchy(t *testing.T) {
	store := fixtureProfileStore(t)
	add := func(name, project, title, executable, repository string) Object {
		t.Helper()
		return observeClassified(t, store, name, project, title, executable, repository)
	}
	// "big" has three families in two operations; "pair" two; "single" one;
	// "unclassified" has no operation.
	add("big tests one", "big", "Go tests", "go", "big")
	add("big tests two", "big", "Go tests", "go", "big")
	add("big build", "big", "Go build", "go", "big")
	add("pair lint", "pair", "Lint", "pnpm", "pair")
	add("single", "single", "Go tests", "go", "single")
	add("mystery", "pair", "Mystery", "tool", "pair")
	hierarchy, e := store.Hierarchy(100)
	if e != nil {
		t.Fatal(e)
	}
	projects := hierarchy["projects"].([]Object)
	if len(projects) != 2 || projects[0]["project"] != "big" || projects[0]["collapsed"] != false || projects[1]["collapsed"] != true {
		t.Fatalf("Unexpected hierarchy %v", projects)
	}
	if metric(projects[0]["workloadCount"]) != 3 || metric(projects[0]["groupCount"]) != 2 {
		t.Fatalf("big counts %v", projects[0])
	}
	operations := projects[0]["operations"].([]Object)
	if len(operations) != 2 || operations[0]["operation"] != "tests" || metric(operations[0]["workloadCount"]) != 2 || metric(operations[0]["groupCount"]) != 1 {
		t.Fatalf("big operations %v", operations)
	}
	if projects[1]["project"] != "Other projects" || metric(projects[1]["workloadCount"]) != 3 || metric(projects[1]["projectCount"]) != 2 {
		t.Fatalf("collapsed row %v", projects[1])
	}
	// A real project literally named like the collapsed row folds like any other.
	add("lookalike", "Other projects", "Go tests", "go", "lookalike")
	hierarchy, _ = store.Hierarchy(100)
	rows := hierarchy["projects"].([]Object)
	named := 0
	for _, row := range rows {
		if row["project"] == "Other projects" && row["collapsed"] == false {
			named++
		}
	}
	if named != 0 {
		t.Fatal("A one-workload project named like the collapsed row was not folded")
	}
	list := func(filter ProfileFilter) Object {
		t.Helper()
		result, e := store.CatalogFiltered(0, 100, "", filter)
		if e != nil {
			t.Fatal(e)
		}
		return result
	}
	if r := list(ProfileFilter{Project: "big"}); metric(r["total"]) != 2 {
		t.Fatal("Project filter", r)
	}
	if r := list(ProfileFilter{Project: "big", Operation: "build"}); metric(r["total"]) != 1 {
		t.Fatal("Project and operation filter", r)
	}
	if r := list(ProfileFilter{Operation: "tests"}); metric(r["total"]) != 3 {
		t.Fatal("Operation filter", r)
	}
	other := list(ProfileFilter{OtherProjects: true})
	if metric(other["total"]) != 4 {
		t.Fatal("The collapsed set does not match the hierarchy row", other)
	}
	for _, p := range other["profiles"].([]Object) {
		if p["project"] == "big" {
			t.Fatal("A named project leaked into the collapsed set")
		}
	}
	if r := list(ProfileFilter{OtherProjects: true, Operation: "other"}); metric(r["total"]) != 1 {
		t.Fatal("Collapsed set and operation filter", r)
	}
	if page, e := store.CatalogFiltered(1, 1, "", ProfileFilter{Project: "big"}); e != nil || len(page["profiles"].([]Object)) != 1 || metric(page["total"]) != 2 {
		t.Fatal("Filters apply after paging", page, e)
	}
	if r, e := store.CatalogFiltered(0, 100, "build", ProfileFilter{Project: "big"}); e != nil || metric(r["total"]) != 1 {
		t.Fatal("Search and filter do not combine", r, e)
	}
	if limited, _ := store.Hierarchy(1); len(limited["projects"].([]Object)) != 1 || metric(limited["total"]) != 1 {
		t.Fatal("The hierarchy limit counts the other-projects row", limited)
	}
	for _, bad := range []ProfileFilter{{Operation: "typo"}, {Project: strings.Repeat("x", 121)}, {Project: "a\nb"}, {Project: " pad"}, {Project: "big", OtherProjects: true}} {
		if _, e := store.CatalogFiltered(0, 100, "", bad); e == nil {
			t.Errorf("Invalid filter accepted: %+v", bad)
		}
	}
	for _, limit := range []int{0, 101, -1} {
		if _, e := store.Hierarchy(limit); e == nil {
			t.Errorf("Invalid hierarchy limit %d accepted", limit)
		}
	}
}

func TestProfileCLICatalogFilters(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	identity := similarityIdentity("cli")
	description := profileSimilarityDescription(Object{"title": "Go tests", "project": "cli", "executable": "go", "directory": repository}, map[string]profileIdentity{})
	if e = store.Observe(identity, description); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) (int, Object) {
		var output, stderr bytes.Buffer
		code := LearningCLI(g, append([]string{"list", "--json"}, args...), strings.NewReader(""), &output, &stderr)
		result := Object{}
		_ = json.Unmarshal(output.Bytes(), &result)
		return code, result
	}
	if code, r := run("--project", "cli", "--operation=tests"); code != 0 || metric(r["total"]) != 1 {
		t.Fatal("Filtered list", code, r)
	}
	if code, r := run("--operation", "build"); code != 0 || metric(r["total"]) != 0 {
		t.Fatal("Filtered list should be empty", code, r)
	}
	if code, r := run("--other-projects"); code != 0 || metric(r["total"]) != 1 {
		t.Fatal("A one-workload project is in the collapsed set", code, r)
	}
	if code, r := run("--hierarchy"); code != 0 || len(r["projects"].([]any)) != 1 || r["controllerStatus"] == nil {
		t.Fatal("Hierarchy", code, r)
	}
	for _, bad := range [][]string{
		{"--operation", "nope"}, {"--project", ""}, {"--operation", ""}, {"--project", "a", "--project", "b"},
		{"--project", "a", "--other-projects"}, {"--other-projects", "--other-projects"},
		{"--hierarchy", "--project", "a"}, {"--hierarchy", "--query", "a"}, {"--hierarchy", "--offset", "1"},
		{"--hierarchy", "--offset", "0"}, {"--hierarchy", "--query", ""}, {"--hierarchy", "--query="}, {"--hierarchy", "--offset=0"}, {"--hierarchy", "--other-projects"},
		{"--hierarchy", "--limit", "101"}, {"--project"}, {"--kind", "go:tests"},
	} {
		if code, r := run(bad...); code != 125 || r["error"] == nil {
			t.Errorf("%v accepted: %d %v", bad, code, r)
		}
	}
}

func TestProfilePreviewMatchesAppliedCatalog(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	for i := 0; i < 5; i++ {
		identity := similarityIdentity(fmt.Sprint(i))
		title := []string{"Go tests", "Go tests", "Go build", "Go tests", "Mystery"}[i]
		if e = store.Observe(identity, Object{"title": title, "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		token := fmt.Sprintf("%032x", i+1)
		recordProfile(t, store, token, identity, GiB, 1, i != 3, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, token+".json"), Object{"title": title, "project": "fixture", "executable": "go", "directory": filepath.Join(repository, []string{"", "sub", "", "sub/deep", ""}[i])})
	}
	revision, _ := store.Revision()
	if e = store.Edit(text(similarityIdentity("1")["family"]), revision, "Pinned", nil, nil); e != nil {
		t.Fatal(e)
	}
	preview, e := LearningRegroup(g, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	saved, e := LearningRegroup(g, true, Object{"revision": preview["revision"]})
	if e != nil {
		t.Fatal(e)
	}
	catalog, _ := store.Catalog(0, 100, "")
	if metric(catalog["total"]) != metric(preview["profilesAfter"]) || metric(preview["profilesBefore"])-metric(preview["profilesAfter"]) != 1 {
		t.Fatal("Preview counts differ from the applied catalog", preview, catalog)
	}
	if !reflect.DeepEqual(saved["groups"], preview["groups"]) || saved["familiesGrouped"] != preview["familiesGrouped"] {
		t.Fatal("Applied plan differs from the preview", saved, preview)
	}
}

func TestProfilePlanMergesRules(t *testing.T) {
	class := func(repository, tool, operation string) profileClass {
		return profileClass{repository: repository, tool: tool, operation: operation}
	}
	repoA, repoB := profileDigest("repo a"), profileDigest("repo b")
	member := func(profile string, pinned, classified bool, c profileClass) profileMemberClass {
		return profileMemberClass{family: "family-" + profile, profile: profile, pinned: pinned, classified: classified, class: c}
	}
	for _, entry := range []struct {
		name    string
		members []profileMemberClass
		want    int
	}{
		{"same class merges", []profileMemberClass{member("b", false, true, class(repoA, "go", "tests")), member("a", false, true, class(repoA, "go", "tests"))}, 1},
		{"different repositories stay apart", []profileMemberClass{member("a", false, true, class(repoA, "go", "tests")), member("b", false, true, class(repoB, "go", "tests"))}, 0},
		{"different operations stay apart", []profileMemberClass{member("a", false, true, class(repoA, "go", "tests")), member("b", false, true, class(repoA, "go", "lint"))}, 0},
		{"different tools stay apart", []profileMemberClass{member("a", false, true, class(repoA, "go", "tests")), member("b", false, true, class(repoA, "cargo", "tests"))}, 0},
		{"no repository stays alone", []profileMemberClass{member("a", false, true, class("", "go", "tests")), member("b", false, true, class("", "go", "tests"))}, 0},
		{"unrecognized tool stays alone", []profileMemberClass{member("a", false, true, class(repoA, "other", "tests")), member("b", false, true, class(repoA, "other", "tests"))}, 0},
		{"unknown operation shares", []profileMemberClass{member("a", false, true, class(repoA, "go", "other")), member("b", false, true, class(repoA, "go", "other"))}, 1},
		{"pinned profile is untouched", []profileMemberClass{member("a", true, true, class(repoA, "go", "tests")), member("b", false, true, class(repoA, "go", "tests"))}, 0},
		{"unclassified member blocks", []profileMemberClass{member("a", false, false, class(repoA, "go", "tests")), member("b", false, true, class(repoA, "go", "tests"))}, 0},
		{"mixed profile is never split", []profileMemberClass{member("a", false, true, class(repoA, "go", "tests")), {family: "second", profile: "a", classified: true, class: class(repoA, "go", "lint")}, member("b", false, true, class(repoA, "go", "tests"))}, 0},
	} {
		merges := profilePlanMerges(entry.members)
		if len(merges) != entry.want {
			t.Errorf("%s: %d merges, want %d (%+v)", entry.name, len(merges), entry.want, merges)
		}
	}
	merges := profilePlanMerges([]profileMemberClass{member("c", false, true, class(repoA, "go", "tests")), member("a", false, true, class(repoA, "go", "tests")), member("b", false, true, class(repoA, "go", "tests"))})
	if len(merges) != 1 || merges[0].target != "a" || len(merges[0].sources) != 2 {
		t.Fatal("Merging did not keep the lowest profile", merges)
	}
}

func TestProfileUnwrapBehaviours(t *testing.T) {
	for _, entry := range []struct {
		argv []string
		want []string
		ok   bool
	}{
		{[]string{"go", "test"}, []string{"go", "test"}, true},
		{[]string{"env", "A=1", "B=2", "go", "test"}, []string{"go", "test"}, true},
		{[]string{"/usr/bin/env", "--", "A=1", "go"}, []string{"go"}, true},
		{[]string{"env", "A=1", "env", "B=2", "go"}, []string{"go"}, true},
		{[]string{"env", "-i", "A=1", "go"}, []string{"-i", "A=1", "go"}, true},
		{[]string{"env", "A=1"}, []string{}, true},
		{[]string{"fnm", "exec", "--using=26", "--", "pnpm", "test"}, []string{"pnpm", "test"}, true},
		{[]string{"env", "A=1", "fnm", "exec", "--", "env", "B=2", "node"}, []string{"node"}, true},
		{[]string{"fnm", "use", "20"}, []string{"fnm", "use", "20"}, false},
		{[]string{"fnm", "exec", "node"}, []string{"fnm", "exec", "node"}, false},
		{[]string{"fnm"}, []string{"fnm"}, false},
		{[]string{"go", "run", "A=1"}, []string{"go", "run", "A=1"}, true},
	} {
		got, ok := profileUnwrap(entry.argv)
		if ok != entry.ok || !reflect.DeepEqual(append([]string{}, got...), entry.want) {
			t.Errorf("%v: got %v %v, want %v %v", entry.argv, got, ok, entry.want, entry.ok)
		}
	}
	if profileToolOf("env") != "" || profileToolOf("fnm") != "" || profileToolOf("/x/run.sh") != "shell" || profileToolOf("go") != "go" {
		t.Fatal("A wrapper is a tool, or a script is not")
	}
	history := &profileHistory{}
	for _, executable := range []string{"env", "fnm"} {
		if class := profileBackfillClass(Object{"family": "x", "executable": executable}, "go:lint", "", history); class.tool != "go" || class.operation != "lint" {
			t.Errorf("A stored legacy kind lost to the %s wrapper: %+v", executable, class)
		}
	}
}

func TestProfileInlineScanBoundIsExact(t *testing.T) {
	// The bound is part of the behaviour, so it is spelled out here.
	exact := strings.Repeat(" ", 8192-len("go test")) + "go test"
	if got := profileInlineOperation([]string{"bash", "-c", exact}); got != "tests" {
		t.Fatalf("A program ending at the bound was not scanned: %q", got)
	}
	if got := profileInlineOperation([]string{"bash", "-c", " " + exact}); got != "" {
		t.Fatalf("A program crossing the bound was scanned: %q", got)
	}
}

func TestProfileFirstObservationHonorsStoredLegacyKind(t *testing.T) {
	store := fixtureProfileStore(t)
	repository := gitRepository(t)
	peer := observeIn(t, store, "peer", repository, "Go tests", "go")
	// Registered by an older controller: a legacy kind, no classification yet.
	old := similarityIdentity("old")
	if e := store.Observe(old, Object{}); e != nil {
		t.Fatal(e)
	}
	if e := store.connection(true, func(c profileSQL) error {
		return profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", old["family"], profileDigest("old scope"), "go:full-tests")
	}); e != nil {
		t.Fatal(e)
	}
	description := profileSimilarityDescription(Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": repository}, map[string]profileIdentity{})
	if e := store.Observe(old, description); e != nil {
		t.Fatal(e)
	}
	if class, ok := storedClass(t, store, old); !ok || class.tool != "go" || class.operation != "full-tests" || class.repository == "" {
		t.Fatal("The first classification ignored the stored kind", class, ok)
	}
	if matchID(t, store, old) == matchID(t, store, peer) {
		t.Fatal("A full-tests family merged with a tests family")
	}
}

func TestProfileCorruptOrphanMetadataDoesNotBlockBackfill(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	orphan := filepath.Join(g.Metadata, strings.Repeat("e", 32)+".json")
	if e = os.WriteFile(orphan, []byte("{truncated"), 0600); e != nil {
		t.Fatal(e)
	}
	// A readable orphan names a checkout but belongs to no family, so it teaches nothing.
	mustWrite(t, filepath.Join(g.Metadata, strings.Repeat("f", 32)+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": repository})
	history, e := store.historicalSimilarityDescriptions(g)
	if e != nil || len(history.aliases) != 0 || len(history.repositories) != 0 || len(history.descriptions) != 0 {
		t.Fatal("An orphan file broke or taught the history", history, e)
	}
	if _, e = LearningRegroup(g, false, nil); e != nil {
		t.Fatal(e)
	}
	if e = store.backfillSimilarity(g); e != nil {
		t.Fatal("A corrupt orphan held the backfill", e)
	}
	// A corrupt file that belongs to a family is skipped like an orphan.
	identity := similarityIdentity("owned")
	if e = store.Observe(identity, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
		t.Fatal(e)
	}
	recordProfile(t, store, strings.Repeat("1", 32), identity, GiB, 1, true, profileZero(), 0)
	if e = os.WriteFile(filepath.Join(g.Metadata, strings.Repeat("1", 32)+".json"), []byte("{truncated"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = LearningRegroup(g, false, nil); e != nil {
		t.Fatal("A corrupt file with a family blocked the regroup", e)
	}
}

func TestProfileMissingDirectoryIdentityNeverSpawnsGit(t *testing.T) {
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "spawned")
	if e := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	missing := filepath.Join(t.TempDir(), "deleted", "worktree")
	wantScope, wantRepository := profileDirectoryIdentity(missing)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, e := os.Stat(marker); e == nil {
		t.Fatal("Fixture: the reference computation should have used the real git")
	}
	identities := map[string]profileIdentity{}
	for range 2 {
		got := profileDescriptionIdentity(missing, identities)
		if got.scope != wantScope || got.repository != wantRepository || got.repository != "" {
			t.Fatal("A missing directory has a different identity", got, wantScope)
		}
	}
	for _, directory := range []string{"", "relative/path"} {
		if got := profileDescriptionIdentity(directory, identities); got != (profileIdentity{}) {
			t.Fatalf("%q was identified: %v", directory, got)
		}
	}
	if _, e := os.Stat(marker); e == nil {
		t.Fatal("git was spawned for a directory that does not exist")
	}
	profileDescriptionIdentity(t.TempDir(), identities)
	if _, e := os.Stat(marker); e != nil {
		t.Fatal("Fixture: an existing directory should consult git")
	}
}

func TestProfileOnlyTwoRepositoriesStayIndependentAndNothingElseBlocksSharing(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	peer := observeIn(t, store, "peer", repository, "Go tests", "go")
	_, want := profileDirectoryIdentity(repository)
	deleted := filepath.Join(t.TempDir(), "gone")
	token := 0
	describe := func(name, title string, directories ...string) Object {
		identity := similarityIdentity(name)
		if e := store.Observe(identity, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		for _, directory := range directories {
			token++
			value := fmt.Sprintf("%032x", token)
			recordProfile(t, store, value, identity, GiB, 1, true, profileZero(), 0)
			mustWrite(t, filepath.Join(g.Metadata, value+".json"), Object{"title": title, "project": "fixture", "executable": "go", "directory": directory})
		}
		return identity
	}
	kinds := similarityIdentity("kinds")
	if e = store.Observe(kinds, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
		t.Fatal(e)
	}
	for _, title := range []string{"Go tests", "Go build", "Go tests"} {
		token++
		value := fmt.Sprintf("%032x", token)
		recordProfile(t, store, value, kinds, GiB, 1, true, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, value+".json"), Object{"title": title, "project": "fixture", "executable": "go", "directory": repository})
	}
	// Metadata that claims one repository cannot hide two checkouts of different ones.
	claimed := similarityIdentity("claimed")
	if e = store.Observe(claimed, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
		t.Fatal(e)
	}
	for _, directory := range []string{repository, gitRepository(t)} {
		token++
		value := fmt.Sprintf("%032x", token)
		recordProfile(t, store, value, claimed, GiB, 1, true, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, value+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": directory, "learningRepository": want})
	}
	subdirectories := describe("subdirectories", "Go tests", repository, filepath.Join(repository, "sub"), repository)
	retired := describe("retired", "Go tests", deleted, repository)
	if _, e = LearningRegroup(g, true, Object{"revision": 0}); e != nil {
		t.Fatal(e)
	}
	// Titles that disagree do not block sharing: the label is not identity. Old
	// clients still get no legacy row, because they would see two kinds.
	if class, ok := storedClass(t, store, kinds); !ok || class.repository != want || class.operation != "tests" {
		t.Fatal("Metadata that disagrees on its label blocked the repository", class)
	}
	if matchID(t, store, kinds) != matchID(t, store, peer) || familyHasLegacyRow(t, store, kinds) {
		t.Fatal("A family with differing labels did not share, or received a conflicting legacy scope")
	}
	if class, ok := storedClass(t, store, claimed); !ok || class.repository != "" || matchID(t, store, claimed) == matchID(t, store, peer) {
		t.Fatal("Checkouts of two repositories shared a group", class)
	}
	for name, identity := range map[string]Object{"subdirectories": subdirectories, "one deleted directory": retired} {
		if class, ok := storedClass(t, store, identity); !ok || class.repository != want {
			t.Errorf("%s: a directory difference counted as a conflict: %+v", name, class)
		}
		if matchID(t, store, identity) != matchID(t, store, peer) {
			t.Errorf("%s did not share with its repository", name)
		}
	}
}

func TestProfileObservationFillsRepositoryAndRejectsInvalidLegacyKind(t *testing.T) {
	store := fixtureProfileStore(t)
	repository := gitRepository(t)
	peer := observeIn(t, store, "peer", repository, "Go tests", "go")
	_, want := profileDirectoryIdentity(repository)
	// Same title: only the repository is new.
	same := observeIn(t, store, "same", t.TempDir(), "Go tests", "go")
	if class, _ := storedClass(t, store, same); class.repository != "" {
		t.Fatal("Fixture: the repository should be unknown", class)
	}
	description := profileSimilarityDescription(Object{"title": "Go tests", "executable": "go", "directory": repository}, map[string]profileIdentity{})
	if e := store.Observe(same, description); e != nil {
		t.Fatal(e)
	}
	if class, _ := storedClass(t, store, same); class.repository != want || matchID(t, store, same) != matchID(t, store, peer) {
		t.Fatal("A later observation with the same title did not fill the repository", class)
	}
	// A different tool stored first keeps its tool.
	other := observeIn(t, store, "other", t.TempDir(), "Go tests", "go")
	later := profileSimilarityDescription(Object{"title": "Tests", "executable": "pnpm", "directory": repository}, map[string]profileIdentity{})
	if e := store.Observe(other, later); e != nil {
		t.Fatal(e)
	}
	if class, _ := storedClass(t, store, other); class.repository != want || class.tool != "go" || class.operation != "tests" || matchID(t, store, other) != matchID(t, store, peer) {
		t.Fatal("The repository fill-in changed the stored tool", class)
	}
	// A legacy kind no client could have written is treated as absent.
	broken := similarityIdentity("broken")
	if e := store.Observe(broken, Object{}); e != nil {
		t.Fatal(e)
	}
	if e := store.connection(true, func(c profileSQL) error {
		return profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", broken["family"], profileDigest("scope"), "ruby:deploy")
	}); e != nil {
		t.Fatal(e)
	}
	if e := store.Observe(broken, profileSimilarityDescription(Object{"title": "Go tests", "executable": "go", "directory": repository}, map[string]profileIdentity{})); e != nil {
		t.Fatal("An invalid stored kind failed the observation", e)
	}
	if class, ok := storedClass(t, store, broken); !ok || class.tool != "go" || class.operation != "tests" {
		t.Fatal("An invalid stored kind decided the class", class)
	}
}

func TestProfileBackfillFillsRepositoryAndPreviewMatchesApply(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	_, want := profileDirectoryIdentity(repository)
	// One family stored with an unknown repository, one already in the repository.
	unverified, verified := similarityIdentity("unverified"), observeIn(t, store, "verified", repository, "Go tests", "go")
	if e = store.Observe(unverified, profileSimilarityDescription(Object{"title": "Go tests", "project": "fixture", "executable": "go"}, map[string]profileIdentity{})); e != nil {
		t.Fatal(e)
	}
	recordProfile(t, store, strings.Repeat("1", 32), unverified, GiB, 1, true, profileZero(), 0)
	mustWrite(t, filepath.Join(g.Metadata, strings.Repeat("1", 32)+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": repository})
	if class, _ := storedClass(t, store, unverified); class.repository != "" || class.tool != "go" {
		t.Fatal("Fixture: the family should be stored without a repository", class)
	}
	preview, e := LearningRegroup(g, false, nil)
	if e != nil || metric(preview["profilesBefore"])-metric(preview["profilesAfter"]) != 1 {
		t.Fatal("The preview ignored the repository the backfill fills in", preview, e)
	}
	saved, e := LearningRegroup(g, true, Object{"revision": preview["revision"]})
	if e != nil {
		t.Fatal(e)
	}
	if class, _ := storedClass(t, store, unverified); class.repository != want || class.operation != "tests" {
		t.Fatal("The backfill did not fill the repository", class)
	}
	catalog, _ := store.Catalog(0, 100, "")
	if metric(catalog["total"]) != metric(preview["profilesAfter"]) || !reflect.DeepEqual(saved["groups"], preview["groups"]) || matchID(t, store, unverified) != matchID(t, store, verified) {
		t.Fatal("Preview and apply disagree", preview, saved, catalog)
	}
}

func TestProfileRegroupLearnsAliasFromScopeComputedNow(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	scope, want := profileDirectoryIdentity(filepath.Join(repository, "sub"))
	// The live family's stored scope teaches nothing; only its checkout does.
	live, retired := similarityIdentity("live"), similarityIdentity("retired")
	for i, identity := range []Object{live, retired} {
		if e = store.Observe(identity, Object{"title": "Go tests", "project": "fixture", "executable": "go"}); e != nil {
			t.Fatal(e)
		}
		recordProfile(t, store, fmt.Sprintf("%032x", i+1), identity, GiB, 1, true, profileZero(), 0)
	}
	if e = store.connection(true, func(c profileSQL) error {
		if e := profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", live["family"], profileDigest("stale"), "go:tests"); e != nil {
			return e
		}
		return profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", retired["family"], scope, "go:tests")
	}); e != nil {
		t.Fatal(e)
	}
	mustWrite(t, filepath.Join(g.Metadata, fmt.Sprintf("%032x.json", 1)), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": filepath.Join(repository, "sub")})
	if _, e = LearningRegroup(g, true, Object{"revision": 0}); e != nil {
		t.Fatal(e)
	}
	if class, _ := storedClass(t, store, retired); class.repository != want {
		t.Fatal("The deleted checkout was not recovered from a live scope", class)
	}
}

func TestProfileEmptyObservationStoresNoClassificationAndEditClassifiesRunOnlyFamilies(t *testing.T) {
	store := fixtureProfileStore(t)
	empty := similarityIdentity("empty")
	if e := store.Observe(empty, profileSimilarityDescription(Object{}, map[string]profileIdentity{})); e != nil {
		t.Fatal(e)
	}
	if _, ok := storedClass(t, store, empty); ok {
		t.Fatal("An empty description left a lasting classification")
	}
	// A later observation classifies it.
	observeIn(t, store, "empty", gitRepository(t), "Go tests", "go")
	if class, ok := storedClass(t, store, empty); !ok || class.tool != "go" || class.operation != "tests" {
		t.Fatal("A later observation did not classify the family", class, ok)
	}
	runOnly := similarityIdentity("run only")
	recordProfile(t, store, strings.Repeat("2", 32), runOnly, GiB, 1, true, profileZero(), 0)
	revision, _ := store.Revision()
	if e := store.Edit(text(runOnly["family"]), revision, "Mine", nil, nil); e != nil {
		t.Fatal(e)
	}
	if class, ok := storedClass(t, store, runOnly); !ok || class.tool != "other" || class.operation != "other" {
		t.Fatal("A family registered by an edit has no classification", class, ok)
	}
	if e := store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT (SELECT COUNT(*) FROM learning_members) AS members,(SELECT COUNT(*) FROM learning_classification) AS classes")
		if e == nil && (row["members"] != row["classes"] || metric(row["members"]) != 2) {
			t.Errorf("members and classifications differ: %v", row)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestProfileInvalidStoredClassificationFailsEveryReadPath(t *testing.T) {
	for name, set := range map[string]string{
		"operation":  "UPDATE learning_classification SET operation='deploy'",
		"tool":       "UPDATE learning_classification SET tool='ruby'",
		"repository": "UPDATE learning_classification SET repository='not a digest'",
	} {
		g := testGate(t)
		store, e := NewProfileStore(g.State, false)
		if e != nil {
			t.Fatal(e)
		}
		identity := observeIn(t, store, "x", gitRepository(t), "Go tests", "go")
		if e = store.connection(true, func(c profileSQL) error { return profileExec(c, set) }); e != nil {
			t.Fatal(e)
		}
		readonly, e := NewProfileStore(g.State, true)
		if e != nil {
			t.Fatal(e)
		}
		_, catalogError := readonly.Catalog(0, 100, "")
		_, hierarchyError := readonly.Hierarchy(10)
		_, detailError := readonly.Detail(text(identity["family"]), "")
		_, previewError := LearningRegroup(g, false, nil)
		for read, e := range map[string]error{"catalog": catalogError, "hierarchy": hierarchyError, "detail": detailError, "preview": previewError} {
			if e == nil || !strings.Contains(e.Error(), "Invalid stored workload classification") {
				t.Errorf("%s %s: %v", name, read, e)
			}
		}
	}
}

func TestProfileSummaryProjectOperationAndMergeLabel(t *testing.T) {
	store := fixtureProfileStore(t)
	// The most common project wins, ties go to the lower name, and none is unknown.
	if got := profileProject([]Object{{"project": "b"}, {"project": "a"}, {"project": "b"}}); got != "b" {
		t.Fatal(got)
	}
	if got := profileProject([]Object{{"project": "b"}, {"project": "a"}}); got != "a" {
		t.Fatal(got)
	}
	if got := profileProject([]Object{{"project": nil}, {}}); got != "Unknown project" {
		t.Fatal(got)
	}
	observeClassified(t, store, "one", "x", "Go tests", "go", "repo")
	observeClassified(t, store, "two", "y", "Go tests", "go", "repo")
	observeClassified(t, store, "three", "y", "Go tests", "go", "repo")
	observeClassified(t, store, "four", "", "Go build", "go", "elsewhere")
	catalog, e := store.Catalog(0, 100, "")
	if e != nil {
		t.Fatal(e)
	}
	byProject := map[string]Object{}
	for _, summary := range catalog["profiles"].([]Object) {
		byProject[text(summary["project"])] = summary
	}
	if byProject["y"] == nil || metric(byProject["y"]["memberCount"]) != 3 || byProject["y"]["operation"] != "tests" || byProject["y"]["label"] != "y · go tests" {
		t.Fatal("The merged group is mislabeled", byProject)
	}
	if byProject["Unknown project"] == nil || byProject["Unknown project"]["operation"] != "build" {
		t.Fatal("A group without a project is not unknown", byProject)
	}
	// Members with different operations share no operation.
	revision, _ := store.Revision()
	if e = store.Merge(text(byProject["y"]["id"]), text(byProject["Unknown project"]["id"]), revision); e != nil {
		t.Fatal(e)
	}
	catalog, _ = store.Catalog(0, 100, "")
	if len(catalog["profiles"].([]Object)) != 1 || catalog["profiles"].([]Object)[0]["operation"] != "other" {
		t.Fatal("A mixed group reported an operation", catalog)
	}
}

func TestProfileHierarchyLimitCountsAndOrder(t *testing.T) {
	store := fixtureProfileStore(t)
	// Inserted out of order: names and sizes both decide the order.
	var alpha Object
	for i, entry := range []struct{ project, title, executable string }{
		{"gamma", "Go tests", "go"}, {"gamma", "Go tests", "go"}, {"gamma", "Go tests", "go"},
		{"beta", "Go tests", "go"}, {"beta", "Go build", "go"}, {"beta", "Go build", "go"},
		{"alpha", "Go tests", "go"}, {"alpha", "Go tests", "go"}, {"alpha", "Go build", "go"}, {"alpha", "Lint", "pnpm"},
		{"tiny", "Go tests", "go"},
	} {
		identity := observeClassified(t, store, fmt.Sprint("workload ", i), entry.project, entry.title, entry.executable, "repo "+entry.project)
		if entry.project == "alpha" && alpha == nil {
			alpha = identity
		}
	}
	recordProfile(t, store, strings.Repeat("1", 32), alpha, GiB, 1, true, profileZero(), 0)
	recordProfile(t, store, strings.Repeat("2", 32), alpha, GiB, 1, false, profileZero(), 0)
	names := func(hierarchy Object) []string {
		result := []string{}
		for _, row := range hierarchy["projects"].([]Object) {
			result = append(result, text(row["project"]))
		}
		return result
	}
	for limit, want := range map[int][]string{1: {"alpha"}, 2: {"alpha", "beta"}, 3: {"alpha", "beta", "gamma"}, 4: {"alpha", "beta", "gamma", "Other projects"}, 100: {"alpha", "beta", "gamma", "Other projects"}} {
		hierarchy, e := store.Hierarchy(limit)
		if e != nil || !reflect.DeepEqual(names(hierarchy), want) || metric(hierarchy["total"]) != 3 || metric(hierarchy["limit"]) != float64(limit) {
			t.Fatalf("limit %d: %v %v", limit, names(hierarchy), e)
		}
	}
	hierarchy, _ := store.Hierarchy(100)
	rows := hierarchy["projects"].([]Object)
	if metric(rows[0]["workloadCount"]) != 4 || metric(rows[0]["groupCount"]) != 3 || metric(rows[0]["runCount"]) != 2 || metric(rows[3]["projectCount"]) != 1 {
		t.Fatal("Unexpected alpha counts", rows[0], rows[3])
	}
	operations := rows[0]["operations"].([]Object)
	beta := rows[1]["operations"].([]Object)
	if len(beta) != 2 || beta[0]["operation"] != "build" || metric(beta[0]["workloadCount"]) != 2 || metric(beta[0]["groupCount"]) != 1 || beta[1]["operation"] != "tests" {
		t.Fatal("Operations are not ordered by workload count", beta)
	}
	if len(operations) != 3 || operations[0]["operation"] != "tests" || metric(operations[0]["runCount"]) != 2 || operations[1]["operation"] != "build" || metric(operations[1]["runCount"]) != 0 {
		t.Fatal("Unexpected alpha operations", operations)
	}
	for _, limit := range []int{1, 2, 100} {
		if hierarchy, _ = store.Hierarchy(limit); len(hierarchy["projects"].([]Object)) > limit {
			t.Fatalf("limit %d returned %d rows", limit, len(hierarchy["projects"].([]Object)))
		}
	}
}

// shuffledHistory is the whole retroactive conclusion for one evidence order.
func shuffledHistory(evidence []profileEvidence, scopes map[string]string, members []Object) (*profileHistory, map[string]profileClass, []profileMerge) {
	history := profileAggregate(evidence, scopes)
	classes, planned := map[string]profileClass{}, []profileMemberClass{}
	for _, member := range members {
		family := text(member["family"])
		classes[family] = profileBackfillClass(member, "", scopes[family], history)
		planned = append(planned, profileMemberClass{family: family, profile: family, project: "fixture", classified: true, class: classes[family]})
	}
	return history, classes, profilePlanMerges(planned)
}

func TestProfileClassificationIsIndependentOfEvidenceOrder(t *testing.T) {
	repositoryA, repositoryB := profileDigest("repository a"), profileDigest("repository b")
	scope := func(name string) string { return profileDigest("scope " + name) }
	file := func(family, title, scopeName, kind string, identity profileIdentity) profileEvidence {
		clean := Object{"title": title, "project": "fixture", "executable": "go"}
		if kind != "" {
			clean["learningScope"], clean["learningKind"] = scope(scopeName), kind
		}
		return profileEvidence{family, clean, identity}
	}
	live := func(name, repository string) profileIdentity { return profileIdentity{scope(name), repository} }
	evidence := []profileEvidence{
		file("missing", "Go tests", "a", "", live("a", repositoryA)),
		file("kinds", "Go tests", "a", "go:tests", live("a", repositoryA)),
		file("kinds", "Go tests", "a", "go:build", live("a", repositoryA)),
		file("directories", "Go tests", "a", "go:tests", live("a", repositoryA)),
		file("directories", "Go tests", "sub", "go:tests", live("sub", repositoryA)),
		file("two", "Go tests", "a", "go:tests", live("a", repositoryA)),
		file("two", "Go tests", "b", "go:tests", live("b", repositoryB)),
		file("labels", "Go tests", "a", "go:tests", live("a", repositoryA)),
		file("labels", "Go tests for issue 7", "a", "go:tests", live("a", repositoryA)),
		file("deleted", "Go tests", "gone", "go:tests", live("gone", "")),
		file("deleted", "Go tests", "gone", "go:tests", live("gone", "")),
	}
	scopes := map[string]string{"deleted": scope("gone"), "directories": scope("a")}
	// A second checkout of repository A teaches the deleted scope its repository.
	evidence = append(evidence, file("teacher", "Go tests", "gone", "go:tests", live("gone", repositoryA)))
	members := []Object{}
	for _, family := range []string{"missing", "kinds", "directories", "two", "labels", "deleted", "teacher"} {
		members = append(members, Object{"family": family, "title": "Go tests", "executable": "go"})
	}
	wantHistory, wantClasses, wantMerges := shuffledHistory(evidence, scopes, members)
	if wantClasses["two"].repository != "" || wantClasses["deleted"].repository != repositoryA || wantClasses["kinds"].repository != repositoryA || wantClasses["missing"].repository != repositoryA {
		t.Fatal("Unexpected baseline classes", wantClasses)
	}
	if len(wantMerges) != 1 || len(wantMerges[0].sources) != 5 {
		t.Fatal("The mixed fixture should merge every family except the two-repository one", wantMerges)
	}
	if _, ok := wantHistory.descriptions["kinds"]["learningKind"]; ok || wantHistory.descriptions["labels"]["title"] != "Go tests" || wantHistory.descriptions["labels"]["learningKind"] != "go:tests" {
		t.Fatal("The legacy pair rule or the smallest description changed", wantHistory.descriptions)
	}
	random := rand.New(rand.NewSource(7))
	for i := 0; i < 30; i++ {
		random.Shuffle(len(evidence), func(a, b int) { evidence[a], evidence[b] = evidence[b], evidence[a] })
		history, classes, merges := shuffledHistory(evidence, scopes, members)
		if !reflect.DeepEqual(history, wantHistory) || !reflect.DeepEqual(classes, wantClasses) || !reflect.DeepEqual(merges, wantMerges) {
			t.Fatalf("Shuffle %d changed the conclusion", i)
		}
	}
}

func TestProfileBackfillSkipsBadRecordsAndClassifiesTheRest(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	_, want := profileDirectoryIdentity(repository)
	good := map[string]any{"title": "Go tests", "project": "fixture", "executable": "go", "directory": repository}
	healthy := observeIn(t, store, "healthy", repository, "Go tests", "go")
	recordProfile(t, store, strings.Repeat("1", 32), healthy, GiB, 1, true, profileZero(), 0)
	mustWrite(t, filepath.Join(g.Metadata, strings.Repeat("1", 32)+".json"), good)
	for i, mode := range []os.FileMode{0600, 0644} {
		identity := similarityIdentity(fmt.Sprint("damaged ", i))
		token := strings.Repeat(fmt.Sprint(i+2), 32)
		recordProfile(t, store, token, identity, GiB, 1, true, profileZero(), 0)
		path := filepath.Join(g.Metadata, token+".json")
		if i == 0 {
			if e = os.WriteFile(path, []byte("{truncated"), mode); e != nil {
				t.Fatal(e)
			}
		} else {
			mustWrite(t, path, good)
			if e = os.Chmod(path, mode); e != nil {
				t.Fatal(e)
			}
		}
	}
	// A readable file whose token has no run and a corrupt scheduling sidecar.
	mustWrite(t, filepath.Join(g.Metadata, strings.Repeat("4", 32)+".json"), good)
	if e = os.MkdirAll(filepath.Join(g.State, "workloads"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(g.State, "workloads", strings.Repeat("4", 32)+".json"), []byte("{truncated"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = store.backfillSimilarity(g); e != nil {
		t.Fatal("A bad record held the backfill", e)
	}
	if e = store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT version FROM learning_similarity_state WHERE id=1")
		if metric(row["version"]) != profileClassificationVersion {
			t.Fatal("The backfill was not marked complete", row)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if class, ok := storedClass(t, store, healthy); !ok || class.repository != want {
		t.Fatal("The healthy family was not classified", class, ok)
	}
}

func TestProfileInvalidLegacyRowsAreAbsentEverywhere(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	for i, row := range [][2]string{{profileDigest("scope"), "ruby:deploy"}, {"not a scope", "go:tests"}} {
		identity := similarityIdentity(fmt.Sprint("broken ", i))
		if e = store.Observe(identity, Object{}); e != nil {
			t.Fatal(e)
		}
		if e = store.connection(true, func(c profileSQL) error {
			return profileExec(c, "INSERT INTO learning_similarity VALUES (?,?,?)", identity["family"], row[0], row[1])
		}); e != nil {
			t.Fatal(e)
		}
		if e = store.Observe(identity, profileSimilarityDescription(Object{"title": "Lint", "executable": "pnpm", "directory": repository}, map[string]profileIdentity{})); e != nil {
			t.Fatal("An invalid legacy row failed an observation", e)
		}
		if class, ok := storedClass(t, store, identity); !ok || class.tool != "javascript" || class.operation != "lint" {
			t.Fatal("An invalid legacy row decided the class", class)
		}
		if _, e = store.Match(identity); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = LearningRegroup(g, false, nil); e != nil {
		t.Fatal("An invalid legacy row failed the preview", e)
	}
	if e = store.backfillSimilarity(g); e != nil {
		t.Fatal("An invalid legacy row held the backfill", e)
	}
}

func TestProfileRunsOnlyFamilyInTwoDirectoriesIsRegisteredAndClassified(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	repository := gitRepository(t)
	_, want := profileDirectoryIdentity(repository)
	identity := similarityIdentity("runs only")
	for i, directory := range []string{repository, filepath.Join(repository, "sub")} {
		token := fmt.Sprintf("%032x", i+1)
		recordProfile(t, store, token, identity, GiB, 1, true, profileZero(), 0)
		mustWrite(t, filepath.Join(g.Metadata, token+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": directory})
	}
	if _, e = LearningRegroup(g, true, Object{"revision": 0}); e != nil {
		t.Fatal(e)
	}
	if class, ok := storedClass(t, store, identity); !ok || class.repository != want || class.tool != "go" || class.operation != "tests" {
		t.Fatal("A family in two directories was not registered and classified", class, ok)
	}
	if familyHasLegacyRow(t, store, identity) {
		t.Fatal("Two scopes produced a legacy pair")
	}
}

func TestProfileEveryLegacyKindYieldsAValidClassAndInvalidClassesAreRefused(t *testing.T) {
	for _, tool := range profileTools {
		for _, operation := range profileOperations {
			if kind := tool + ":" + operation; validProfileSimilarityKind(kind) {
				if class := profileBackfillClass(Object{"family": "x"}, kind, "", &profileHistory{}); !class.valid() || class.kind() != kind {
					t.Errorf("%s produced %+v", kind, class)
				}
			}
		}
	}
	store := fixtureProfileStore(t)
	if e := store.connection(true, func(c profileSQL) error {
		return profileStoreClass(c, "x", profileClass{tool: "ruby", operation: "tests"})
	}); e == nil {
		t.Fatal("An invalid class was stored")
	}
}

func TestProfileUnknownClassIsReplaceableOnce(t *testing.T) {
	store := fixtureProfileStore(t)
	repository := profileDigest("repository")
	store.connection(true, func(c profileSQL) error {
		for _, step := range []profileClass{{"", "other", "other"}, {"", "go", "tests"}, {repository, "javascript", "lint"}} {
			if e := profileStoreClass(c, "x", step); e != nil {
				t.Fatal(e)
			}
		}
		row, e := profileRow(c, "SELECT * FROM learning_classification WHERE family='x'")
		if class, ok := profileStoredClass(row); !ok || class != (profileClass{repository, "go", "tests"}) {
			t.Fatal("The unknown class was not replaced exactly once, or the repository was not filled", class, e)
		}
		return e
	})
}

func TestProfileOperationVocabularyMatchesDashboardSchema(t *testing.T) {
	source, e := os.ReadFile(filepath.Join("..", "..", "remote", "static", "schema.mjs"))
	if e != nil {
		t.Fatal(e)
	}
	match := regexp.MustCompile(`operationSchema = z\.literal\(\[([^\]]*)\]`).FindSubmatch(source)
	if match == nil {
		t.Fatal("schema.mjs has no operationSchema literal list")
	}
	listed := []string{}
	for _, word := range strings.Split(string(match[1]), ",") {
		listed = append(listed, strings.Trim(strings.TrimSpace(word), `"`))
	}
	if !reflect.DeepEqual(listed, ProfileOperations()) {
		t.Fatalf("schema.mjs operations %v differ from ProfileOperations %v", listed, ProfileOperations())
	}
}

func TestProfileBackfillCountsHistoricalWrappersAsTheirRuntime(t *testing.T) {
	for executable, tool := range map[string]string{"env": "shell", "/usr/bin/env": "shell", "fnm": "javascript", "go": "go", "run.sh": "shell", "mystery": "other"} {
		member := Object{"family": "f", "executable": executable, "title": "Run tests"}
		if class := profileBackfillClass(member, "", "", &profileHistory{}); class.tool != tool || class.operation != "tests" {
			t.Fatalf("%s classified as %+v, want %s/tests", executable, class, tool)
		}
	}
	// A new job shows what the wrapper really ran.
	if tool, _ := profileClassify([]string{"env", "FOO=1", "mystery"}, "Run tests", "env"); tool != "other" {
		t.Fatalf("observation of a wrapped unknown tool got %s", tool)
	}
}

func TestProfileMetadataWithOnlyADirectoryPassesValidation(t *testing.T) {
	clean := profileSimilarityDescription(Object{"directory": gitRepository(t)}, map[string]profileIdentity{})
	if e := profileValidateSimilarityDescription(Object{}, clean); e != nil {
		t.Fatal("Metadata origin accepted would hold every tick:", e)
	}
}

func TestProfileObservationOfOldMetadataMatchesTheBackfill(t *testing.T) {
	clean := profileSimilarityDescription(Object{"title": "Run tests", "executable": "env"}, map[string]profileIdentity{})
	if clean["learningTool"] != "shell" || clean["learningOperation"] != "tests" {
		t.Fatal("An old wrapper job classifies differently when a tick sees it first", clean)
	}
}

func TestProfileAggregatePrefersDescribedMetadataOverBlankFields(t *testing.T) {
	described := Object{"title": "Go tests", "project": "fixture", "executable": "go"}
	for _, evidence := range [][]profileEvidence{{{family: "f", clean: described}, {family: "f", clean: Object{}}}, {{family: "f", clean: Object{}}, {family: "f", clean: described}}} {
		if got := profileAggregate(evidence, nil).descriptions["f"]; got["title"] != "Go tests" || got["executable"] != "go" {
			t.Fatal("A blank file registered the family", got)
		}
	}
	// A sparse file loses even when its first fields sort earlier.
	for _, sparse := range []Object{{"project": "a"}, {"project": "fixture", "title": "A"}, {"title": "A"}} {
		evidence := []profileEvidence{{family: "f", clean: sparse}, {family: "f", clean: described}}
		if got := profileAggregate(evidence, nil).descriptions["f"]; got["executable"] != "go" {
			t.Fatal("A sparse file registered the family", sparse, got)
		}
	}
}

func TestProfileTwoRepositoriesNeverFallBackToAnAlias(t *testing.T) {
	scope, repository := strings.Repeat("a", 64), strings.Repeat("b", 64)
	history := &profileHistory{repositories: map[string]string{}, aliases: map[string]string{scope: repository}, descriptions: map[string]Object{"f": {"learningScope": scope}}}
	if class := profileBackfillClass(Object{"family": "f"}, "", "", history); class.repository != repository {
		t.Fatal("Fixture: an unverified family should use its alias", class)
	}
	history.repositories["f"] = ""
	if class := profileBackfillClass(Object{"family": "f"}, "", "", history); class.repository != "" {
		t.Fatal("A family verified in two repositories took an alias", class)
	}
}

func TestProfileBackfillSkipsAFamilyWhenTheRegistryIsFull(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.connection(true, func(c profileSQL) error {
		for i := 0; i < maxRegisteredFamilies; i++ {
			family := profileDigest([]any{"filler", i})
			if e := profileExec(c, "INSERT INTO learning_profiles VALUES (?,NULL,NULL,NULL,1,0,?)", family, 1e12); e != nil {
				return e
			}
			if e := profileExec(c, "INSERT INTO learning_members VALUES (?,?,NULL,NULL,NULL,?)", family, family, 1e12); e != nil {
				return e
			}
		}
		return profileExec(c, "UPDATE learning_profiles SET pinned=1")
	}); e != nil {
		t.Fatal(e)
	}
	identity := similarityIdentity("no room")
	token := fmt.Sprintf("%032x", 1)
	recordProfile(t, store, token, identity, GiB, 1, true, profileZero(), 0)
	mustWrite(t, filepath.Join(g.Metadata, token+".json"), Object{"title": "Go tests", "project": "fixture", "executable": "go", "directory": gitRepository(t)})
	if e = store.backfillSimilarity(g); e != nil {
		t.Fatal("A full registry stopped the backfill:", e)
	}
	if e = store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT version FROM learning_similarity_state WHERE id=1")
		if e == nil && metric(row["version"]) != profileClassificationVersion {
			t.Error("The pass was not marked complete", row)
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if familyHasLegacyRow(t, store, identity) {
		t.Fatal("A family with no registry slot gained a legacy row")
	}
	if e = store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT 1 FROM learning_members WHERE family=?", identity["family"])
		if e == nil && row != nil {
			t.Error("A family was registered past the cap")
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestControllerForgetsScopeAnswersAfterTheirLifetime(t *testing.T) {
	c := &Controller{}
	c.scopes()["checkout"] = profileIdentity{scope: "s"}
	if _, kept := c.scopes()["checkout"]; !kept {
		t.Fatal("A fresh answer was dropped")
	}
	c.scopesChecked = time.Now().Add(-learningScopeLifetime - time.Second)
	if _, kept := c.scopes()["checkout"]; kept {
		t.Fatal("A failed git probe would last for the controller's lifetime")
	}
}

func TestFailedHistoryPassDoesNotHoldAdmissions(t *testing.T) {
	g, _ := schedulerFixtureGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	// A vocabulary this build does not know fails the catalog read the pass needs.
	family := profileDigest([]any{"future"})
	if e = store.connection(true, func(c profileSQL) error {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO learning_profiles VALUES (?,NULL,NULL,NULL,1,0,?)", []any{family, 1e12}},
			{"INSERT INTO learning_members VALUES (?,?,NULL,NULL,NULL,?)", []any{family, family, 1e12}},
			{"INSERT INTO learning_classification VALUES (?,NULL,'future','other')", []any{family}},
			{"UPDATE learning_similarity_state SET version=1 WHERE id=1", nil},
		} {
			if e := profileExec(c, statement.query, statement.args...); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	sampler := NewResourceSampler(g.State, &admissionFixtureBackend{pressure: "normal", processCount: map[int]uint64{}})
	if e = serveScheduler(g, sampler, true); e != nil {
		t.Fatal("A failed history pass stopped the controller:", e)
	}
	status, e := ReadPrivate(filepath.Join(g.State, "resource-status.json"))
	if e != nil || status["host"] == nil || status["reason"] == "Resource collection failed; admissions held" {
		t.Fatalf("A failed history pass held admissions: %v %v", status, e)
	}
	if e = store.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT version FROM learning_similarity_state WHERE id=1")
		if e == nil && metric(row["version"]) >= profileClassificationVersion {
			t.Error("Fixture: the pass should have failed, not completed")
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
}

func TestHistoryPassWaitsBeforeRetrying(t *testing.T) {
	g := testGate(t)
	store, e := NewProfileStore(g.State, false)
	if e != nil {
		t.Fatal(e)
	}
	family := profileDigest([]any{"future"})
	if e = store.connection(true, func(c profileSQL) error {
		if e := profileExec(c, "INSERT INTO learning_profiles VALUES (?,NULL,NULL,NULL,1,0,?)", family, 1e12); e != nil {
			return e
		}
		if e := profileExec(c, "INSERT INTO learning_members VALUES (?,?,NULL,NULL,NULL,?)", family, family, 1e12); e != nil {
			return e
		}
		if e := profileExec(c, "INSERT INTO learning_classification VALUES (?,NULL,'future','other')", family); e != nil {
			return e
		}
		return profileExec(c, "UPDATE learning_similarity_state SET version=1 WHERE id=1")
	}); e != nil {
		t.Fatal(e)
	}
	c := &Controller{store: store}
	start := time.Now()
	if c.historyPass(g, start) {
		t.Fatal("Fixture: the pass should fail")
	}
	retry := c.retryHistory
	if !retry.Equal(start.Add(learningScopeLifetime)) {
		t.Fatal("A failed pass set no retry time", retry)
	}
	if c.historyPass(g, start.Add(time.Minute)) || !c.retryHistory.Equal(retry) {
		t.Fatal("The pass ran again before its retry time")
	}
	if e = store.connection(true, func(c profileSQL) error {
		return profileExec(c, "DELETE FROM learning_classification WHERE family=?", family)
	}); e != nil {
		t.Fatal(e)
	}
	if c.historyPass(g, start.Add(time.Minute)) {
		t.Fatal("A repaired store ran before its retry time")
	}
	if !c.historyPass(g, retry) {
		t.Fatal("The pass did not retry at its retry time")
	}
}
