package catalog

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// ErrPickerUnsupported identifies a query requiring the complete compatibility reader.
var ErrPickerUnsupported = errors.New("picker query is unsupported; use the complete compatibility reader")

// ErrPickerBudget means no complete page or emptiness proof could be established.
var ErrPickerBudget = errors.New("picker work budget exhausted; retry with an explicit larger budget")

// ErrPickerIdentityAmbiguous means distinct physical aliases share one logical identity.
// Callers cannot infer absence or select an arbitrary archived copy.
var ErrPickerIdentityAmbiguous = errors.New("picker canonical identity has multiple physical publications; choose an exact archived row")

// PickerScope describes the exact repository matcher, without caller predicates.
// All selects every root. Otherwise known RepoKeys match only that RepoKey;
// ProjectIDs also match missing RepoKeys. Without a RepoKey, ProjectIDs match all repositories.
type PickerScope struct {
	All        bool
	RepoKey    string
	ProjectIDs []string
}

// PickerBudget limits node visits and decoded bytes, including cache hits.
// Zero fields use practical defaults: 8192 nodes and 32 MiB per logical operation.
type PickerBudget struct {
	Nodes int
	Bytes int64
}

// PickerQuery selects browse or handoff roots, optionally one canonical harness.
type PickerQuery struct {
	Scope      PickerScope
	Membership string
	Harness    string
	Limit      int
	Budget     PickerBudget
}

// PickerCost counts actual bounded tree work separately from local inventory work.
// HeadObservations and CoordinatorReads count this logical operation's fresh reads.
type PickerCost struct {
	Nodes            int
	Bytes            int64
	HeadObservations int
	CoordinatorReads int
}

// PickerReference is an immutable, snapshot-bound selection authority. Its zero
// value and references from another snapshot are invalid; projections cannot forge it.
type PickerReference struct {
	snapshot *Snapshot
	row      PickerRow
}

// PickerItem joins presentation facts to their immutable selection reference.
type PickerItem struct {
	Row       PickerRow
	Reference PickerReference
}

// PickerPage is complete for the requested page. Total is exact because normalized
// membership ranges are disjoint. Empty proves the whole supported scope empty.
type PickerPage struct {
	Rows  []PickerItem
	Next  string
	More  bool
	Total uint64
	Empty bool
	Cost  PickerCost
}

// PickerIdentity uses a canonical harness/session association. Returned facts
// and references retain the physical raw harness binding; alias conflicts are errors.
type PickerIdentity struct {
	Harness   string
	SessionID string
}

// PickerIdentityResult distinguishes exact absence from a classified publication.
// Lookup is unfiltered: children, replays and known zero-prompt copies are found.
type PickerIdentityResult struct {
	Identity PickerIdentity
	Found    bool
	Item     PickerItem
}

// PickerIdentityBatch contains results in input order under one captured view.
type PickerIdentityBatch struct {
	Results []PickerIdentityResult
	Cost    PickerCost
}

// PickerHydration returns a verified body with existing consumable body provenance.
// Callers still apply current source, replay and destination policy before source reads.
type PickerHydration struct {
	Row Row
	Raw []byte
}

type pickerWorkKey struct{}
type pickerWork struct {
	budget PickerBudget
	cost   PickerCost
}

func (w *pickerWork) node() error {
	if w.cost.Nodes >= w.budget.Nodes {
		return ErrPickerBudget
	}
	w.cost.Nodes++
	return nil
}
func (w *pickerWork) bytes(n int) error {
	if int64(n) > w.budget.Bytes-w.cost.Bytes {
		return ErrPickerBudget
	}
	w.cost.Bytes += int64(n)
	return nil
}
func pickerWorkContext(ctx context.Context, b PickerBudget) (context.Context, *pickerWork, error) {
	if b.Nodes < 0 || b.Bytes < 0 {
		return ctx, nil, ErrPickerUnsupported
	}
	if b.Nodes == 0 {
		b.Nodes = 8192
	}
	if b.Bytes == 0 {
		b.Bytes = 32 << 20
	}
	w := &pickerWork{budget: b}
	return context.WithValue(ctx, pickerWorkKey{}, w), w, nil
}
func (s *Snapshot) pickerStart(ctx context.Context, w *pickerWork) error {
	if s.head.Picker == nil || !s.head.Picker.valid() {
		return ErrPickerUnsupported
	}
	s.requestMu.Lock()
	first := s.requests == 0
	s.requests++
	s.requestMu.Unlock()
	w.cost.CoordinatorReads++
	w.cost.HeadObservations++
	if first {
		// Opening supplied both fresh activation and head observations.
		return s.check(ctx)
	}
	if err := s.writer.Coordinator().Active(ctx); err != nil {
		return err
	}
	return s.ValidateContinuation(ctx)
}
func normalizePicker(q PickerQuery) (PickerQuery, []Query, error) {
	if q.Limit < 1 || q.Limit > 100 || (q.Membership != "browse" && q.Membership != "handoff") {
		return q, nil, ErrPickerUnsupported
	}
	if q.Harness != "" {
		q.Harness = archive.CanonicalHarness(q.Harness)
		if len(q.Harness) > 1024 || !utf8.ValidString(q.Harness) {
			return q, nil, ErrPickerUnsupported
		}
	}
	if q.Scope.All && (q.Scope.RepoKey != "" || len(q.Scope.ProjectIDs) > 0) {
		return q, nil, ErrPickerUnsupported
	}
	if q.Scope.RepoKey != "" && !archive.IsRepoKey(q.Scope.RepoKey) {
		return q, nil, ErrPickerUnsupported
	}
	q.Scope.ProjectIDs = slices.Clone(q.Scope.ProjectIDs)
	slices.Sort(q.Scope.ProjectIDs)
	q.Scope.ProjectIDs = slices.Compact(q.Scope.ProjectIDs)
	for _, p := range q.Scope.ProjectIDs {
		if p == "" || len(p) > 512 || !utf8.ValidString(p) {
			return q, nil, ErrPickerUnsupported
		}
	}
	scopes := []string{}
	if q.Scope.All {
		scopes = append(scopes, "global/-")
	} else {
		kind := "project-any/"
		if q.Scope.RepoKey != "" {
			scopes = append(scopes, "repo/"+hex.EncodeToString([]byte(q.Scope.RepoKey)))
			kind = "project-missing/"
		}
		for _, p := range q.Scope.ProjectIDs {
			scopes = append(scopes, kind+hex.EncodeToString([]byte(p)))
		}
	}
	if len(scopes) > 32 {
		return q, nil, ErrPickerUnsupported
	}
	harness := "all"
	if q.Harness != "" {
		harness = hex.EncodeToString([]byte(q.Harness))
	}
	ranges := make([]Query, 0, len(scopes))
	for _, scope := range scopes {
		prefix := "!picker/v1/" + q.Membership + "/" + scope + "/" + harness + "/"
		ranges = append(ranges, Query{Index: ProjectIndex, Lower: prefix, Upper: prefix + "~", Reverse: true})
	}
	return q, ranges, nil
}

type pickerCursor struct {
	Nonce string
	Head  CatalogHead
	Query PickerQuery
	After []string
	Seal  string
}

func (s *Snapshot) pickerSeal(t pickerCursor) string {
	t.Seal = ""
	raw, _ := json.Marshal(t)
	mac := hmac.New(sha256.New, []byte(s.secret))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Snapshot) decodePickerCursor(next string, q PickerQuery, n int) ([]string, error) {
	if next == "" {
		return make([]string, n), nil
	}
	if len(next) > 64<<10 {
		return nil, ErrStaleCursor
	}
	encoded, err := base64.RawURLEncoding.DecodeString(next)
	if err != nil {
		return nil, ErrStaleCursor
	}
	reader, err := zlib.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return nil, ErrStaleCursor
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return nil, ErrStaleCursor
	}
	var token pickerCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&token) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrStaleCursor
	}
	canonical, err := json.Marshal(token)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, ErrStaleCursor
	}
	expected := pickerCursor{Nonce: s.nonce, Head: s.head, Query: q, After: token.After}
	want, err := hex.DecodeString(s.pickerSeal(expected))
	got, e := hex.DecodeString(token.Seal)
	// The seal covers all query, format, exact roots and nonce bindings. Compare
	// the expected envelope rather than trusting token fields after decoding.
	actual, aerr := hex.DecodeString(s.pickerSeal(token))
	if err != nil || e != nil || aerr != nil || !hmac.Equal(actual, got) || !hmac.Equal(want, got) || token.Nonce != s.nonce || len(token.After) != n {
		return nil, ErrStaleCursor
	}
	return token.After, nil
}
func clonePickerRow(r PickerRow) PickerRow {
	if r.PromptCount != nil {
		n := *r.PromptCount
		r.PromptCount = &n
	}
	return r
}
func (s *Snapshot) pickerItem(r PickerRow) PickerItem {
	return PickerItem{Row: clonePickerRow(r), Reference: PickerReference{snapshot: s, row: clonePickerRow(r)}}
}
func pickerBefore(a, b PickerRow) bool {
	if !a.Activity.Equal(b.Activity) {
		return a.Activity.After(b.Activity)
	}
	if !a.CapturedAt.Equal(b.CapturedAt) {
		return a.CapturedAt.After(b.CapturedAt)
	}
	return a.SessionKey < b.SessionKey
}

// PickerPage merges at most 32 disjoint ordered membership ranges. Each range
// reads at most Limit+1 candidates; continuations record only consumed keys.
// Errors, including budget exhaustion, return no successful partial page.
func (s *Snapshot) PickerPage(ctx context.Context, q PickerQuery, next string) (PickerPage, error) {
	q, ranges, err := normalizePicker(q)
	if err != nil {
		return PickerPage{}, err
	}
	ctx, work, err := pickerWorkContext(ctx, q.Budget)
	if err != nil {
		return PickerPage{}, err
	}
	if err = s.pickerStart(ctx, work); err != nil {
		return PickerPage{}, err
	}
	after, err := s.decodePickerCursor(next, q, len(ranges))
	if err != nil {
		return PickerPage{}, err
	}
	buffers := make([][]Row, len(ranges))
	positions := make([]int, len(ranges))
	result := PickerPage{}
	for i, r := range ranges {
		count, e := s.Count(ctx, r)
		if e != nil {
			return PickerPage{}, e
		}
		if count > math.MaxUint64-result.Total {
			return PickerPage{}, errors.New("picker count overflow")
		}
		result.Total += count
		page := Page{}
		last := ""
		if err = s.visitRange(ctx, r, snapshotCursor{After: after[i]}, s.head.Project, 0, q.Limit, &page, &last); err != nil {
			return PickerPage{}, err
		}
		buffers[i] = page.Rows
	}
	for len(result.Rows) < q.Limit {
		best := -1
		for i, b := range buffers {
			if positions[i] >= len(b) {
				continue
			}
			if best < 0 || pickerBefore(*b[positions[i]].Entry.Picker, *buffers[best][positions[best]].Entry.Picker) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		row := buffers[best][positions[best]]
		positions[best]++
		keys, e := PickerMembershipKeys(row.Key, *row.Entry.Picker)
		if e != nil {
			return PickerPage{}, e
		}
		for _, key := range keys {
			if rangeContains(key, ranges[best], "") {
				after[best] = key
				break
			}
		}
		result.Rows = append(result.Rows, s.pickerItem(*row.Entry.Picker))
	}
	for i, b := range buffers {
		if positions[i] < len(b) {
			result.More = true
		}
	}
	if result.More {
		t := pickerCursor{Nonce: s.nonce, Head: s.head, Query: q, After: after}
		t.Seal = s.pickerSeal(t)
		raw, e := json.Marshal(t)
		if e != nil {
			return PickerPage{}, e
		}
		var compressed bytes.Buffer
		encoder := zlib.NewWriter(&compressed)
		if _, e = encoder.Write(raw); e != nil {
			return PickerPage{}, e
		}
		if e = encoder.Close(); e != nil {
			return PickerPage{}, e
		}
		result.Next = base64.RawURLEncoding.EncodeToString(compressed.Bytes())
		if len(result.Next) > 64<<10 {
			return PickerPage{}, ErrPickerUnsupported
		}
	}
	result.Empty = result.Total == 0
	result.Cost = work.cost
	return result, s.check(ctx)
}

// PickerLookup performs a finite exact unfiltered identity batch, with one fresh
// head observation and pinned internal walks. Each identity costs O(tree height).
func (s *Snapshot) PickerLookup(ctx context.Context, ids []PickerIdentity) (PickerIdentityBatch, error) {
	if len(ids) > 100 {
		return PickerIdentityBatch{}, ErrPickerUnsupported
	}
	ranges := make([]Query, len(ids))
	normalized := slices.Clone(ids)
	for i, id := range ids {
		lo, hi, e := PickerIdentityRange(id.Harness, id.SessionID)
		if e != nil {
			return PickerIdentityBatch{}, e
		}
		normalized[i].Harness = archive.CanonicalHarness(id.Harness)
		ranges[i] = Query{Index: ProjectIndex, Lower: lo, Upper: hi}
	}
	ctx, work, err := pickerWorkContext(ctx, PickerBudget{})
	if err != nil {
		return PickerIdentityBatch{}, err
	}
	if err = s.pickerStart(ctx, work); err != nil {
		return PickerIdentityBatch{}, err
	}
	result := PickerIdentityBatch{Results: make([]PickerIdentityResult, len(ids))}
	for i, q := range ranges {
		result.Results[i].Identity = normalized[i]
		count, e := s.Count(ctx, q)
		if e != nil {
			return PickerIdentityBatch{}, e
		}
		if count == 0 {
			continue
		}
		if count > 1 {
			return PickerIdentityBatch{}, ErrPickerIdentityAmbiguous
		}
		page := Page{}
		last := ""
		if err = s.visitRange(ctx, q, snapshotCursor{}, s.head.Project, 0, 1, &page, &last); err != nil {
			return PickerIdentityBatch{}, err
		}
		if len(page.Rows) != 1 || page.Rows[0].Entry.Picker == nil {
			return PickerIdentityBatch{}, errors.New("picker canonical identity count differs from its leaf")
		}
		r := page.Rows[0].Entry.Picker
		if r.CanonicalHarness != normalized[i].Harness || r.SessionID != normalized[i].SessionID {
			return PickerIdentityBatch{}, errors.New("picker canonical lookup identity mismatch")
		}
		result.Results[i].Found = true
		result.Results[i].Item = s.pickerItem(*r)
	}
	result.Cost = work.cost
	return result, s.check(ctx)
}

// HydratePicker verifies the selected immutable body against its captured facts
// and the currently observed authority. It never substitutes a same-ID rewrite.
func (s *Snapshot) HydratePicker(ctx context.Context, ref PickerReference) (PickerHydration, error) {
	if ref.snapshot != s || ref.row.SessionKey == "" {
		return PickerHydration{}, errors.New("invalid or foreign picker selection reference")
	}
	if err := s.writer.Coordinator().Active(ctx); err != nil {
		return PickerHydration{}, err
	}
	if err := s.ValidateContinuation(ctx); err != nil {
		return PickerHydration{}, err
	}
	entry, err := s.findPinned(ctx, ref.row.SessionKey)
	if err != nil {
		return PickerHydration{}, err
	}
	if entry == nil || entry.Picker == nil || !samePickerRow(*entry.Picker, ref.row) {
		return PickerHydration{}, errors.New("picker reference differs from captured identity")
	}
	raw, err := s.ReadMetadata(ctx, *entry)
	if err != nil {
		return PickerHydration{}, err
	}
	body, err := decodeEntryBody(*entry, raw)
	if err != nil {
		return PickerHydration{}, err
	}
	full := *entry
	full.Summary = body
	full.SummaryOverflow = ""
	projection, err := BuildPickerRow(ref.row.SessionKey, full)
	if err != nil || !samePickerRow(*projection, ref.row) {
		return PickerHydration{}, errors.New("picker selected body projection mismatch")
	}
	row := Row{Key: ref.row.SessionKey, Entry: full}
	hash, err := rowDigest(row)
	if err != nil {
		return PickerHydration{}, err
	}
	row.resolved = &resolvedRow{snapshot: s, key: row.Key, hash: hash}
	return PickerHydration{Row: row, Raw: raw}, s.check(ctx)
}
