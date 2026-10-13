package catalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
)

// PickerRowBytes bounds the encoded presentation facts independently of metadata.
const PickerRowBytes = 8192

// PickerEnvelopeBytes bounds the compact tree envelope surrounding row facts.
const PickerEnvelopeBytes = 16384

// PickerLabelBytes bounds each UTF-8 presentation label.
const PickerLabelBytes = 256

// PickerRow is a bounded projection, never an authoritative metadata body.
// Child aggregates describe the live index and may change without a revision.
type PickerRow struct {
	Schema           uint64    `json:"Schema"`
	SessionKey       string    `json:"SessionKey"`
	SessionID        string    `json:"SessionID"`
	Harness          string    `json:"Harness"`
	CanonicalHarness string    `json:"CanonicalHarness"`
	Metadata         ObjectRef `json:"Metadata"`
	Revision         string    `json:"Revision"`
	ProjectID        string    `json:"ProjectID"`
	RepoKey          string    `json:"RepoKey"`
	Activity         time.Time `json:"Activity"`
	CapturedAt       time.Time `json:"CapturedAt"`
	Name             string    `json:"Name"`
	Title            string    `json:"Title"`
	ProjectName      string    `json:"ProjectName"`
	Root             bool      `json:"Root"`
	Replay           bool      `json:"Replay"`
	OrdinaryChildren int64     `json:"OrdinaryChildren"`
	ReplayChildren   int64     `json:"ReplayChildren"`
	PromptState      string    `json:"PromptState"`
	PromptCount      *int64    `json:"PromptCount,omitempty"`
}

func pickerLabel(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("invalid picker label UTF-8")
	}
	if len(s) > PickerLabelBytes {
		s = s[:PickerLabelBytes]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s, nil
}

// BuildPickerRow derives facts from full verified metadata. Callers must verify
// the immutable body before using this builder for publication or selection.
func BuildPickerRow(key string, e CatalogEntry) (*PickerRow, error) {
	if e.SummaryOverflow != "" {
		return nil, errors.New("picker facts require full verified metadata")
	}
	m := e.Summary
	rawIdentity, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil || key != rawIdentity {
		return nil, errors.New("picker metadata identity mismatch")
	}
	if e.OrdinaryChildren > math.MaxInt64 || e.ReplayChildren > math.MaxInt64 {
		return nil, errors.New("picker child count overflow")
	}
	r := &PickerRow{Schema: 1, SessionKey: key, SessionID: m.SessionID, Harness: m.Harness.Name, CanonicalHarness: archive.CanonicalHarness(m.Harness.Name), Metadata: e.Metadata, Revision: e.Revision, ProjectID: m.ProjectID, RepoKey: m.RepoKey, Activity: listingindex.ActivityTime(m).UTC(), CapturedAt: m.CapturedAt.UTC(), Root: !m.IsChild(), Replay: m.Replay != nil, OrdinaryChildren: int64(e.OrdinaryChildren), ReplayChildren: int64(e.ReplayChildren), PromptState: "unknown"}
	for _, p := range []struct {
		dst *string
		src string
	}{{&r.Name, m.Name}, {&r.Title, m.Title}, {&r.ProjectName, m.ProjectName}} {
		*p.dst, err = pickerLabel(p.src)
		if err != nil {
			return nil, err
		}
	}
	if m.Counts.Turns != nil {
		n := int64(*m.Counts.Turns)
		r.PromptCount = &n
		r.PromptState = "positive"
		if n == 0 {
			r.PromptState = "zero"
		}
	}
	if err = r.validate(); err != nil {
		return nil, err
	}
	if _, err = PickerMembershipKeys(key, *r); err != nil {
		return nil, err
	}
	if _, err = PickerIdentityKey(*r); err != nil {
		return nil, err
	}
	return r, nil
}

func (r PickerRow) validate() error {
	key, err := archive.MetadataObjectKey(r.Harness, r.SessionID)
	if err != nil || key != r.SessionKey || r.Schema != 1 || r.Revision == "" || r.CanonicalHarness == "" || r.CanonicalHarness != archive.CanonicalHarness(r.Harness) || len(key) > 1024 || r.OrdinaryChildren < 0 || r.ReplayChildren < 0 {
		return errors.New("invalid picker identity, schema, revision or counts")
	}
	for _, s := range []string{r.ProjectID, r.RepoKey} {
		if !utf8.ValidString(s) || len(s) > 512 {
			return errors.New("invalid picker semantic string")
		}
	}
	for _, s := range []string{r.Harness, r.CanonicalHarness, r.SessionID, r.Revision} {
		if !utf8.ValidString(s) || len(s) > 1024 {
			return errors.New("invalid picker identity string")
		}
	}
	if r.RepoKey != "" && !archive.IsRepoKey(r.RepoKey) {
		return errors.New("invalid picker repository key")
	}
	for _, s := range []string{r.Name, r.Title, r.ProjectName} {
		if !utf8.ValidString(s) || len(s) > PickerLabelBytes {
			return errors.New("invalid picker label")
		}
	}
	if err = r.validatePrompts(); err != nil {
		return err
	}
	if err = validateEntryReference(CatalogEntry{Metadata: r.Metadata}); err != nil {
		return err
	}
	raw, err := json.Marshal(r) // #nosec G117 -- SessionKey is archive object identity, never credential material.
	if err != nil {
		return err
	}
	if len(raw) > PickerRowBytes {
		return errors.New("picker row exceeds bounded bytes")
	}
	return nil
}

// UnmarshalJSON refuses unknown fields, formats and oversized facts.
func (r *PickerRow) UnmarshalJSON(raw []byte) error {
	if len(raw) > PickerRowBytes || !utf8.Valid(raw) {
		return errors.New("invalid picker row bytes")
	}
	type plain PickerRow
	var v plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing picker row bytes")
	}
	// Zero values are valid facts, so decoding into the typed row alone cannot
	// distinguish an explicit value from an absent or null wire field.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"Schema", "SessionKey", "SessionID", "Harness", "CanonicalHarness", "Metadata", "Revision", "ProjectID", "RepoKey", "Activity", "CapturedAt", "Name", "Title", "ProjectName", "Root", "Replay", "OrdinaryChildren", "ReplayChildren", "PromptState"} {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("picker row requires explicit non-null facts")
		}
	}
	if value, ok := fields["PromptCount"]; ok && (v.PromptState == "unknown" || bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
		return errors.New("invalid picker prompt count field")
	}
	next := PickerRow(v)
	if err := next.validate(); err != nil {
		return err
	}
	*r = next
	return nil
}

// PickerMembershipKeys returns the complete ordinary scoped membership universe.
// Identity ordering retains the raw harness even when filtering its canonical alias.
func PickerMembershipKeys(key string, r PickerRow) ([]string, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if key != r.SessionKey {
		return nil, errors.New("picker key identity mismatch")
	}
	if !r.Root || r.Replay {
		return nil, nil
	}
	scopes := []string{"global/-"}
	if r.ProjectID != "" {
		scopes = append(scopes, "project-any/"+hex.EncodeToString([]byte(r.ProjectID)))
	}
	if r.RepoKey != "" {
		scopes = append(scopes, "repo/"+hex.EncodeToString([]byte(r.RepoKey)))
	} else if r.ProjectID != "" {
		scopes = append(scopes, "project-missing/"+hex.EncodeToString([]byte(r.ProjectID)))
	}
	memberships := []string{"browse"}
	if r.PromptState != "zero" {
		memberships = append(memberships, "handoff")
	}
	// MetadataObjectKey permits no NUL in either raw component. Terminators
	// preserve shorter-component ordering before inversion, unlike path slashes
	// which can sort after a continuation byte such as '-'. The exact raw tuple
	// uniquely determines the physical metadata identity.
	identity := descendingIdentity(r.Harness + "\x00" + r.SessionID + "\x00")
	order := r.Activity.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + r.CapturedAt.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + identity
	var keys []string
	for _, m := range memberships {
		for _, scope := range scopes {
			for _, h := range []string{"all", hex.EncodeToString([]byte(r.CanonicalHarness))} {
				k := "!picker/v1/" + m + "/" + scope + "/" + h + "/" + order
				if len(k) > 4096 {
					return nil, errors.New("picker key exceeds structural bound")
				}
				keys = append(keys, k)
			}
		}
	}
	return keys, nil
}

// DecodePickerRow validates a picker leaf envelope and exact membership key.
// It requires no metadata-body reads.
func DecodePickerRow(key string, raw []byte) (*PickerRow, error) {
	if len(raw) > PickerEnvelopeBytes || !pickerTreeKey(key) {
		return nil, errors.New("invalid picker namespace")
	}
	var e CatalogEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	if e.Picker == nil || e.SummaryOverflow != overflowMetadata {
		return nil, errors.New("missing picker facts")
	}
	if strings.HasPrefix(key, "!picker-identity/") {
		identity, err := PickerIdentityKey(*e.Picker)
		if err != nil {
			return nil, err
		}
		if identity != key {
			return nil, errors.New("picker canonical identity key mismatch")
		}
		return e.Picker, nil
	}
	keys, err := PickerMembershipKeys(e.Picker.SessionKey, *e.Picker)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k == key {
			return e.Picker, nil
		}
	}
	return nil, errors.New("picker membership key mismatch")
}

func (r PickerRow) validatePrompts() error {
	switch r.PromptState {
	case "unknown":
		if r.PromptCount != nil {
			return errors.New("unknown picker prompts carry count")
		}
	case "zero":
		if r.PromptCount == nil || *r.PromptCount != 0 {
			return errors.New("invalid zero picker prompts")
		}
	case "positive":
		if r.PromptCount == nil || *r.PromptCount <= 0 {
			return errors.New("invalid positive picker prompts")
		}
	default:
		return errors.New("unknown picker prompt state")
	}
	return nil
}

func samePickerRow(a, b PickerRow) bool {
	if !a.Activity.Equal(b.Activity) || !a.CapturedAt.Equal(b.CapturedAt) || (a.PromptCount == nil) != (b.PromptCount == nil) {
		return false
	}
	if a.PromptCount != nil && *a.PromptCount != *b.PromptCount {
		return false
	}
	a.Activity = b.Activity
	a.CapturedAt = b.CapturedAt
	a.PromptCount = nil
	b.PromptCount = nil
	return a == b
}

// PickerIdentityRange addresses all physical raw identities for one canonical
// harness/session association. The exclusive upper bound includes the complete
// trailing-slash boundary; callers must treat multiple entries as ambiguity.
func PickerIdentityRange(harness, sessionID string) (lower, upper string, err error) {
	if !utf8.ValidString(harness) || !utf8.ValidString(sessionID) {
		return "", "", errors.New("invalid picker canonical identity UTF-8")
	}
	canonical := archive.CanonicalHarness(harness)
	identity, e := archive.MetadataObjectKey(canonical, sessionID)
	if e != nil || canonical == "" || len(identity) > 1024 {
		return "", "", errors.New("invalid picker canonical identity")
	}
	lower = "!picker-identity/v1/" + hex.EncodeToString([]byte(canonical)) + "/" + hex.EncodeToString([]byte(sessionID)) + "/"
	return lower, strings.TrimSuffix(lower, "/") + "0", nil
}

// PickerIdentityKey preserves one exact raw identity within its canonical group.
// Every live entry receives this copy, independent of browse eligibility/scope.
func PickerIdentityKey(row PickerRow) (string, error) {
	if err := row.validate(); err != nil {
		return "", err
	}
	lower, _, err := PickerIdentityRange(row.CanonicalHarness, row.SessionID)
	if err != nil {
		return "", err
	}
	key := lower + hex.EncodeToString([]byte(row.SessionKey))
	if len(key) > 4096 {
		return "", errors.New("picker canonical identity key exceeds structural bound")
	}
	return key, nil
}

func pickerTreeKey(key string) bool {
	return strings.HasPrefix(key, "!picker/") || strings.HasPrefix(key, "!picker-identity/")
}
