package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// profileEvidence is what one metadata file says, read once. Everything the
// retroactive pass concludes is a pure function of a slice of these.
type profileEvidence struct {
	family   string
	clean    Object
	identity profileIdentity
}

// Historical execution payloads are disposable. Join retained numeric runs to
// their private safe metadata by hashed token, without retaining raw commands.
// One bad file or sidecar is skipped: only an unsafe directory, cancellation
// and database errors stop the pass.
func (s *ProfileStore) historicalSimilarityDescriptions(g *Gate) (*profileHistory, error) {
	tokens, scopes := map[string]string{}, map[string]string{}
	if !s.empty {
		if e := s.connection(false, func(c profileSQL) error {
			rows, e := profileRows(c, "SELECT token,family FROM runs")
			if e != nil {
				return e
			}
			for _, row := range rows {
				tokens[text(row["token"])] = text(row["family"])
			}
			if has, e := hasProfileSimilarity(c); e != nil || !has {
				return e
			}
			rows, e = profileRows(c, "SELECT * FROM learning_similarity")
			for _, row := range rows {
				if profileValidLegacy(row) != nil {
					scopes[text(row["family"])] = text(row["scope"])
				}
			}
			return e
		}); e != nil {
			return nil, e
		}
	}
	evidence, e := profileReadEvidence(g, tokens)
	if e != nil {
		return nil, e
	}
	return profileAggregate(evidence, scopes), nil
}

func profileReadEvidence(g *Gate, tokens map[string]string) ([]profileEvidence, error) {
	info, e := os.Lstat(g.Metadata)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !owned || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("Unsafe workload metadata directory")
	}
	entries, e := os.ReadDir(g.Metadata)
	if e != nil {
		return nil, e
	}
	identities, evidence := map[string]profileIdentity{}, []profileEvidence{}
	for _, entry := range entries {
		if e := g.operationContext().Err(); e != nil {
			return nil, e
		}
		value := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validToken(value) {
			continue
		}
		family := tokens[profileDigest([]any{"run-v1", value})]
		if family == "" {
			identity, e := SchedulingIdentity(g, value)
			if e != nil {
				continue
			}
			family = text(identity["family"])
		}
		if family == "" {
			continue
		}
		description, e := ReadPrivate(filepath.Join(g.Metadata, entry.Name()))
		if e != nil {
			continue
		}
		evidence = append(evidence, profileEvidence{family, profileSimilarityDescription(description, identities), profileDescriptionIdentity(text(description["directory"]), identities)})
	}
	return evidence, g.operationContext().Err()
}

// profileAggregate concludes from all the evidence at once, so file order cannot
// matter. Live metadata has no scope of its own, so each file that still
// verifies as a checkout teaches aliases to its repository, from the v1 scope
// computed now and from the scope stored for its family. A stored scope can
// outlive its checkout and differ from a fresh one, and it is how deleted
// worktrees are recovered.
func profileAggregate(evidence []profileEvidence, scopes map[string]string) *profileHistory {
	history := &profileHistory{descriptions: map[string]Object{}, aliases: map[string]string{}, repositories: map[string]string{}}
	type pair struct{ scope, kind string }
	pairs, chosen := map[string]map[pair]bool{}, map[string]string{}
	for _, item := range evidence {
		profileAddAlias(history.aliases, item.identity.scope, item.identity.repository)
		// shortcut: a family's stored scope is trusted, so a path reused by an unrelated repository can assign the wrong repository, upgrade when operators report that more than rarely.
		profileAddAlias(history.aliases, scopes[item.family], item.identity.repository)
		profileAddAlias(history.repositories, item.family, item.identity.repository)
		if item.clean["learningScope"] != nil {
			if pairs[item.family] == nil {
				pairs[item.family] = map[pair]bool{}
			}
			pairs[item.family][pair{text(item.clean["learningScope"]), text(item.clean["learningKind"])}] = true
		}
		// The smallest description registers the family, whatever the file order.
		// Fewer blank fields come first, so a sparse file never beats a described one.
		key, blanks := "", byte('0')
		for _, name := range []string{"project", "title", "executable"} {
			key += text(item.clean[name]) + "\x00"
			if text(item.clean[name]) == "" {
				blanks++
			}
		}
		key = string(blanks) + key
		if prior, seen := chosen[item.family]; !seen || key < prior {
			chosen[item.family] = key
			description := Object{}
			for _, name := range []string{"title", "project", "executable"} {
				if item.clean[name] != nil {
					description[name] = item.clean[name]
				}
			}
			history.descriptions[item.family] = description
		}
	}
	// The legacy table is shared with old clients, which reject unknown kinds, so
	// a family gets a row only when its files name exactly one scope and kind.
	for family, set := range pairs {
		for each := range set {
			if len(set) == 1 {
				history.descriptions[family]["learningScope"], history.descriptions[family]["learningKind"] = each.scope, each.kind
			}
		}
	}
	return history
}

func profileRegroupPlan(c profileSQL, history *profileHistory) (Object, map[string]profileClass, error) {
	profiles, groups, e := profileDataset(c)
	if e != nil {
		return nil, nil, e
	}
	registered := map[string]bool{}
	if has, e := hasProfileLearning(c); e != nil {
		return nil, nil, e
	} else if has {
		rows, e := profileRows(c, "SELECT family FROM learning_members")
		if e != nil {
			return nil, nil, e
		}
		for _, row := range rows {
			registered[text(row["family"])] = true
		}
	}
	// Apply registers a family that only has runs from its description, so
	// the preview classifies it from the same description.
	members, owners := []Object{}, []string{}
	for id, group := range groups {
		for _, member := range group.members {
			if description := history.descriptions[text(member["family"])]; !registered[text(member["family"])] && description != nil {
				member = Object{"family": member["family"], "title": description["title"], "project": description["project"], "executable": description["executable"]}
			}
			members, owners = append(members, member), append(owners, id)
		}
	}
	classes, e := profileBackfillClasses(c, history, members)
	if e != nil {
		return nil, nil, e
	}
	planned := make([]profileMemberClass, 0, len(members))
	for i, member := range members {
		family := text(member["family"])
		planned = append(planned, profileMemberClass{family: family, profile: owners[i], project: text(member["project"]), pinned: metric(profiles[owners[i]]["pinned"]) != 0, classified: true, class: classes[family]})
	}
	merges := profilePlanMerges(planned)
	grouped, proposed, removed := 0, []Object{}, 0
	for _, merge := range merges {
		ids := append([]string{merge.target}, merge.sources...)
		count, successes, incomplete := 0, 0, 0
		for _, id := range ids {
			count += len(groups[id].members)
			for _, run := range groups[id].runs {
				if metric(run["success"]) == 1 {
					successes++
				} else {
					incomplete++
				}
			}
		}
		proposed = append(proposed, Object{"id": merge.target, "label": profileSimilarityLabel(merge.class.kind(), merge.project), "memberCount": count, "successCount": successes, "incompleteCount": incomplete})
		grouped += count
		removed += len(merge.sources)
	}
	revision, e := profileRevision(c)
	if e != nil {
		return nil, nil, e
	}
	return Object{"revision": revision, "profilesBefore": len(profiles), "profilesAfter": len(profiles) - removed, "familiesGrouped": grouped, "groups": proposed, "applied": false}, classes, nil
}

func (s *ProfileStore) regroup(history *profileHistory, revision int, apply bool) (Object, error) {
	if s.empty {
		return Object{"revision": 0, "profilesBefore": 0, "profilesAfter": 0, "familiesGrouped": 0, "groups": []Object{}, "applied": false}, nil
	}
	var result Object
	e := s.connection(apply, func(c profileSQL) error {
		if apply {
			if e := profileExpectedRevision(c, revision); e != nil {
				return e
			}
		}
		var e error
		var classes map[string]profileClass
		result, classes, e = profileRegroupPlan(c, history)
		if e != nil || !apply {
			return e
		}
		profiles, groups, e := profileDataset(c)
		if e != nil {
			return e
		}
		families := map[string]bool{}
		for id := range profiles {
			for _, member := range groups[id].members {
				families[text(member["family"])] = true
			}
		}
		keys := []string{}
		for family := range history.descriptions {
			if families[family] {
				keys = append(keys, family)
			}
		}
		sort.Strings(keys)
		for _, family := range keys {
			clean := history.descriptions[family]
			member, e := profileRegister(c, family, clean, false)
			if e != nil {
				return e
			}
			if member == nil {
				continue // The registry is full; this family stays unregistered.
			}
			if text(clean["learningKind"]) != "" {
				if e = profileExec(c, "INSERT OR IGNORE INTO learning_similarity VALUES (?,?,?)", family, clean["learningScope"], clean["learningKind"]); e != nil {
					return e
				}
			}
		}
		members, e := profileRows(c, "SELECT family FROM learning_members ORDER BY family")
		if e != nil {
			return e
		}
		for _, member := range members {
			if class, ok := classes[text(member["family"])]; ok {
				if e = profileStoreClass(c, text(member["family"]), class); e != nil {
					return e
				}
			}
		}
		changed, e := profileGroupSimilar(c, nil)
		if e != nil {
			return e
		}
		if changed {
			if e = profileChanged(c); e != nil {
				return e
			}
		}
		result["revision"], e = profileRevision(c)
		result["applied"] = true
		return e
	})
	return result, e
}

func LearningRegroup(g *Gate, apply bool, request Object) (Object, error) {
	store, e := NewProfileStore(g.State, true)
	if e != nil {
		return nil, e
	}
	history, e := store.historicalSimilarityDescriptions(g)
	if e != nil {
		return nil, e
	}
	if !apply {
		return store.regroup(history, 0, false)
	}
	revision, ok := schedulingInteger(request["revision"])
	if !ok || revision < 0 || len(request) != 1 {
		return nil, fmt.Errorf("Regroup requires the reviewed learning revision")
	}
	if e = g.Prepare(); e != nil {
		return nil, e
	}
	unlockScheduling, e := LockPrivate(g, "scheduling.lock")
	if e != nil {
		return nil, e
	}
	defer unlockScheduling()
	unlockLease, e := LockPrivate(g, "")
	if e != nil {
		return nil, e
	}
	defer unlockLease()
	store, e = NewProfileStore(g.State, false)
	if e != nil {
		return nil, e
	}
	if current, e := store.Revision(); e != nil || current != revision {
		if e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("Learning changed since you opened it; refresh before saving")
	}
	// Rotate grants through the same guarded path as an operator correction.
	// Active ownership and manual overrides survive; pending grants do not.
	plan, e := store.regroup(history, revision, false)
	if e != nil {
		return nil, e
	}
	if plan["profilesBefore"] != plan["profilesAfter"] {
		if e = invalidateLearningAdmissions(g); e != nil {
			return nil, e
		}
	}
	return store.regroup(history, revision, true)
}

func (s *ProfileStore) backfillSimilarity(g *Gate) error {
	needed := false
	if e := s.connection(false, func(c profileSQL) error {
		row, e := profileRow(c, "SELECT version FROM learning_similarity_state WHERE id=1")
		needed = metric(row["version"]) < profileClassificationVersion
		return e
	}); e != nil || !needed {
		return e
	}
	revision, e := s.Revision()
	if e != nil {
		return e
	}
	_, e = LearningRegroup(g, true, Object{"revision": revision})
	if e != nil {
		return e
	}
	// Only the updated controller marks its backfill complete. An explicit CLI
	// regroup can precede a runtime upgrade while old clients still add families.
	return s.connection(true, func(c profileSQL) error {
		return profileExec(c, "UPDATE learning_similarity_state SET version=? WHERE id=1", profileClassificationVersion)
	})
}
