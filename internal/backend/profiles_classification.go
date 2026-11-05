package backend

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The classification lives beside learning_similarity rather than in it: old
// clients insert three values into that table and reject any kind they do not
// know, and they share this database. Nothing here is written to it.
const profileClassificationSchema = `CREATE TABLE IF NOT EXISTS learning_classification (family TEXT PRIMARY KEY,repository TEXT,tool TEXT NOT NULL,operation TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS learning_classification_key ON learning_classification(repository,operation,tool);`

// The similarity version that marks the repository-wide classification backfill.
const profileClassificationVersion = 2

var profileOperations = []string{"tests", "browser-tests", "native-tests", "race-tests", "full-tests", "build", "lint", "typecheck", "dependencies", "benchmark", "checks", "other"}

// ProfileOperations is the fixed operation vocabulary, for adapters that
// validate it without opening the database.
func ProfileOperations() []string { return append([]string{}, profileOperations...) }

var profileTools = []string{"python", "shell", "javascript", "go", "cargo", "swift", "make", "gmake", "other"}

// A class is shared only when it is verified (repository) and recognized (tool).
// Everything else is stored for browsing only.
type profileClass struct{ repository, tool, operation string }

func (k profileClass) valid() bool {
	return (k.repository == "" || profileHash.MatchString(k.repository)) && containsString(profileTools, k.tool) && containsString(profileOperations, k.operation)
}

func (k profileClass) shareable() bool {
	return k.repository != "" && k.tool != "other"
}

func (k profileClass) kind() string { return k.tool + ":" + k.operation }

func hasProfileClassification(c profileSQL) (bool, error) {
	row, e := profileRow(c, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='learning_classification'")
	return row != nil, e
}

// profileToolOf extends the legacy tool vocabulary for classification only, so
// the kinds written for old clients keep their meaning.
// A wrapper is never a tool itself: what it runs decides.
func profileToolOf(executable string) string {
	if profileWrapper(executable) {
		return ""
	}
	if tool := profileToolClass(executable); tool != "" {
		return tool
	}
	if strings.HasSuffix(filepath.Base(executable), ".sh") {
		return "shell"
	}
	return ""
}

func profileWrapper(executable string) bool {
	return containsString([]string{"env", "fnm"}, filepath.Base(executable))
}

// profileUnwrap strips simple env and fnm wrappers without interpreting shell
// programs. It reports false for an fnm invocation it cannot unwrap.
func profileUnwrap(argv []string) ([]string, bool) {
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
				return command, false
			}
			separator := -1
			for i, arg := range command[2:] {
				if arg == "--" {
					separator = i + 2
					break
				}
			}
			if separator < 0 {
				return command, false
			}
			command = command[separator+1:]
		default:
			return command, true
		}
	}
	return command, true
}

const profileInlineLimit = 8192

var profileInlineSeparators = regexp.MustCompile(`[;|&\n(){}]`)
var profileInlineAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// profileInlineOperation reads the category of a `sh -c` program and keeps
// nothing else. It splits on shell separators without honoring quotes, which
// can only misclassify a command, never expose it. Mixed programs return "".
func profileInlineOperation(command []string) string {
	if len(command) < 3 || profileToolClass(command[0]) != "shell" {
		return ""
	}
	program := ""
	for i := 1; i < len(command)-1; i++ {
		arg := command[i]
		if strings.HasPrefix(arg, "--") {
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return "" // A script path, not an inline program.
		}
		if strings.Contains(arg, "c") {
			program = command[i+1]
			break
		}
		if strings.HasSuffix(arg, "o") {
			i++ // -o and clusters like -euo take an option name.
		}
	}
	program = program[:min(len(program), profileInlineLimit)]
	operation := ""
	for _, segment := range profileInlineSeparators.Split(program, -1) {
		tokens := []string{}
		for _, token := range strings.Fields(segment) {
			tokens = append(tokens, strings.Trim(token, `'"`))
		}
		for len(tokens) > 0 && (containsString([]string{"do", "then", "else", "if", "while", "until", "time", "exec", "!"}, tokens[0]) || profileInlineAssignment.MatchString(tokens[0])) {
			tokens = tokens[1:]
		}
		// The builtin test mentions "test" without testing anything.
		if len(tokens) == 0 || tokens[0] == "test" {
			continue
		}
		found := ""
		if kind := profileCommandKind(tokens, "", filepath.Base(tokens[0])); kind != "" {
			_, found, _ = strings.Cut(kind, ":")
		} else if unwrapped, ok := profileUnwrap(tokens); ok {
			summary, _ := SafeSummary(unwrapped)
			found = profileOperation(summary)
		}
		if found == "" {
			continue
		}
		if operation != "" && operation != found {
			return ""
		}
		operation = found
	}
	return operation
}

// profileClassify returns the tool and operation of a new job. Sources in
// order: the legacy kind (which already covers the safe summary and wrappers),
// the summary and title for tools the legacy vocabulary skips, then the inline
// program. Anything mixed or unknown is "other".
func profileClassify(argv []string, title, executable string) (string, string) {
	tool, operation := "", ""
	command, ok := profileUnwrap(argv)
	if profileWrapper(executable) {
		// The wrapper's name must not stand in for the tool it runs.
		executable = ""
		if ok && len(command) > 0 {
			executable = command[0]
		}
	}
	if kind := profileCommandKind(argv, title, executable); kind != "" {
		tool, operation, _ = strings.Cut(kind, ":")
	} else {
		if len(command) > 0 {
			tool = profileToolOf(command[0])
		}
		if tool == "" {
			tool = profileToolOf(executable)
		}
		summary, _ := SafeSummary(command)
		if operation = profileOperation(summary); operation == "" {
			operation = profileOperation(title)
		}
		if operation == "" {
			operation = profileInlineOperation(command)
		}
	}
	return profileDefault(tool), profileDefault(operation)
}

func profileDefault(value string) string {
	if value == "" {
		return "other"
	}
	return value
}

// A repository digest never includes the relative directory.
type profileIdentity struct{ scope, repository string }

// profileHistory is what the retroactive pass learns from retained metadata.
type profileHistory struct {
	// descriptions holds what registering a family that only has runs needs. A
	// learning scope and kind appear only when every file of the family agrees,
	// because they become the pair old clients read from learning_similarity.
	descriptions map[string]Object
	// aliases maps a v1 scope to the one repository it belongs to. An empty
	// value marks a scope seen in two repositories, which is ambiguous.
	aliases map[string]string
	// repositories maps a family to the one repository its live checkouts
	// verify, or "" when they verify two. A family with no entry verified none.
	repositories map[string]string
}

func profileAddAlias(aliases map[string]string, scope, repository string) {
	if scope == "" || repository == "" {
		return
	}
	if prior, seen := aliases[scope]; seen && prior != repository {
		repository = ""
	}
	aliases[scope] = repository
}

// profileStoredClass reads a stored class. A row that is missing or that the
// vocabulary no longer accepts is unclassified, so it never merges.
func profileStoredClass(row Object) (profileClass, bool) {
	if row == nil || row["tool"] == nil {
		return profileClass{}, false
	}
	class := profileClass{text(row["repository"]), text(row["tool"]), text(row["operation"])}
	return class, class.valid()
}

// profileValidLegacy returns a learning_similarity row old clients wrote, or nil
// when it is absent or holds a scope or kind this version cannot read.
func profileValidLegacy(row Object) Object {
	if row == nil || !profileHash.MatchString(text(row["scope"])) || !validProfileSimilarityKind(text(row["kind"])) {
		return nil
	}
	return row
}

// improvedBy fills what a stored class lacks: a repository, or a tool and
// operation that were both unknown.
func (k profileClass) improvedBy(known profileClass) profileClass {
	if k.tool == "other" && k.operation == "other" {
		k.tool, k.operation = known.tool, known.operation
	}
	if k.repository == "" {
		k.repository = known.repository
	}
	return k
}

// profileBackfillClass classifies a registered family. Tool and operation come
// from the member alone, so file order and metadata cannot change them: the
// legacy kind, then the stored executable and title. The repository is the one
// the family's checkouts verify, else the one its legacy scope belongs to.
func profileBackfillClass(member Object, kind, scope string, history *profileHistory) profileClass {
	family := text(member["family"])
	tool, operation, ok := strings.Cut(kind, ":")
	if !ok {
		tool, operation = profileHistoricalTool(text(member["executable"])), profileOperation(text(member["title"]))
	}
	class := profileClass{tool: profileDefault(tool), operation: profileDefault(operation)}
	if repository, verified := history.repositories[family]; verified {
		class.repository = repository
	} else {
		if scope == "" {
			scope = text(history.descriptions[family]["learningScope"])
		}
		class.repository = history.aliases[scope]
	}
	return class
}

// shortcut: history keeps no command line, so a wrapper counts as its usual runtime and a job it ran under another tool is grouped by that runtime, upgrade if retained metadata ever records the wrapped tool.
func profileHistoricalTool(executable string) string {
	switch filepath.Base(executable) {
	case "env":
		return "shell"
	case "fnm":
		return "javascript"
	}
	return profileToolOf(executable)
}

// profileBackfillClasses returns the class every listed family has or would
// receive. An existing class wins; a missing repository may be filled in.
// Preview and apply both call this before apply writes any legacy row.
func profileBackfillClasses(c profileSQL, history *profileHistory, members []Object) (map[string]profileClass, error) {
	stored, legacy := map[string]Object{}, map[string]Object{}
	if has, e := hasProfileClassification(c); e != nil {
		return nil, e
	} else if has {
		rows, e := profileRows(c, "SELECT * FROM learning_classification")
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			stored[text(row["family"])] = row
		}
	}
	if has, e := hasProfileSimilarity(c); e != nil {
		return nil, e
	} else if has {
		rows, e := profileRows(c, "SELECT * FROM learning_similarity")
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			legacy[text(row["family"])] = profileValidLegacy(row)
		}
	}
	result := map[string]profileClass{}
	for _, member := range members {
		family := text(member["family"])
		old := legacy[family]
		class := profileBackfillClass(member, text(old["kind"]), text(old["scope"]), history)
		if prior, ok := profileStoredClass(stored[family]); ok {
			class = prior.improvedBy(class)
		}
		result[family] = class
	}
	return result, nil
}

// profileStoreClass keeps what is stored except for the two fills improvedBy
// describes, so the SQL below must change with it.
func profileStoreClass(c profileSQL, family string, class profileClass) error {
	if !class.valid() {
		return fmt.Errorf("Invalid workload classification")
	}
	var repository any
	if class.repository != "" {
		repository = class.repository
	}
	if e := profileExec(c, "INSERT OR IGNORE INTO learning_classification VALUES (?,?,?,?)", family, repository, class.tool, class.operation); e != nil {
		return e
	}
	if class.tool != "other" || class.operation != "other" {
		if e := profileExec(c, "UPDATE learning_classification SET tool=?,operation=? WHERE family=? AND tool='other' AND operation='other'", class.tool, class.operation, family); e != nil {
			return e
		}
	}
	if repository == nil {
		return nil
	}
	return profileExec(c, "UPDATE learning_classification SET repository=? WHERE family=? AND repository IS NULL", repository, family)
}

type profileMemberClass struct {
	family, profile, project string
	pinned, classified       bool
	class                    profileClass
}

type profileMerge struct {
	class   profileClass
	project string
	target  string
	sources []string
}

// profilePlanMerges is the single grouping rule. Only unpinned profiles whose
// members all share one shareable class merge, into the lowest profile id.
// It never splits a profile.
func profilePlanMerges(members []profileMemberClass) []profileMerge {
	type candidate struct {
		class   profileClass
		project string
		ok      bool
	}
	profiles := map[string]*candidate{}
	sort.Slice(members, func(i, j int) bool {
		if members[i].profile != members[j].profile {
			return members[i].profile < members[j].profile
		}
		return members[i].family < members[j].family
	})
	for _, member := range members {
		entry := profiles[member.profile]
		if entry == nil {
			entry = &candidate{class: member.class, project: member.project, ok: !member.pinned}
			profiles[member.profile] = entry
		}
		entry.ok = entry.ok && member.classified && member.class.shareable() && member.class == entry.class
	}
	byClass := map[profileClass][]string{}
	for id, entry := range profiles {
		if entry.ok {
			byClass[entry.class] = append(byClass[entry.class], id)
		}
	}
	merges := []profileMerge{}
	for class, ids := range byClass {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		merges = append(merges, profileMerge{class: class, project: profiles[ids[0]].project, target: ids[0], sources: ids[1:]})
	}
	sort.Slice(merges, func(i, j int) bool { return merges[i].target < merges[j].target })
	return merges
}

func profileApplyMerges(c profileSQL, merges []profileMerge) (bool, error) {
	for _, merge := range merges {
		for _, source := range merge.sources {
			if e := profileExec(c, "UPDATE learning_members SET profile=? WHERE profile=?", merge.target, source); e != nil {
				return false, e
			}
			if e := profileExec(c, "DELETE FROM learning_profiles WHERE id=? AND NOT EXISTS(SELECT 1 FROM learning_members WHERE profile=?)", source, source); e != nil {
				return false, e
			}
		}
		if e := profileExec(c, "UPDATE learning_profiles SET label=?,updated=? WHERE id=? AND pinned=0", profileSimilarityLabel(merge.class.kind(), merge.project), resourceNow(), merge.target); e != nil {
			return false, e
		}
	}
	return len(merges) > 0, nil
}

// profileGroupSimilar groups by the stored classification. A non-nil class
// limits the work to profiles holding a family of that class, which is all a
// single new observation can affect.
func profileGroupSimilar(c profileSQL, only *profileClass) (bool, error) {
	has, e := hasProfileClassification(c)
	if e != nil || !has {
		return false, e
	}
	query := "SELECT m.family,m.profile,m.project,p.pinned,k.repository,k.tool,k.operation FROM learning_members m JOIN learning_profiles p ON p.id=m.profile LEFT JOIN learning_classification k ON k.family=m.family"
	args := []any{}
	if only != nil {
		query += " WHERE m.profile IN (SELECT m2.profile FROM learning_members m2 JOIN learning_classification k2 ON k2.family=m2.family WHERE k2.repository=? AND k2.tool=? AND k2.operation=?)"
		args = append(args, only.repository, only.tool, only.operation)
	}
	rows, e := profileRows(c, query, args...)
	if e != nil {
		return false, e
	}
	members := make([]profileMemberClass, 0, len(rows))
	for _, row := range rows {
		class, ok := profileStoredClass(row)
		members = append(members, profileMemberClass{family: text(row["family"]), profile: text(row["profile"]), project: text(row["project"]), pinned: metric(row["pinned"]) != 0, classified: ok, class: class})
	}
	return profileApplyMerges(c, profilePlanMerges(members))
}
