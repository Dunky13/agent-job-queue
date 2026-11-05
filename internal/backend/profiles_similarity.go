package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Similarity only shares upper envelopes. It never aliases execution identities
// or promotes another command's runs to exact-variant successful observations.
const profileSimilaritySchema = `CREATE TABLE IF NOT EXISTS learning_similarity (family TEXT PRIMARY KEY,scope TEXT NOT NULL,kind TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS learning_similarity_key ON learning_similarity(scope,kind);
CREATE TABLE IF NOT EXISTS learning_similarity_state (id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL);
INSERT OR IGNORE INTO learning_similarity_state VALUES (1,0);`

func hasProfileSimilarity(c profileSQL) (bool, error) {
	row, e := profileRow(c, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='learning_similarity'")
	return row != nil, e
}

func profileMemberRows(c profileSQL, where string, args ...any) ([]Object, error) {
	has, e := hasProfileSimilarity(c)
	if e != nil {
		return nil, e
	}
	classified, e := hasProfileClassification(c)
	if e != nil {
		return nil, e
	}
	columns, joins := "m.*", ""
	if has {
		columns += ",s.scope AS learningScope,s.kind AS learningKind"
		joins += " LEFT JOIN learning_similarity s ON s.family=m.family"
	}
	if classified {
		columns += ",k.repository AS learningRepository,k.tool AS learningTool,k.operation AS learningOperation"
		joins += " LEFT JOIN learning_classification k ON k.family=m.family"
	}
	return profileRows(c, "SELECT "+columns+" FROM learning_members m"+joins+where, args...)
}

func profileMember(c profileSQL, family string) (Object, error) {
	rows, e := profileMemberRows(c, " WHERE m.family=?", family)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	return rows[0], nil
}

func profileDirectoryScope(directory string) string {
	scope, _ := profileDirectoryIdentity(directory)
	return scope
}

// profileDirectoryIdentity returns the v1 scope (repository plus relative
// directory) and the repository digest alone. The repository is empty unless
// git verifies it; a basename or path pattern never establishes one.
func profileDirectoryIdentity(directory string) (string, string) {
	if directory == "" || !filepath.IsAbs(directory) {
		return "", ""
	}
	location := strings.Split(profileGit(directory, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir"), "\n")
	if len(location) == 2 {
		root, err := filepath.EvalSymlinks(location[0])
		common, commonError := filepath.EvalSymlinks(location[1])
		resolved, directoryError := filepath.EvalSymlinks(directory)
		if err == nil && commonError == nil && directoryError == nil {
			info, err := os.Stat(common)
			relative, inside := profileUnder(resolved, root)
			if err == nil && inside {
				dev, ino := profileStatIdentity(info)
				return profileDigest([]any{"learning-scope-v1", "git", dev, ino, relative}), profileDigest([]any{"learning-repository-v1", dev, ino})
			}
		}
	}
	return profileMissingDirectoryIdentity(directory)
}

// Deleted checkouts can still share evidence within their original directory.
// Never infer repository identity from a project basename or a path pattern.
func profileMissingDirectoryIdentity(directory string) (string, string) {
	if resolved, e := filepath.EvalSymlinks(directory); e == nil {
		directory = resolved
	}
	return profileDigest([]any{"learning-scope-v1", "directory", filepath.Clean(directory)}), ""
}

func profileToolClass(executable string) string {
	name := filepath.Base(executable)
	switch {
	case pythonInterpreter.MatchString(name) || name == "pytest" || name == "uv":
		return "python"
	case containsString([]string{"sh", "bash", "zsh", "dash", "fish", "ksh"}, name):
		return "shell"
	case containsString([]string{"npm", "pnpm", "pnpx", "npx", "yarn", "node", "nodejs", "bun", "deno", "fnm"}, name):
		return "javascript"
	case containsString([]string{"go", "cargo", "swift", "make", "gmake"}, name):
		return name
	}
	return ""
}

// Only recognize explicit operation words. Issue numbers, branch names and
// feature names do not become grouping keys, and ambiguous batches stay alone.
func profileSimilarityKind(title, executable string) string {
	tool := profileToolClass(executable)
	if tool == "" {
		return ""
	}
	if operation := profileOperation(title); operation != "" {
		return tool + ":" + operation
	}
	return ""
}

// profileOperation returns the one operation a title names, or "" when it names
// none or several. It needs no recognized tool.
func profileOperation(title string) string {
	words := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		words[word] = true
	}
	has := func(values ...string) bool {
		for _, value := range values {
			if words[value] {
				return true
			}
		}
		return false
	}
	operations := []string{}
	if has("test", "tests", "regression", "regressions", "suite", "suites", "pytest", "vitest") {
		operation := "tests"
		switch {
		case has("browser", "playwright", "chromium", "webkit", "firefox", "e2e"):
			operation = "browser-tests"
		case has("native", "swift"):
			operation = "native-tests"
		case has("race"):
			operation = "race-tests"
		case has("full", "complete", "all", "integration"):
			operation = "full-tests"
		}
		operations = append(operations, operation)
	}
	for _, entry := range []struct {
		words []string
		kind  string
	}{
		{[]string{"build", "builds", "compile", "compilation"}, "build"},
		{[]string{"lint", "linting", "shellcheck", "vet"}, "lint"},
		{[]string{"typecheck", "typechecking", "tsc", "tsgo"}, "typecheck"},
		{[]string{"dependencies", "dependency", "install", "bootstrap"}, "dependencies"},
		{[]string{"benchmark", "benchmarks"}, "benchmark"},
		{[]string{"check", "checks", "validation", "verification", "verify", "audit"}, "checks"},
	} {
		if has(entry.words...) {
			operations = append(operations, entry.kind)
		}
	}
	// A mixed validation command must not borrow a focused test envelope.
	// Generic check words qualify a named operation, rather than making a test
	// labelled "regression checks" a mixed-purpose batch.
	if len(operations) > 1 && operations[len(operations)-1] == "checks" {
		operations = operations[:len(operations)-1]
	}
	if len(operations) != 1 {
		return ""
	}
	return operations[0]
}

func validProfileSimilarityKind(kind string) bool {
	tool, operation, ok := strings.Cut(kind, ":")
	return ok && containsString([]string{"python", "shell", "javascript", "go", "cargo", "swift", "make", "gmake"}, tool) &&
		containsString([]string{"tests", "browser-tests", "native-tests", "race-tests", "full-tests", "build", "lint", "typecheck", "dependencies", "benchmark", "checks"}, operation)
}

func profileCommandKind(argv []string, title, executable string) string {
	// Recognize simple wrappers without interpreting or executing shell programs.
	command, ok := profileUnwrap(argv)
	if !ok {
		return profileSimilarityKind(title, executable)
	}
	if len(command) > 0 {
		if filepath.Base(command[0]) == "cargo" && len(command) > 1 && command[1] == "bench" {
			return "cargo:benchmark"
		}
		if filepath.Base(command[0]) == "go" && len(command) > 1 && command[1] == "test" {
			for _, arg := range command[2:] {
				if arg == "-bench" || strings.HasPrefix(arg, "-bench=") {
					return "go:benchmark"
				}
			}
		}
		summary, tool := SafeSummary(command)
		if kind := profileSimilarityKind(summary, tool); kind != "" {
			// Retain broad, browser and instrumented test categories for transfer.
			if strings.HasSuffix(kind, ":tests") {
				for _, arg := range command[1:] {
					if arg == "-race" {
						return strings.TrimSuffix(kind, "tests") + "race-tests"
					}
				}
				filtered := false
				for _, arg := range command[1:] {
					filtered = filtered || arg == "-run" || strings.HasPrefix(arg, "-run=")
				}
				if containsString(command, "./...") && !filtered {
					return strings.TrimSuffix(kind, "tests") + "full-tests"
				}
			}
			return kind
		}
	}
	return profileSimilarityKind(title, executable)
}

func profileSimilarityLabel(kind, project string) string {
	tool, operation, _ := strings.Cut(kind, ":")
	label := strings.ReplaceAll(tool+" "+operation, "-", " ")
	if project != "" {
		label = project + " · " + label
	}
	return DisplayText(label, 120)
}

func profileRememberSimilarity(c profileSQL, family string, clean Object) (bool, error) {
	class := profileClass{text(clean["learningRepository"]), text(clean["learningTool"]), text(clean["learningOperation"])}
	// A family that already has a legacy kind keeps it: the controller observes
	// before it backfills, and a job description no longer carries that kind.
	if has, e := hasProfileSimilarity(c); e != nil {
		return false, e
	} else if has {
		row, e := profileRow(c, "SELECT scope,kind FROM learning_similarity WHERE family=?", family)
		if e != nil {
			return false, e
		}
		if legacy := profileValidLegacy(row); legacy != nil {
			class.tool, class.operation, _ = strings.Cut(text(legacy["kind"]), ":")
		}
	}
	// Once registered, a label change cannot reclassify measured history.
	if scope, kind := text(clean["learningScope"]), text(clean["learningKind"]); scope != "" && kind != "" {
		if e := profileExec(c, "INSERT OR IGNORE INTO learning_similarity VALUES (?,?,?)", family, scope, kind); e != nil {
			return false, e
		}
	}
	// A description with nothing in it cannot be classified yet. Backfill or a
	// later observation will, rather than storing a lasting other/other.
	if clean["learningTool"] == nil {
		return false, nil
	}
	if e := profileStoreClass(c, family, class); e != nil {
		return false, e
	}
	row, e := profileRow(c, "SELECT repository,tool,operation FROM learning_classification WHERE family=?", family)
	if e != nil {
		return false, e
	}
	stored, ok := profileStoredClass(row)
	if !ok || !stored.shareable() {
		return false, nil
	}
	return profileGroupSimilar(c, &stored)
}

// profileDescriptionIdentity computes the directory's v1 scope and repository
// once per checkout, keyed by device and inode so renames do not go stale.
func profileDescriptionIdentity(directory string, identities map[string]profileIdentity) profileIdentity {
	cacheKey := directory
	info, statError := os.Stat(directory)
	if statError == nil {
		dev, ino := profileStatIdentity(info)
		cacheKey = profileDigest([]any{directory, dev, ino})
	}
	identity, ok := identities[cacheKey]
	if !ok {
		if statError != nil && filepath.IsAbs(directory) {
			// Nothing to verify, so do not spend a git process on it.
			identity.scope, identity.repository = profileMissingDirectoryIdentity(directory)
		} else {
			identity.scope, identity.repository = profileDirectoryIdentity(directory)
		}
		identities[cacheKey] = identity
	}
	return identity
}

func profileSimilarityDescription(description Object, identities map[string]profileIdentity) Object {
	result := Object{}
	for _, key := range []string{"title", "project", "executable"} {
		limit := 80
		if key == "title" {
			limit = 120
		}
		if value := DisplayText(text(description[key]), limit); value != "" {
			result[key] = value
		}
	}
	kind := text(description["learningKind"])
	if !validProfileSimilarityKind(kind) {
		kind = profileSimilarityKind(text(result["title"]), text(result["executable"]))
	}
	identity := profileDescriptionIdentity(text(description["directory"]), identities)
	if scope := text(description["learningScope"]); profileHash.MatchString(scope) {
		identity.scope = scope
	}
	if repository := text(description["learningRepository"]); profileHash.MatchString(repository) {
		identity.repository = repository
	}
	if identity.scope != "" && kind != "" {
		result["learningScope"], result["learningKind"] = identity.scope, kind
	}
	// New jobs carry the tool and operation seen at submission, where the
	// arguments were available. Older metadata falls back to what survives.
	tool, operation := text(description["learningTool"]), text(description["learningOperation"])
	if !containsString(profileTools, tool) || !containsString(profileOperations, operation) {
		if result["title"] == nil && result["project"] == nil && result["executable"] == nil {
			return result // Nothing survives to classify.
		}
		tool, operation, _ = strings.Cut(kind, ":")
		if tool == "" {
			tool = profileHistoricalTool(text(result["executable"]))
		}
		if operation == "" {
			operation = profileOperation(text(result["title"]))
		}
		tool, operation = profileDefault(tool), profileDefault(operation)
	}
	result["learningTool"], result["learningOperation"] = tool, operation
	if identity.repository != "" {
		result["learningRepository"] = identity.repository
	}
	return result
}

func profileValidateSimilarityDescription(clean, description Object) error {
	scope, kind := text(description["learningScope"]), text(description["learningKind"])
	if scope != "" || kind != "" {
		if !profileHash.MatchString(scope) || !validProfileSimilarityKind(kind) {
			return fmt.Errorf("Invalid workload similarity description")
		}
		clean["learningScope"], clean["learningKind"] = scope, kind
	}
	repository, tool, operation := text(description["learningRepository"]), text(description["learningTool"]), text(description["learningOperation"])
	if repository == "" && tool == "" && operation == "" {
		return nil
	}
	if !(profileClass{repository, tool, operation}).valid() {
		return fmt.Errorf("Invalid workload classification description")
	}
	if repository != "" {
		clean["learningRepository"] = repository
	}
	clean["learningTool"], clean["learningOperation"] = tool, operation
	return nil
}
