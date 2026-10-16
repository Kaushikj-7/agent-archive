package archive

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CompressedSource is the exact immutable byte sequence to persist and retry.
// SHA256 is over Bytes, never an object-store ETag.
type CompressedSource struct {
	Bytes  []byte
	SHA256 string
}

// NewSourceBundle converts only adapter-filtered records into the durable
// envelope. A caller cannot accidentally add a raw transcript through this API.
func NewSourceBundle(reg SessionRegistration, adapter Adapter, transcript FilteredTranscript, capturedAt time.Time, supplemental []SupplementalEvidence) (SourceBundle, error) {
	if err := reg.Validate(); err != nil {
		return SourceBundle{}, err
	}
	if adapter == nil {
		return SourceBundle{}, errors.New("adapter is required")
	}
	if capturedAt.IsZero() {
		return SourceBundle{}, errors.New("captured_at is required and must be reused for retries")
	}
	if transcript.Format == "" {
		return SourceBundle{}, errors.New("filtered transcript format is required")
	}
	records := make([]map[string]any, 0, len(transcript.Records))
	for index, raw := range transcript.Records {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return SourceBundle{}, fmt.Errorf("filtered record %d is invalid: %w", index, err)
		}
		if len(record) == 0 {
			return SourceBundle{}, fmt.Errorf("filtered record %d is empty", index)
		}
		records = append(records, record)
	}
	nativeText := make([]TextTranscript, 0, len(transcript.Text))
	for _, text := range transcript.Text {
		if text != "" {
			nativeText = append(nativeText, TextTranscript{Format: transcript.Format, Content: text})
		}
	}
	if len(records) == 0 && len(nativeText) == 0 {
		return SourceBundle{}, errors.New("filtered transcript has no retained evidence")
	}
	discoveryOrigin := SessionOrigin("")
	if reg.Origin == SessionOriginDiscovery {
		discoveryOrigin = reg.Origin
	}
	harness := reg.Harness
	if transcript.ObservedHarness.Version != "" {
		harness.Version = transcript.ObservedHarness.Version
	}
	if transcript.ObservedHarness.Mode != "" {
		harness.Mode = transcript.ObservedHarness.Mode
	}
	for _, evidence := range supplemental {
		if evidence.Kind == EvidenceKindSessionLabels {
			label, ok := labelFromEvidence(evidence)
			if !ok || evidence.Provenance != labelProvenance(reg.Harness.Name) || label.NativeID != reg.NativeSessionID {
				return SourceBundle{}, errors.New("session label does not match its owning source")
			}
		}
	}
	filteredSupplemental, gaps, err := FilterSupplementalEvidence(supplemental)
	if err != nil {
		return SourceBundle{}, err
	}
	allGaps := append(append([]CaptureGap(nil), transcript.Gaps...), gaps...)
	if reg.Origin == SessionOriginDiscovery {
		allGaps = append(allGaps, CaptureGap{Code: CaptureGapDiscoveredWithoutHookEvidence, Detail: "Discovered locally; hooks did not establish complete lifecycle coverage"})
	}
	if reg.PreviousGenerationID != "" {
		allGaps = append(allGaps, CaptureGap{Code: "recovered_generation", Detail: "Starts from the current native transcript; earlier retained history belongs to the previous archive generation"})
	}
	for _, item := range filteredSupplemental {
		if item.Kind == EvidenceKindCaptureGap {
			if code := firstString(item.Payload, "code"); code != "" {
				allGaps = append(allGaps, CaptureGap{Code: code, Detail: firstString(item.Payload, "detail")})
			}
		}
	}
	if reg.NativeChild && reg.ParentSessionID == "" {
		allGaps = append(allGaps, CaptureGap{Code: "native_parent_link_pending", Detail: "Native child captured independently; parent archive link is unresolved"})
	}
	schema := SourceSchemaVersion
	if transcript.History != nil {
		schema = HistorySourceSchemaVersion
	}
	return SourceBundle{
		History: transcript.History, Ordinals: transcript.Ordinals,
		SchemaVersion:    schema,
		ArchiveSessionID: reg.ArchiveSessionID,
		NativeSessionID:  reg.NativeSessionID,
		ProjectID:        reg.ProjectID,
		Capture: SourceCapture{
			Origin:  discoveryOrigin,
			Harness: harness, AdapterName: adapter.Name(), AdapterVersion: adapter.Version(),
			SourceFormat: transcript.Format, Boundary: transcript.Boundary,
			FilterVersion: FilterVersion, CapturedAt: capturedAt.UTC(), Gaps: allGaps,
		},
		NativeRecords: records, NativeText: nativeText, SupplementalEvidence: filteredSupplemental,
		PreviousGenerationID: reg.PreviousGenerationID, NativeChild: reg.NativeChild, ParentSessionID: reg.ParentSessionID, LinkedSessions: deriveLinkedSessions(filteredSupplemental),
	}, nil
}

func deriveLinkedSessions(evidence []SupplementalEvidence) []LinkedSessionReference {
	unverified := map[string]bool{}
	for _, item := range evidence {
		if item.Kind == EvidenceKindLinkedSession && item.Provenance == NativeLegacyUnverifiedLinkProvenance {
			unverified[firstString(item.Payload, "archive_session_id")] = true
		}
	}
	latest := map[string]LinkedSessionReference{}
	for _, item := range evidence {
		if item.Kind != EvidenceKindLinkedSession {
			continue
		}
		id := firstString(item.Payload, "archive_session_id")
		if unverified[id] {
			continue
		}
		relationship := firstString(item.Payload, "relationship")
		status := LinkedSessionStatus(firstString(item.Payload, "status"))
		if id == "" || relationship != "subagent" || (status != LinkedSessionPending && status != LinkedSessionPublished && status != LinkedSessionUnavailable) {
			continue
		}
		candidate := LinkedSessionReference{SessionID: id, Relationship: relationship, Status: status, ObservedAt: item.ObservedAt.UTC()}
		prior, found := latest[id]
		if !found || candidate.ObservedAt.After(prior.ObservedAt) || (candidate.ObservedAt.Equal(prior.ObservedAt) && linkedStatusRank(candidate.Status) > linkedStatusRank(prior.Status)) {
			latest[id] = candidate
		}
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]LinkedSessionReference, 0, len(ids))
	for _, id := range ids {
		out = append(out, latest[id])
	}
	return out
}

// NewLinkedSessionEvidence constructs and filters the archive-generated link
// marker shared by hooks and the background collector.
func NewLinkedSessionEvidence(sessionID string, status LinkedSessionStatus, observedAt time.Time) (SupplementalEvidence, error) {
	if sessionID == "" || observedAt.IsZero() || (status != LinkedSessionPending && status != LinkedSessionPublished && status != LinkedSessionUnavailable) {
		return SupplementalEvidence{}, errors.New("linked session evidence is incomplete")
	}
	filtered, _, err := FilterSupplementalEvidence([]SupplementalEvidence{{
		Kind: EvidenceKindLinkedSession, ObservedAt: observedAt,
		Provenance: "hook:subagent-link", Payload: map[string]any{
			"archive_session_id": sessionID, "relationship": "subagent", "status": string(status),
		},
	}})
	if err != nil {
		return SupplementalEvidence{}, err
	}
	if len(filtered) != 1 {
		return SupplementalEvidence{}, errors.New("linked session evidence was not retained")
	}
	return filtered[0], nil
}

func linkedStatusRank(status LinkedSessionStatus) int {
	switch status {
	case LinkedSessionPublished:
		return 3
	case LinkedSessionUnavailable:
		return 2
	case LinkedSessionPending:
		return 1
	default:
		return 1
	}
}

// FilterSupplementalEvidence applies the same strict allowlist and secret
// redaction policy used for native records. Hooks must call it (or use
// NewSourceBundle, which calls it) before persisting upload-ready evidence.
func FilterSupplementalEvidence(in []SupplementalEvidence) ([]SupplementalEvidence, []CaptureGap, error) {
	out := make([]SupplementalEvidence, 0, len(in))
	var gaps []CaptureGap
	for _, evidence := range in {
		if strings.TrimSpace(string(evidence.Kind)) == "" || strings.TrimSpace(evidence.Provenance) == "" || evidence.ObservedAt.IsZero() {
			return nil, nil, errors.New("supplemental evidence requires kind, provenance, and observation time")
		}
		if evidence.Kind == EvidenceKindSessionLabels {
			label, ok := labelFromEvidence(evidence)
			if !ok {
				return nil, nil, errors.New("invalid session label evidence")
			}
			out = append(out, label.Evidence(evidence.ObservedAt.UTC(), labelEvidenceHarness(evidence)))
			continue
		}
		var extraAllowed map[string]bool
		if evidence.Kind == EvidenceKindCaptureGap {
			extraAllowed = captureGapKeys
		}
		state := PrivacyState{
			AddGap: func(code string, _ int, detail string) {
				gaps = append(gaps, CaptureGap{Code: code, Detail: "supplemental " + detail})
			},
			ExtraAllowed: extraAllowed,
		}
		payload, keep := sanitizeObject(evidence.Payload, &state)
		if !keep {
			gaps = append(gaps, CaptureGap{Code: "supplemental_evidence_omitted", Detail: "no allowed fields"})
			continue
		}
		if evidence.Kind == EvidenceKindFinalResponse && firstString(payload, "agent_id") != "" {
			gaps = append(gaps, CaptureGap{Code: "subagent_final_not_reconciled", Detail: "separate subagent source required"})
		}
		out = append(out, SupplementalEvidence{Kind: evidence.Kind, ObservedAt: evidence.ObservedAt.UTC(), Provenance: evidence.Provenance, Payload: payload})
	}
	return out, gaps, nil
}

// AnnotateSupplementalGaps records what FilterSupplementalEvidence did to a
// producer's evidence on that evidence itself. Producers filter before
// persistence, so the later pass in NewSourceBundle sees already-clean input
// and cannot report these gaps in Capture.Gaps. "redacted" and "truncated"
// are set only by their own codes; "gaps" lists every distinct code once.
func AnnotateSupplementalGaps(payload map[string]any, gaps []CaptureGap) {
	if payload == nil || len(gaps) == 0 {
		return
	}
	seen := map[string]bool{}
	codes := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		if gap.Code == "" || seen[gap.Code] {
			continue
		}
		seen[gap.Code] = true
		codes = append(codes, gap.Code)
		//lint:ignore LV1001 CaptureGap.Code is also set from evidence payloads and by internal/cli, so it stays a plain string
		switch gap.Code {
		case "sensitive_content_redacted":
			payload["redacted"] = true
		case "content_truncated":
			payload["truncated"] = true
		}
	}
	sort.Strings(codes)
	list := make([]any, 0, len(codes))
	for _, code := range codes {
		list = append(list, code)
	}
	payload["gaps"] = list
}

// MergeSupplementalEvidence combines observations without allowing the time
// of a repeated background scan to manufacture a new source snapshot.
// Inventories form a history: a changed inventory is appended with its actual
// observation time, while an unchanged latest inventory is omitted. Skill
// snapshots retain every distinct filtered/original-hash version but do not
// repeat identical bytes. Other evidence is event-shaped and remains
// append-only, with exact retry duplicates removed.
func MergeSupplementalEvidence(previous, fresh []SupplementalEvidence) []SupplementalEvidence {
	out := append([]SupplementalEvidence(nil), previous...)
	for _, candidate := range fresh {
		switch candidate.Kind {
		case EvidenceKindSessionLabels:
			replaced := false
			for i := range out {
				if out[i].Kind == EvidenceKindSessionLabels && firstString(out[i].Payload, "native_session_id") == firstString(candidate.Payload, "native_session_id") {
					if !supplementalPayloadEqual(out[i].Payload, candidate.Payload) {
						out[i] = candidate
					}
					replaced = true
					break
				}
			}
			if !replaced {
				out = append(out, candidate)
			}
		case EvidenceKindSkillInventory:
			identity := supplementalIdentity(candidate)
			unchanged := false
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].Kind == EvidenceKindSkillInventory && supplementalIdentity(out[i]) == identity {
					unchanged = supplementalPayloadEqual(out[i].Payload, candidate.Payload)
					break
				}
			}
			if !unchanged {
				out = append(out, candidate)
			}
		case EvidenceKindSkillSnapshot:
			duplicate := false
			for _, existing := range out {
				if existing.Kind == EvidenceKindSkillSnapshot && supplementalIdentity(existing) == supplementalIdentity(candidate) && supplementalPayloadEqual(existing.Payload, candidate.Payload) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				out = append(out, candidate)
			}
		case EvidenceKindSkillDiscovered, EvidenceKindSkillInvocation, EvidenceKindSkillRead,
			EvidenceKindLifecycleHook, EvidenceKindFinalResponse, EvidenceKindExplicitFeedback,
			EvidenceKindLinkedSession, EvidenceKindCaptureGap:
			// Every other kind, and any added later, dedupes on its full value.
			fallthrough
		default:
			duplicate := false
			for _, existing := range out {
				if supplementalEvidenceEqual(existing, candidate) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				out = append(out, candidate)
			}
		}
	}
	return out
}

// SupplementalEvidenceEqual reports whether two evidence items are the same
// observation. Callers that accumulate durable hook evidence use it to drop
// exact repeats, which mergeSupplementalEvidence would discard at publication
// time anyway, before they can grow a pending request without bound.
func SupplementalEvidenceEqual(a, b SupplementalEvidence) bool {
	return supplementalEvidenceEqual(a, b)
}

func supplementalEvidenceEqual(a, b SupplementalEvidence) bool {
	return a.Kind == b.Kind && a.Provenance == b.Provenance && a.ObservedAt.Equal(b.ObservedAt) && supplementalPayloadEqual(a.Payload, b.Payload)
}

func supplementalIdentity(e SupplementalEvidence) string {
	identity := string(e.Kind) + "\x00" + e.Provenance
	if e.Kind == EvidenceKindSkillSnapshot {
		identity += "\x00" + firstString(e.Payload, "name") + "\x00" + firstString(e.Payload, "scope")
	} else {
		identity += "\x00" + firstString(e.Payload, "coverage") + "\x00" + firstString(e.Payload, "scope")
	}
	return identity
}

func supplementalPayloadEqual(a, b map[string]any) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}

// BuildCompressedSource serializes a bundle canonically as schema-2 JSONL
// (see EncodeSource) and uses gzip headers that are independent of the wall
// clock and host platform, so identical evidence yields identical bytes and
// the same SHA-256. Lines are streamed into the compressor; the uncompressed
// document is never held whole.
func BuildCompressedSource(bundle SourceBundle) (CompressedSource, error) {
	if err := validateBundle(bundle); err != nil {
		return CompressedSource{}, err
	}
	var output bytes.Buffer
	if err := CompressSource(&output, bundle); err != nil {
		return CompressedSource{}, err
	}
	bytes := output.Bytes()
	digest := sha256.Sum256(bytes)
	return CompressedSource{Bytes: append([]byte(nil), bytes...), SHA256: hex.EncodeToString(digest[:])}, nil
}

// CompressSource streams the canonical deterministic source gzip to output.
// The caller owns output's allocation and lifetime; errors publish no authority.
func CompressSource(output io.Writer, bundle SourceBundle) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	writer, err := gzip.NewWriterLevel(output, gzip.DefaultCompression)
	if err != nil {
		return err
	}
	// A non-zero epoch avoids gzip's special "unknown time" representation
	// while remaining independent of capture and wall-clock time.
	writer.ModTime = time.Unix(1, 0).UTC()
	writer.OS = 255
	if err = EncodeSource(writer, bundle); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	return nil
}

func validateBundle(bundle SourceBundle) error {
	if err := ValidateSessionLabels(bundle); err != nil {
		return err
	}
	if err := bundle.ValidateHistory(); err != nil {
		return err
	}
	if bundle.SchemaVersion != SourceSchemaVersion && bundle.SchemaVersion != HistorySourceSchemaVersion {
		return fmt.Errorf("unsupported source schema version %d", bundle.SchemaVersion)
	}
	if strings.TrimSpace(bundle.ArchiveSessionID) == "" || strings.TrimSpace(bundle.NativeSessionID) == "" || strings.TrimSpace(bundle.ProjectID) == "" {
		return errors.New("source bundle IDs are required")
	}
	if strings.TrimSpace(bundle.Capture.AdapterName) == "" || bundle.Capture.CapturedAt.IsZero() {
		return errors.New("source capture provenance is required")
	}
	return nil
}

// SourceObjectKey returns the provider-relative content-addressed object key.
func SourceObjectKey(bundle SourceBundle, sha256 string) (string, error) {
	if err := validateBundle(bundle); err != nil {
		return "", err
	}
	if !safeObjectComponent(bundle.Capture.Harness.Name) || !safeObjectComponent(bundle.ArchiveSessionID) {
		return "", errors.New("harness name and archive session ID must be safe object-key components")
	}
	if !isLowerHexSHA256(sha256) {
		return "", errors.New("source sha256 must be 64 lowercase hexadecimal characters")
	}
	return fmt.Sprintf("sessions/%s/%s/source.%s.jsonl.gz", bundle.Capture.Harness.Name, bundle.ArchiveSessionID, sha256), nil
}

// ProjectID deterministically derives a stable project identifier from a
// project root path, so a hook and a setup flow running at different times
// agree on the same ID for the same project without a shared lookup table.
// It is lexical, matching ProjectActivation.Eligible: a caller is
// responsible for resolving symlinks before deriving an ID it depends on
// matching a previously derived one.
func ProjectID(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return "project-" + hex.EncodeToString(sum[:])[:16]
}

// MetadataObjectKey returns the provider-relative key for a session's
// replaceable metadata sidecar. Unlike SourceObjectKey it is not
// content-addressed: publishing new metadata overwrites this key.
func MetadataObjectKey(harnessName, archiveSessionID string) (string, error) {
	if !safeObjectComponent(harnessName) || !safeObjectComponent(archiveSessionID) {
		return "", errors.New("harness name and archive session ID must be safe object-key components")
	}
	return fmt.Sprintf("sessions/%s/%s/metadata.json", harnessName, archiveSessionID), nil
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func safeObjectComponent(value string) bool {
	//lint:ignore LV1001 rejects the special path names; object-key components are an open domain
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// NativeLegacyUnverifiedLinkProvenance marks a positively identified legacy
// composite reservation without independent native admission. Historical raw
// evidence stays retained, but this identity is excluded from current links.
const NativeLegacyUnverifiedLinkProvenance = "native:legacy-unverified-composite"
