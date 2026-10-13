package stats

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// unit is one session as the statistics see it: a top-level session with
// its subagent sessions rolled in. Everything the engine aggregates is
// computed here once.
type unit struct {
	root          *archive.Metadata
	capturedAt    time.Time
	sessionID     string
	messages      int
	compacted     bool
	subagentCount int
	children      []*archive.Metadata
	harness       string
	project       string

	// day is the root's captured_at day as a day number in the window's
	// zone (see dayNumber); month is year*12 + month - 1.
	day   int
	month int

	// perModel is the unit's tokens by normalized model id, sorted by id,
	// each priced; tokens and cost are their sums. Subagents are included.
	perModel    []modelUse
	tokens      tokenSet
	hasData     bool
	approximate bool
	cost        costAcc

	// rootTokens is the root's own tokens, without its subagents.
	rootTokens  tokenSet
	rootHasData bool
	// childTokens is the subagents' tokens; childrenWithData how many
	// subagents reported any.
	childTokens        int64
	childrenWithData   int
	prompts            *int64
	toolErrors         int64
	toolResults        int64
	toolMembersKnown   int
	toolMembersUnknown int
	skills             map[string]struct{}
	mcp                map[string]int64
	lacksParser014     bool
	orphan             bool
}

type modelUse struct {
	id     string
	set    tokenSet
	cost   costAcc
	label  string
	priced bool
}

// buildUnits groups metadata into units, ordered by the root's captured_at
// and then id, so every later sum runs in one order whatever order the input
// came in.
func buildUnits(sessions []archive.Metadata, loc *time.Location, prices priceIndex) []*unit {
	metas := dedupe(sessions)
	byID := make(map[string]*archive.Metadata, len(metas))
	for _, m := range metas {
		if m.SessionID != "" {
			byID[m.SessionID] = m
		}
	}
	roots := newRootResolver(byID)
	units := map[*archive.Metadata]*unit{}
	var ordered []*unit
	get := func(root *archive.Metadata) *unit {
		if u, ok := units[root]; ok {
			return u
		}
		u := &unit{root: root}
		units[root] = u
		ordered = append(ordered, u)
		return u
	}
	for _, m := range metas {
		root := roots.resolve(m)
		u := get(root)
		if m != root {
			u.children = append(u.children, m)
		}
	}
	lookup := &modelLookup{prices: prices, normalized: map[string]string{}, entries: map[string]modelPriceLookup{}}
	scratch := unitScratch{member: map[string]tokenSet{}, merged: map[string]tokenSet{}}
	for _, u := range ordered {
		u.finish(loc, lookup, &scratch)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return metadataBefore(ordered[i].root, ordered[j].root) })
	return ordered
}

// metadataBefore orders metadata by captured_at, then session id, then (only
// for records equal in both, such as sessions with no id) by their JSON, so
// the order of the input never changes a float sum or a tie.
func metadataBefore(a, b *archive.Metadata) bool {
	if !a.CapturedAt.Equal(b.CapturedAt) {
		return a.CapturedAt.Before(b.CapturedAt)
	}
	if a.SessionID != b.SessionID {
		return a.SessionID < b.SessionID
	}
	return metadataKey(a) < metadataKey(b)
}

// metadataKey is a record's JSON, the last tie-break between records that are
// otherwise the same session at the same time.
func metadataKey(m *archive.Metadata) string {
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(data)
}

// dedupe drops repeated session ids, keeping the metadata derived last, and
// returns the rest sorted by (captured_at, session id).
func dedupe(sessions []archive.Metadata) []*archive.Metadata {
	kept := make([]*archive.Metadata, 0, len(sessions))
	index := map[string]int{}
	for i := range sessions {
		m := &sessions[i]
		if m.SessionID != "" {
			if at, seen := index[m.SessionID]; seen {
				// Derived at the same instant: keep the one whose JSON sorts
				// last, so the order of the input does not pick.
				if derived := kept[at].MetadataDerivedAt; m.MetadataDerivedAt.After(derived) ||
					(m.MetadataDerivedAt.Equal(derived) && metadataKey(m) > metadataKey(kept[at])) {
					kept[at] = m
				}
				continue
			}
			index[m.SessionID] = len(kept)
		}
		kept = append(kept, m)
	}
	sort.SliceStable(kept, func(i, j int) bool { return metadataBefore(kept[i], kept[j]) })
	return kept
}

// rootResolver finds each session's root: the topmost session present in the
// input, following parent_session_id. A session that names no parent, or whose
// parent is missing (an orphan subagent), is its own root, and so is every
// session whose chain loops, so no session is ever counted twice. It remembers
// the answer for every session on a path it walked, so a chain of any depth
// costs one walk in total, not one per session.
type rootResolver struct {
	byID map[string]*archive.Metadata
	done map[*archive.Metadata]rootAnswer
}

// rootAnswer is a session's root; looped is set when its chain never ends.
type rootAnswer struct {
	root   *archive.Metadata
	looped bool
}

func newRootResolver(byID map[string]*archive.Metadata) *rootResolver {
	return &rootResolver{byID: byID, done: map[*archive.Metadata]rootAnswer{}}
}

func (r *rootResolver) resolve(m *archive.Metadata) *archive.Metadata {
	if answer, ok := r.done[m]; ok {
		return answer.root
	}
	var path []*archive.Metadata
	var onPath map[*archive.Metadata]bool
	var answer rootAnswer
	for current := m; ; {
		if known, ok := r.done[current]; ok {
			answer = known
			break
		}
		path = append(path, current)
		parent, ok := r.byID[current.ParentSessionID]
		if current.ParentSessionID == "" || !ok {
			answer = rootAnswer{root: current}
			break
		}
		if onPath == nil {
			onPath = map[*archive.Metadata]bool{}
		}
		onPath[current] = true
		if onPath[parent] || parent == current {
			answer = rootAnswer{looped: true}
			break
		}
		current = parent
	}
	for _, member := range path {
		if answer.looped {
			r.done[member] = rootAnswer{root: member, looped: true}
		} else {
			r.done[member] = answer
		}
	}
	return r.done[m].root
}

// unitScratch belongs to one buildUnits call. Units copy its values into their
// own model slices before the scratch is reused for another unit.
type unitScratch struct {
	member map[string]tokenSet
	merged map[string]tokenSet
	ids    []string
}

// finish computes everything derived from the unit's members.
func (u *unit) finish(loc *time.Location, lookup *modelLookup, scratch *unitScratch) {
	root := u.root
	u.capturedAt = root.CapturedAt
	u.sessionID = root.SessionID
	if root.Counts.Messages != nil {
		u.messages = *root.Counts.Messages
	}
	u.compacted = root.Counts.Compactions != nil && *root.Counts.Compactions > 0
	u.subagentCount = len(u.children)
	u.harness = archive.CanonicalHarness(root.Harness.Name)
	u.project = strings.TrimSpace(root.ProjectName)
	captured := root.CapturedAt.In(loc)
	u.day = dayNumber(civilOf(captured))
	u.month = captured.Year()*12 + int(captured.Month()) - 1
	u.orphan = root.IsChild()
	u.lacksParser014 = !parserAtLeast(root.Parser.Version, 0, 14, 0)
	if root.Counts.Turns != nil {
		turns := value(root.Counts.Turns)
		u.prompts = &turns
	}
	merged := scratch.merged
	clear(merged)
	for i := 0; i <= len(u.children); i++ {
		m := root
		if i > 0 {
			m = u.children[i-1]
		}
		has, approximate := memberUsage(m, lookup.normalize, scratch.member)
		byModel := scratch.member
		var memberTotal tokenSet
		for id, set := range byModel {
			all := merged[id]
			all.add(set)
			merged[id] = all
			memberTotal.add(set)
		}
		if has {
			u.hasData = true
			u.approximate = u.approximate || approximate
			if i == 0 {
				u.rootHasData = true
				u.rootTokens = memberTotal
			} else {
				u.childrenWithData++
				u.childTokens = satAdd(u.childTokens, memberTotal.total())
			}
		}
		u.addActivity(m)
	}
	scratch.ids = scratch.ids[:0]
	for id := range merged {
		scratch.ids = append(scratch.ids, id)
	}
	sort.Strings(scratch.ids)
	u.perModel = make([]modelUse, 0, len(scratch.ids))
	for _, id := range scratch.ids {
		entry := lookup.price(id)
		use := modelUse{id: id, set: merged[id], label: entry.label, priced: entry.priced}
		if entry.priced {
			usd := entry.entry.cost(use.set)
			use.cost = costAcc{usd: usd, priced: true, approximate: u.approximate}
		} else {
			use.cost = costAcc{unpriced: use.set.total(), approximate: u.approximate}
		}
		u.perModel = append(u.perModel, use)
		u.tokens.add(use.set)
		u.cost.add(use.cost)
	}
}

// addActivity copies tool, skill and MCP totals from one member.
func (u *unit) addActivity(m *archive.Metadata) {
	u.addToolErrors(m)
	for _, skill := range m.SkillsUsed {
		if name := strings.TrimSpace(skill.Name); name != "" {
			if u.skills == nil {
				u.skills = map[string]struct{}{}
			}
			u.skills[name] = struct{}{}
		}
	}
	for _, call := range m.MCPCalls {
		if name := strings.TrimSpace(call.Name); name != "" && call.Count > 0 {
			if u.mcp == nil {
				u.mcp = map[string]int64{}
			}
			u.mcp[name] = satAdd(u.mcp[name], int64(call.Count))
		}
	}
}

// addToolErrors counts a member's tool errors when it knows them: both the
// error count and the results it is out of.
func (u *unit) addToolErrors(m *archive.Metadata) {
	if m.Counts.ToolErrors == nil || m.Counts.ToolResults == nil {
		u.toolMembersUnknown++
		return
	}
	u.toolMembersKnown++
	u.toolErrors = satAdd(u.toolErrors, value(m.Counts.ToolErrors))
	u.toolResults = satAdd(u.toolResults, value(m.Counts.ToolResults))
}

// parserAtLeast reports whether a "major.minor.patch" version is at least
// the given one. A version that does not parse is older than any.
func parserAtLeast(version string, major, minor, patch int) bool {
	parts := strings.Split(strings.TrimSpace(version), ".")
	if len(parts) != 3 {
		return false
	}
	var have [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		have[i] = n
	}
	want := [3]int{major, minor, patch}
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}
