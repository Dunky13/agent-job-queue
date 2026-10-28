package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	query := "SELECT m.* FROM learning_members m"
	if has {
		query = "SELECT m.*,s.scope AS learningScope,s.kind AS learningKind FROM learning_members m LEFT JOIN learning_similarity s ON s.family=m.family"
	}
	return profileRows(c, query+where, args...)
}

func profileMember(c profileSQL, family string) (Object, error) {
	rows, e := profileMemberRows(c, " WHERE m.family=?", family)
	if e != nil || len(rows) == 0 {
		return nil, e
	}
	return rows[0], nil
}

func profileDirectoryScope(directory string) string {
	if directory == "" || !filepath.IsAbs(directory) {
		return ""
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
				return profileDigest([]any{"learning-scope-v1", "git", dev, ino, relative})
			}
		}
	}
	// Deleted checkouts can still share evidence within their original directory.
	// Never infer repository identity from a project basename or a path pattern.
	if resolved, e := filepath.EvalSymlinks(directory); e == nil {
		directory = resolved
	}
	return profileDigest([]any{"learning-scope-v1", "directory", filepath.Clean(directory)})
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
	return tool + ":" + operations[0]
}

func validProfileSimilarityKind(kind string) bool {
	tool, operation, ok := strings.Cut(kind, ":")
	return ok && containsString([]string{"python", "shell", "javascript", "go", "cargo", "swift", "make", "gmake"}, tool) &&
		containsString([]string{"tests", "browser-tests", "native-tests", "race-tests", "full-tests", "build", "lint", "typecheck", "dependencies", "benchmark", "checks"}, operation)
}

func profileCommandKind(argv []string, title, executable string) string {
	// Recognize simple wrappers without interpreting or executing shell programs.
	command := argv
	for len(command) > 0 {
		switch filepath.Base(command[0]) {
		case "env":
			command = command[1:]
			for len(command) > 0 && (command[0] == "--" || (!strings.HasPrefix(command[0], "-") && strings.Contains(command[0], "="))) {
				command = command[1:]
			}
		case "fnm":
			if len(command) < 2 || command[1] != "exec" {
				return profileSimilarityKind(title, executable)
			}
			separator := -1
			for i, arg := range command[2:] {
				if arg == "--" {
					separator = i + 2
					break
				}
			}
			if separator < 0 {
				return profileSimilarityKind(title, executable)
			}
			command = command[separator+1:]
		default:
			goto unwrapped
		}
	}
unwrapped:
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
	scope, kind := text(clean["learningScope"]), text(clean["learningKind"])
	if scope == "" || kind == "" {
		return false, nil
	}
	// Once registered, a label change cannot reclassify measured history.
	if e := profileExec(c, "INSERT OR IGNORE INTO learning_similarity VALUES (?,?,?)", family, scope, kind); e != nil {
		return false, e
	}
	stored, e := profileRow(c, "SELECT scope,kind FROM learning_similarity WHERE family=?", family)
	if e != nil {
		return false, e
	}
	return profileGroupSimilar(c, stored)
}

// profileGroupSimilar is deterministic, idempotent and transactional. Pinned
// profiles (including explicit splits and resets) are never automatic targets.
func profileGroupSimilar(c profileSQL, signature ...Object) (bool, error) {
	query := `WITH eligible AS (SELECT m.profile,s.scope,s.kind FROM learning_members m JOIN learning_profiles p ON p.id=m.profile JOIN learning_similarity s ON s.family=m.family WHERE p.pinned=0`
	args := []any{}
	if len(signature) > 0 {
		query += " AND s.scope=? AND s.kind=?"
		args = append(args, signature[0]["scope"], signature[0]["kind"])
	}
	// Prove that every member of a profile has this signature once per profile,
	// rather than rescanning a large shared profile for each individual member.
	query += ` GROUP BY m.profile,s.scope,s.kind HAVING COUNT(*)=(SELECT COUNT(*) FROM learning_members all_members WHERE all_members.profile=m.profile)) SELECT m.family,m.profile,m.project,e.scope,e.kind FROM eligible e JOIN learning_members m ON m.profile=e.profile ORDER BY m.profile,m.family`
	rows, e := profileRows(c, query, args...)
	if e != nil {
		return false, e
	}
	groups := map[string][]Object{}
	for _, row := range rows {
		if !profileHash.MatchString(text(row["scope"])) || !validProfileSimilarityKind(text(row["kind"])) {
			return false, fmt.Errorf("Invalid stored workload similarity")
		}
		key := text(row["scope"]) + ":" + text(row["kind"])
		groups[key] = append(groups[key], row)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := false
	for _, key := range keys {
		members := groups[key]
		if len(members) < 2 {
			continue
		}
		target := text(members[0]["profile"])
		groupChanged := false
		for _, member := range members[1:] {
			source := text(member["profile"])
			if source == target {
				continue
			}
			if e = profileExec(c, "UPDATE learning_members SET profile=? WHERE family=?", target, member["family"]); e != nil {
				return false, e
			}
			// Do not delete an automatic profile while it still owns another family.
			if e = profileExec(c, "DELETE FROM learning_profiles WHERE id=? AND NOT EXISTS(SELECT 1 FROM learning_members WHERE profile=?)", source, source); e != nil {
				return false, e
			}
			changed = true
			groupChanged = true
		}
		if groupChanged {
			label := profileSimilarityLabel(text(members[0]["kind"]), text(members[0]["project"]))
			if e = profileExec(c, "UPDATE learning_profiles SET label=?,updated=? WHERE id=? AND pinned=0", label, resourceNow(), target); e != nil {
				return false, e
			}
		}
	}
	return changed, nil
}

func profileSimilarityDescription(description Object, scopes map[string]string) Object {
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
	if kind == "" {
		return result
	}
	scope := text(description["learningScope"])
	if !profileHash.MatchString(scope) {
		directory := text(description["directory"])
		cacheKey := directory
		if info, e := os.Stat(directory); e == nil {
			dev, ino := profileStatIdentity(info)
			cacheKey = profileDigest([]any{directory, dev, ino})
		}
		var ok bool
		scope, ok = scopes[cacheKey]
		if !ok {
			scope = profileDirectoryScope(directory)
			scopes[cacheKey] = scope
		}
	}
	if scope != "" {
		result["learningScope"], result["learningKind"] = scope, kind
	}
	return result
}

func profileValidateSimilarityDescription(clean, description Object) error {
	scope, kind := text(description["learningScope"]), text(description["learningKind"])
	if scope == "" && kind == "" {
		return nil
	}
	if !profileHash.MatchString(scope) || !validProfileSimilarityKind(kind) {
		return fmt.Errorf("Invalid workload similarity description")
	}
	clean["learningScope"], clean["learningKind"] = scope, kind
	return nil
}
