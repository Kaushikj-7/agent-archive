// Package search projects verified retained conversation text into a disposable
// local index. It owns neither publication nor native transcript interpretation.
package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ExtractorVersion changes whenever passage visibility or addressing changes.
const ExtractorVersion = "1"

const passageBytes = 8192

const overlapBytes = 1024

// maxSessionBytes bounds compressed membership and projected text per session.
const maxSessionBytes = 32 << 20

const maxSessionPassages = 100000

// Source identifies both exact bytes and their interpretation, independently of
// display metadata. Destination must include the installation/privacy context.
type Source struct {
	Destination      string                  `json:"Destination"`
	SessionID        string                  `json:"SessionID"`
	RevisionID       string                  `json:"RevisionID"`
	Reference        archive.SourceReference `json:"Reference"`
	ParserVersion    string                  `json:"ParserVersion"`
	FilterVersion    string                  `json:"FilterVersion"`
	SchemaVersion    int                     `json:"SchemaVersion"`
	ExtractorVersion string                  `json:"ExtractorVersion"`
}

// Key binds reuse to ownership and interpretation, not just the byte checksum.
func (s Source) Key() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Reference addresses byte bounds in a normalized turn's presentation text.
// Record is a retained native record index, never a rendered line number.
type Reference struct {
	Source Source `json:"Source"`
	Record int    `json:"Record"`
	Start  int    `json:"Start"`
	End    int    `json:"End"`
}

// Passage is a bounded projection; Physical identifies a proven history record
// for within-session deduplication. Empty Physical never authorizes text dedup.
type Passage struct {
	Reference Reference `json:"Reference"`
	Role      string    `json:"Role"`
	Text      string    `json:"Text"`
	Physical  string    `json:"Physical"`
}

// Gap records unsupported retained coverage without storing source content.
type Gap struct {
	RevisionID string `json:"RevisionID"`
	Reason     string `json:"Reason"`
}

// Projection contains one source's disposable searchable passages.
type Projection struct {
	Source   Source    `json:"Source"`
	Passages []Passage `json:"Passages"`
	Gap      *Gap      `json:"Gap"`
}

// Extract projects an already interpreted verified bundle. The caller supplies
// the parser version that produced analysis. Native parsing stays in agentapi.
func Extract(source Source, bundle archive.SourceBundle, analysis archive.Analysis) (Projection, error) {
	out := Projection{Source: source}
	if err := archive.ValidateSourceBundle(bundle); err != nil {
		return out, err
	}
	if source.SessionID != bundle.ArchiveSessionID || source.FilterVersion != bundle.Capture.FilterVersion || source.SchemaVersion != bundle.SchemaVersion || source.ExtractorVersion != ExtractorVersion {
		return out, errors.New("search projection provenance disagrees with verified source")
	}
	if historicalVisibilityUnavailable(bundle.Capture.FilterVersion) {
		out.Gap = &Gap{source.RevisionID, "historical_visibility"}
		return out, nil
	}
	if analysis.Facts.TextOnly {
		out.Gap = &Gap{source.RevisionID, "unstructured_text"}
		return out, nil
	}
	seen := map[string]bool{}
	totalText := 0
	for _, turn := range analysis.View.Turns {
		if turn.Kind != archive.TurnKindHumanPrompt && turn.Kind != archive.TurnKindAssistant {
			continue
		}
		if turn.RecordIndex < 0 || turn.RecordIndex >= len(bundle.NativeRecords) {
			return out, errors.New("search turn outside retained source")
		}
		text := turn.Text
		if turn.PresentationKnown {
			text = turn.PresentationText
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if !utf8.ValidString(text) {
			return out, errors.New("search turn is not UTF-8")
		}
		identity := turn.MessageID
		if identity == "" {
			identity = turn.ID
		}
		if identity != "" {
			digest := sha256.Sum256([]byte(text))
			key := turn.Role + "\x00" + identity + "\x00" + hex.EncodeToString(digest[:])
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		physical := ""
		if span, ok := bundle.History.SpanAt(turn.RecordIndex); ok {
			physical = fmt.Sprintf("%s/%s/%d", span.ThreadID, span.RolloutID, bundle.Ordinals[turn.RecordIndex])
		}
		for start := 0; start < len(text); {
			end := min(start+passageBytes, len(text))
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end--
			}
			if len(out.Passages) >= maxSessionPassages || end-start > maxSessionBytes-totalText {
				return out, storage.ErrObjectTooLarge
			}
			totalText += end - start
			out.Passages = append(out.Passages, Passage{Reference: Reference{source, turn.RecordIndex, start, end}, Role: turn.Role, Text: text[start:end], Physical: physical})
			if end == len(text) {
				break
			}
			start = end - overlapBytes
			for !utf8.RuneStart(text[start]) {
				start++
			}
		}
	}
	return out, nil
}

// LoadProjection reads exactly one referenced active or preserved source through
// reader's bounded checksum/identity verification before selecting its parser.
func LoadProjection(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, destination string, metadata archive.Metadata, revisionID string, limits reader.Limits) (Projection, error) {
	source := Source{Destination: destination, SessionID: metadata.SessionID, RevisionID: revisionID, Reference: metadata.SourceBundle, ExtractorVersion: ExtractorVersion}
	var bundle archive.SourceBundle
	var err error
	if metadata.History != nil {
		revisions, e := reader.Revisions(metadata)
		if e != nil {
			return Projection{}, e
		}
		found := false
		for _, r := range revisions {
			if r.RevisionID == revisionID {
				source.Reference = r.Source
				found = true
				break
			}
		}
		if !found {
			return Projection{}, ErrChanged
		}
		bundle, err = reader.LoadRevision(ctx, objects, metadata, revisionID, limits)
	} else {
		if revisionID != "" {
			return Projection{}, ErrChanged
		}
		bundle, err = reader.LoadSource(ctx, objects, metadata, limits)
	}
	if err != nil {
		return Projection{}, err
	}
	source.FilterVersion = bundle.Capture.FilterVersion
	source.SchemaVersion = bundle.SchemaVersion
	if parsers == nil {
		return Projection{Source: source, Gap: &Gap{revisionID, "parser_unavailable"}}, nil
	}
	parser, ok := parsers.LookupParser(bundle.Capture.Harness.Name)
	if !ok {
		return Projection{Source: source, Gap: &Gap{revisionID, "parser_unavailable"}}, nil
	}
	source.ParserVersion = parser.Version()
	analysis, err := agentapi.Analyze(ctx, parser, bundle)
	if err != nil {
		if ctx.Err() != nil {
			return Projection{}, ctx.Err()
		}
		return Projection{Source: source, Gap: &Gap{revisionID, "parse_failed"}}, nil
	}
	return Extract(source, bundle, analysis)
}

// Source-bound context failures distinguish observed membership change, deletion,
// unsupported interpretation, and temporarily unavailable retained bytes.
var (
	ErrChanged     = errors.New("search source or extraction changed; refresh search")
	ErrDeleted     = errors.New("search session deleted")
	ErrUnsupported = errors.New("search source interpretation unsupported")
	ErrUnavailable = errors.New("search source unavailable")
)

// Context contains a bounded byte interval within the referenced passage.
// Next is an absolute byte offset; zero means the passage is complete.
type Context struct {
	Text  string `json:"Text"`
	Start int    `json:"Start"`
	End   int    `json:"End"`
	Next  int    `json:"Next"`
}

// ContextSlice retains exact addressing for a neighboring visible message.
type ContextSlice struct {
	Reference Reference
	Role      string
	Context   Context
}

// Surrounding contains the selected passage and bounded same-source neighbors.
type Surrounding struct {
	Role          string
	Selected      Context
	Before, After []ContextSlice
}

// ReadContext verifies and interprets one source once for all requested slices.
func ReadContext(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, ref Reference, offset, maxBytes, before, after int, limits reader.Limits) (Surrounding, error) {
	out := Surrounding{Before: []ContextSlice{}, After: []ContextSlice{}}
	if maxBytes < 4 || maxBytes > passageBytes || offset < ref.Start || offset > ref.End || before < 0 || before > 2 || after < 0 || after > 2 {
		return out, errors.New("invalid search context bounds")
	}
	p, err := verifiedReferenceProjection(ctx, objects, parsers, ref, limits)
	if err != nil {
		return out, err
	}
	selected := -1
	for i, passage := range p.Passages {
		if passage.Reference == ref {
			selected = i
			break
		}
	}
	if selected < 0 {
		return out, ErrChanged
	}
	out.Role = p.Passages[selected].Role
	out.Selected, err = sliceContext(p.Passages[selected], offset, maxBytes)
	if err != nil {
		return out, err
	}
	seen := map[int]bool{ref.Record: true}
	for i := selected - 1; i >= 0 && len(out.Before) < before; i-- {
		v := p.Passages[i]
		if seen[v.Reference.Record] {
			continue
		}
		seen[v.Reference.Record] = true
		// Return the first chunk of the neighboring message, preserving continuation.
		for i > 0 && p.Passages[i-1].Reference.Record == v.Reference.Record {
			i--
			v = p.Passages[i]
		}
		c, e := sliceContext(v, v.Reference.Start, maxBytes)
		if e != nil {
			return out, e
		}
		out.Before = append([]ContextSlice{{v.Reference, v.Role, c}}, out.Before...)
	}
	for i := selected + 1; i < len(p.Passages) && len(out.After) < after; i++ {
		v := p.Passages[i]
		if seen[v.Reference.Record] {
			continue
		}
		seen[v.Reference.Record] = true
		c, e := sliceContext(v, v.Reference.Start, maxBytes)
		if e != nil {
			return out, e
		}
		out.After = append(out.After, ContextSlice{v.Reference, v.Role, c})
	}
	return out, nil
}

func sliceContext(passage Passage, offset, maxBytes int) (Context, error) {
	local := offset - passage.Reference.Start
	if local < 0 || local > len(passage.Text) || (local < len(passage.Text) && !utf8.RuneStart(passage.Text[local])) {
		return Context{}, errors.New("search offset splits UTF-8 or is outside passage")
	}
	end := min(local+maxBytes, len(passage.Text))
	for end < len(passage.Text) && !utf8.RuneStart(passage.Text[end]) {
		end--
	}
	out := Context{Text: passage.Text[local:end], Start: offset, End: passage.Reference.Start + end}
	if out.End < passage.Reference.End {
		out.Next = out.End
	}
	return out, nil
}

func verifiedReferenceProjection(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, ref Reference, limits reader.Limits) (Projection, error) {
	// Harness is not encoded in opaque session IDs; source keys carry the canonical
	// metadata sibling and are validated again by reader and projection identity.
	slash := strings.LastIndex(ref.Source.Reference.Key, "/")
	if slash < 0 {
		return Projection{}, ErrChanged
	}
	m, err := reader.ReadMetadata(ctx, objects, ref.Source.Reference.Key[:slash]+"/metadata.json")
	if errors.Is(err, storage.ErrNotFound) {
		return Projection{}, ErrDeleted
	}
	if err != nil {
		return Projection{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if m.SessionID != ref.Source.SessionID {
		return Projection{}, ErrChanged
	}
	refs, e := m.SourceReferences()
	if e != nil {
		return Projection{}, fmt.Errorf("%w: %w", ErrUnavailable, e)
	}
	if !slices.Contains(refs, ref.Source.Reference) {
		return Projection{}, ErrChanged
	}
	p, err := LoadProjection(ctx, objects, parsers, ref.Source.Destination, m, ref.Source.RevisionID, limits)
	if errors.Is(err, ErrChanged) {
		return Projection{}, err
	}
	if err != nil {
		return Projection{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if p.Gap != nil {
		return Projection{}, ErrUnsupported
	}
	if p.Source != ref.Source {
		return Projection{}, ErrChanged
	}

	return p, nil
}

func historicalVisibilityUnavailable(version string) bool {
	filterVersion, err := strconv.Atoi(version)
	if err != nil {
		return true
	}
	currentFilterVersion, _ := strconv.Atoi(archive.FilterVersion)
	return filterVersion < 6 || filterVersion > currentFilterVersion
}
