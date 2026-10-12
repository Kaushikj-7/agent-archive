package catalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

const overflowMetadata = "metadata-v10"

// Half a node reserves the remaining space for escaped keys, record wrappers,
// and siblings. The actual serialized leaf is measured for every update.
const inlineLeafBytes = 64 << 10

func indexProjection(m archive.Metadata) archive.Metadata {
	origin, started := archive.SessionOrigin(""), time.Time{}
	if m.Origin == archive.SessionOriginImport {
		origin, started = m.Origin, m.StartedAt
	}
	var replay *archive.Replay
	if m.Replay != nil {
		replay = &archive.Replay{}
	}
	p := archive.Metadata{Replay: replay, Origin: origin, StartedAt: started, SchemaVersion: m.SchemaVersion, SessionID: m.SessionID, Harness: archive.Harness{Name: m.Harness.Name}, ProjectID: m.ProjectID, CapturedAt: m.CapturedAt, EndedAt: m.EndedAt, ParentSessionID: m.ParentSessionID, NativeChild: m.NativeChild}
	return p
}

func sameMetadata(a, b archive.Metadata) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}

func validateEntryReference(e CatalogEntry) error {
	hash, err := hex.DecodeString(e.Metadata.SHA256)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != e.Metadata.SHA256 || e.Metadata.Key != "catalog-v4/metadata/"+e.Metadata.SHA256+".json" {
		return errors.New("catalog metadata requires its immutable checksum key")
	}
	if e.SummaryOverflow != "" && e.SummaryOverflow != overflowMetadata {
		return errors.New("unknown catalog entry overflow marker")
	}
	return nil
}

// UnmarshalJSON strictly decodes inline or explicitly marked tree entries.
func (e *CatalogEntry) UnmarshalJSON(raw []byte) error {
	type plain CatalogEntry
	var next plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing catalog entry content")
	}
	*e = CatalogEntry(next)
	if err := validateEntryReference(*e); err != nil {
		return err
	}
	if e.SummaryOverflow != "" && !sameMetadata(e.Summary, indexProjection(e.Summary)) {
		return errors.New("overflow entry contains full or mixed summary fields")
	}
	return nil
}

func compactEntry(e *CatalogEntry) *CatalogEntry {
	if e == nil {
		return nil
	}
	result := *e
	result.SummaryOverflow = overflowMetadata
	result.Summary = indexProjection(e.Summary)
	return &result
}

func compactTreeValue(value any) any {
	switch v := value.(type) {
	case record:
		v.Entry = compactEntry(v.Entry)
		return v
	case *CatalogEntry:
		return compactEntry(v)
	case CatalogEntry:
		return compactEntry(&v)
	default:
		return value
	}
}

func entryTreeValue(value any) bool {
	switch v := value.(type) {
	case record:
		return v.Entry != nil
	case *CatalogEntry:
		return v != nil
	case CatalogEntry:
		return true
	default:
		return false
	}
}

func encodeTreeValue(key string, value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	leaf, err := json.Marshal(item{Key: key, Value: raw})
	if err != nil {
		return nil, err
	}
	if !entryTreeValue(value) || len(leaf) <= inlineLeafBytes {
		return raw, nil
	}
	raw, err = json.Marshal(compactTreeValue(value))
	if err != nil {
		return nil, err
	}
	leaf, err = json.Marshal(item{Key: key, Value: raw})
	if err != nil {
		return nil, err
	}
	if len(leaf) > inlineLeafBytes {
		return nil, errors.New("catalog indexed entry exceeds bounded envelope")
	}
	return raw, nil
}

// MatchMetadata validates an authoritative decoded body against this entry.
// Overflow projections certify only index fields; the immutable body supplies
// all remaining fields and must already have its exact reference hash checked.
func (e CatalogEntry) MatchMetadata(m archive.Metadata) error {
	if err := validateEntryReference(e); err != nil {
		return err
	}
	expected, actual := e.Summary, m
	if e.SummaryOverflow != "" {
		expected = indexProjection(expected)
		actual = indexProjection(actual)
	}
	if !sameMetadata(expected, actual) {
		return errors.New("catalog selected body differs from summary")
	}
	return nil
}

func decodeEntryBody(entry CatalogEntry, raw []byte) (archive.Metadata, error) {
	var m archive.Metadata
	if err := validateEntryReference(entry); err != nil {
		return m, err
	}
	if !storage.VerifySHA256(raw, entry.Metadata.SHA256) {
		return m, storage.ErrChecksumMismatch
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil || key != leafSessionKey(&entry) {
		return m, errors.New("catalog metadata identity mismatch")
	}
	return m, entry.MatchMetadata(m)
}
