package cursor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// SettledImportFacts records verified same-transaction native selection facts.
// Public facts alone confer no admission or publication authority.
type SettledImportFacts struct {
	Chat         agentapi.DatabaseChat `json:"chat"`
	GlobalSHA256 string                `json:"global_sha256"`
}

// SettledImportEvidence owns an actual immutable capture. Its zero value and
// closed copies cannot supply a selected stage; copies share the same lifetime.
type SettledImportEvidence struct{ capture *settledCapture }
type settledCapture struct {
	mu      sync.Mutex
	closed  bool
	reg     archive.SessionRegistration
	bundle  archive.SourceBundle
	facts   SettledImportFacts
	digest  string
	owner   *settledReservation
	charged int64
}

type settledReadBudget struct {
	owner   *settledReservation
	rows    int
	charged int64
}

func (b *settledReadBudget) RemainingRows() int    { return b.rows }
func (b *settledReadBudget) RemainingBytes() int64 { return b.owner.Available() }
func (b *settledReadBudget) Charge(rows int, bytes int64) error {
	if rows < 0 || rows > b.rows || bytes < 0 || !b.owner.Reserve(bytes) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	b.rows -= rows
	b.charged += bytes
	return nil
}

// CaptureSettledImport performs the actual settled read and Cursor filter.
// It accepts reviewed observations as comparisons, never as capture authority.
func CaptureSettledImport(ctx context.Context, database string, reg archive.SessionRegistration, reviewed agentapi.DatabaseChat, at time.Time, budget *agentapi.NativeReadBudget) (_ *SettledImportEvidence, err error) {
	if budget == nil || reg.Harness.Name != archive.HarnessCursor || reg.SourceKind != archive.SourceKindCursorSQLite || reg.SourceKey != reg.NativeSessionID || reg.HistoricalProject != nil || reg.ParentSessionID != "" || reg.NativeChild || reg.ParentNativeSessionID != "" || reg.NativeRootSessionID != "" || reg.NativeSourceHome != "" || reg.PreviousGenerationID != "" || reg.CaptureFrozen || reg.CodexBinding != nil || reg.CodexAdmission != nil {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("unsupported settled Cursor owner"))
	}
	owner := &settledReservation{original: budget.Reservation(), limit: 128 << 20}
	b := &settledReadBudget{owner: owner, rows: 65536}
	keep := false
	defer func() {
		if !keep {
			owner.Release(b.charged)
		}
	}()
	var selected agentapi.DatabaseChat
	global := sha256.New()
	composer, err := cursorstore.ReadSettledAdmission(ctx, database, reg.SourceKey, 128<<20, archive.MaxRecordBytes, b, func(row agentapi.DatabaseRecord) error {
		// The same owner reserves bounded JSON maps and sorting scratch before decode.
		scratch := 2*int64(len(row.Value)) + 32<<10
		if !owner.Reserve(scratch) {
			return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
		}
		defer owner.Release(scratch)
		if err := validateSettledMetadata(ctx, row.Key, row.Value); err != nil {
			return err
		}
		decoded, ok := decodeComposerData(row.Key, row.Value)
		if !ok || decoded.chat.Malformed || decoded.chat.KeyID == "" || len(decoded.chat.KeyID) > 255 {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		facts := decoded.chat.CursorFacts
		if facts.VersionPresent && (facts.Version < 1 || facts.Version > maxComposerVersion) || facts.Relationships == agentapi.CursorRelationshipsMalformed {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		for _, child := range decoded.subagents {
			if child == reg.SourceKey {
				return cursorstore.NotChecked(cursorstore.UnknownFormat)
			}
		}
		// Ordered record hashes preserve the complete observed global routing set.
		sum := sha256.Sum256(row.Value)
		keySum := sha256.Sum256([]byte(row.Key))
		global.Write(keySum[:])
		global.Write(sum[:])
		if decoded.chat.KeyID == reg.SourceKey {
			if !decoded.counted || decoded.chat.Malformed || decoded.chat.ID != reg.NativeSessionID {
				return cursorstore.NotChecked(cursorstore.UnknownFormat)
			}
			selected = decoded.chat
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if selected != reviewed || selected.CreatedAt.IsZero() || !selected.CreatedAt.Equal(reg.SessionStartedAt) || selected.Folder == "" || filepath.Clean(selected.Folder) != filepath.Clean(reg.ProjectRoot) {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("settled Cursor review or routing changed"))
	}
	// Keep raw/filter/bundle simultaneous views charged under the captured owner.
	rawExtent := int64(len(composer.Composer))
	for _, bubble := range composer.Bubbles {
		if int64(len(bubble.Value)) > (128<<20)-rawExtent {
			return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
		}
		rawExtent += int64(len(bubble.Value))
	}
	scratch := 2*rawExtent + 32<<20
	if !owner.Reserve(scratch) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	b.charged += scratch
	pass := &sourcePass{live: map[*chatSnapshot]bool{}}
	snapshot := &chatSnapshot{owner: pass, composer: composer}
	pass.live[snapshot] = true
	defer snapshot.Close()
	filter := Filter{}
	records := snapshot.Input().Records
	first, ok, err := records.Next(ctx)
	if err != nil || !ok || first.Kind != agentapi.ComposerRecord {
		return nil, errors.Join(cursorstore.NotChecked(cursorstore.UnknownFormat), err)
	}
	filtered, closeFiltered, err := nativecodec.FilterComposerRecordsBounded(ctx, first.Raw, func(ctx context.Context) (nativecodec.CursorBubble, bool, error) {
		row, ok, err := records.Next(ctx)
		if err != nil || !ok {
			return nativecodec.CursorBubble{}, ok, err
		}
		if row.Kind != agentapi.BubbleRecord {
			return nativecodec.CursorBubble{}, false, cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		return nativecodec.CursorBubble{ID: row.Key, Value: row.Raw}, true, nil
	}, 32<<20, owner)
	defer closeFiltered()
	if err != nil {
		return nil, err
	}
	if filtered.Boundary.RetainedBytes < 0 || filtered.Boundary.RetainedBytes > 32<<20 || filtered.LocalIdentity.ID != "" && filtered.LocalIdentity.ID != reg.NativeSessionID {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	inspection, err := Imports().InspectImport(ctx, agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.ID(reg.Harness.Name), NativeID: reg.NativeSessionID}, Source: agentapi.SourceRef{Kind: reg.SourceKind, Key: reg.SourceKey}, Header: agentapi.NativeHeader{NativeID: reg.NativeSessionID, StartedAt: reg.SessionStartedAt}, Filtered: filtered})
	if err != nil {
		return nil, err
	}
	if inspection.IdentityMismatch || !inspection.Conversation {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("settled Cursor conversation changed"))
	}
	bundle, err := archive.NewSourceBundle(reg, filter, filtered, at, nil)
	if err != nil {
		return nil, err
	}
	digest, err := settledCaptureDigest(ctx, owner, bundle, 32<<20)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	return &SettledImportEvidence{capture: &settledCapture{reg: reg, bundle: bundle, facts: SettledImportFacts{Chat: selected, GlobalSHA256: hex.EncodeToString(global.Sum(nil))}, digest: digest, owner: owner, charged: b.charged}}, nil
}

// WithVerifiedCapture lends verified evidence only while its captured owner is live.
// The callback must not retain the borrowed bundle after Close.
func (e *SettledImportEvidence) WithVerifiedCapture(ctx context.Context, reg archive.SessionRegistration, visit func(archive.SourceBundle, SettledImportFacts) error) error {
	if e == nil || e.capture == nil || visit == nil {
		return agentapi.ErrClosed
	}
	c := e.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	original, err := settledCaptureDigest(ctx, c.owner, c.reg, 64<<10)
	if err != nil {
		return err
	}
	requested, err := settledCaptureDigest(ctx, c.owner, reg, 64<<10)
	if err != nil {
		return err
	}
	if original != requested {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("settled capture owner changed"))
	}
	digest, err := settledCaptureDigest(ctx, c.owner, c.bundle, 32<<20)
	if err != nil {
		return err
	}
	if digest != c.digest {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("settled capture bytes changed"))
	}
	return withSettledBundleCopy(ctx, c.owner, c.bundle, func(bundle archive.SourceBundle) error { return visit(bundle, c.facts) })
}

// Close releases this capture once, including when the evidence handle was copied.
func (e *SettledImportEvidence) Close() error {
	if e == nil || e.capture == nil {
		return nil
	}
	c := e.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.bundle = archive.SourceBundle{}
	c.owner.Release(c.charged)
	return nil
}

func settledCaptureDigest(ctx context.Context, owner *settledReservation, value any, limit int64) (string, error) {
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return "", agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := agentmeta.JSONWireBound(ctx, value, min(limit, owner.Available()/2))
	owner.Release(scratch)
	if err != nil {
		return "", err
	}
	if !owner.Reserve(2 * n) {
		return "", agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	defer owner.Release(2 * n)
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), ctx.Err()
}

func rejectCursorMetadataDuplicates(ctx context.Context, raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var readValue func(int) error
	readValue = func(depth int) error {
		if depth > 128 {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		if delimiter == '{' {
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return cursorstore.NotChecked(cursorstore.UnknownFormat)
				}
				normalized := strings.ToLower(name)
				if seen[normalized] {
					return cursorstore.NotChecked(cursorstore.UnknownFormat)
				}
				seen[normalized] = true
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		} else if delimiter == '[' {
			for d.More() {
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		} else {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		_, err = d.Token()
		return err
	}
	if err := readValue(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	return nil
}

// UnmarshalJSON keeps persisted settled facts closed and unambiguous.
func (f *SettledImportFacts) UnmarshalJSON(raw []byte) error {
	if len(raw) > 8<<10 {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	if err := exactSettledFields(raw, []string{"chat", "global_sha256"}); err != nil {
		return err
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal(raw, &outer) != nil {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	if err := exactSettledFields(outer["chat"], []string{"CursorFacts", "ID", "KeyID", "CreatedAt", "Folder", "WorkspaceID", "WorkspaceIdentifier", "Malformed"}); err != nil {
		return err
	}
	var chat map[string]json.RawMessage
	if json.Unmarshal(outer["chat"], &chat) != nil {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	if err := exactSettledFields(chat["CursorFacts"], []string{"VersionPresent", "Version", "Relationships", "ChildIDsSHA256"}); err != nil {
		return err
	}
	type plain SettledImportFacts
	var next plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	*f = SettledImportFacts(next)
	return nil
}
func exactSettledFields(raw []byte, keys []string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if !seen[key] || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
	}
	return nil
}

func withSettledBundleCopy(ctx context.Context, owner *settledReservation, bundle archive.SourceBundle, visit func(archive.SourceBundle) error) error {
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := agentmeta.JSONWireBound(ctx, bundle, min(32<<20, owner.Available()/3))
	owner.Release(scratch)
	if err != nil {
		return err
	}
	if !owner.Reserve(3 * n) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	defer owner.Release(3 * n)
	raw, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	var owned archive.SourceBundle
	if err = json.Unmarshal(raw, &owned); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return visit(owned)
}

type settledReservation struct {
	original *agentapi.NativeReadReservation
	limit    int64
	used     int64
}

func (r *settledReservation) Reserve(n int64) bool {
	if n < 0 || n > r.limit-r.used {
		r.original.Refuse(n)
		return false
	}
	if !r.original.Reserve(n) {
		return false
	}
	r.used += n
	return true
}
func (r *settledReservation) Release(n int64) {
	if n < 0 || n > r.used {
		panic("unbalanced settled reservation")
	}
	r.original.Release(n)
	r.used -= n
}
func (r *settledReservation) Available() int64 { return min(r.original.Available(), r.limit-r.used) }
func (r *settledReservation) Refuse(n int64)   { r.original.Refuse(n) }

func validateSettledMetadata(ctx context.Context, key string, raw []byte) error {
	if err := rejectCursorMetadataDuplicates(ctx, raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	names := []string{"_v", "composerId", "workspaceIdentifier", "subagentComposerIds", "isDraft", "fullConversationHeadersOnly", "conversation", "createdAt"}
	for name := range fields {
		for _, canonical := range names {
			if strings.EqualFold(name, canonical) && name != canonical {
				return cursorstore.NotChecked(cursorstore.UnknownFormat)
			}
		}
	}
	if value, present := fields["composerId"]; present {
		var id string
		if json.Unmarshal(value, &id) != nil || id == "" || id != strings.TrimPrefix(key, "composerData:") {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
	}
	if value, present := fields["workspaceIdentifier"]; present {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		var workspace map[string]json.RawMessage
		if json.Unmarshal(value, &workspace) != nil || workspace == nil {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		for name := range workspace {
			if strings.EqualFold(name, "id") && name != "id" || strings.EqualFold(name, "uri") && name != "uri" {
				return cursorstore.NotChecked(cursorstore.UnknownFormat)
			}
		}
		if raw, present := workspace["id"]; present {
			var id string
			if json.Unmarshal(raw, &id) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return cursorstore.NotChecked(cursorstore.UnknownFormat)
			}
		}
		if raw, present := workspace["uri"]; present {
			if err := validateSettledURI(raw); err != nil {
				return err
			}
		}
	}
	if raw, present := fields["subagentComposerIds"]; present {
		if err := validateSettledRoster(ctx, raw); err != nil {
			return err
		}
	}
	return nil
}

func validateSettledURI(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	names := []string{"scheme", "authority", "fsPath", "path", "external"}
	for name, value := range fields {
		for _, canonical := range names {
			if strings.EqualFold(name, canonical) {
				if name != canonical || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
					return cursorstore.NotChecked(cursorstore.UnknownFormat)
				}
			}
		}
	}
	return nil
}

// validateSettledRoster bounds every prospective ID before the lenient catalog
// decoder allocates its roster; that decoder still owns canonical sorting/hash.
func validateSettledRoster(ctx context.Context, raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	seen := map[string]bool{}
	count := 0
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count == 64 {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		var encoded json.RawMessage
		if decoder.Decode(&encoded) != nil || len(encoded) > 6*255+2 || bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		var id string
		if json.Unmarshal(encoded, &id) != nil || id == "" || len(id) > 255 || seen[id] {
			return cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		seen[id] = true
		count++
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return cursorstore.NotChecked(cursorstore.UnknownFormat)
	}
	return nil
}
