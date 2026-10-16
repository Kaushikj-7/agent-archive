package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SessionLabelState distinguishes a verified name from verified absence.
type SessionLabelState string

// SessionLabelSource identifies the fixed class of a naming observation.
type SessionLabelSource string

// SessionLabelPresent and SessionLabelAbsent distinguish a verified name
// from verified absence; the fixed source classes record observation authority.
const (
	SessionLabelPresent  SessionLabelState  = "present"
	SessionLabelAbsent   SessionLabelState  = "confirmed_absent"
	SessionLabelIndex    SessionLabelSource = "index"
	SessionLabelDatabase SessionLabelSource = "database"
	// SessionLabelAPI identifies accepted metadata API evidence.
	SessionLabelAPI SessionLabelSource = "api"
)

// SessionLabel is a bounded filtered native observation. Its zero value is unavailable.
type SessionLabel struct {
	NativeID string             `json:"native_session_id"`
	State    SessionLabelState  `json:"state"`
	Name     string             `json:"name,omitempty"`
	Source   SessionLabelSource `json:"source"`
	Contract string             `json:"contract"`
}

// FilterSessionLabel validates the narrow shape and filters before any persistence.
func FilterSessionLabel(label SessionLabel) (SessionLabel, bool) {
	if !safeLabelToken(label.Contract, 64) || !safeLabelToken(label.NativeID, 256) || (label.Source != SessionLabelIndex && label.Source != SessionLabelDatabase && label.Source != SessionLabelAPI) || (label.State != SessionLabelPresent && label.State != SessionLabelAbsent) {
		return SessionLabel{}, false
	}
	if label.State == SessionLabelAbsent {
		return label, label.Name == ""
	}
	if len(label.Name) > 16384 || !utf8.ValidString(label.Name) {
		return SessionLabel{}, false
	}
	state := PrivacyState{AddGap: func(string, int, string) {}}
	v, keep := sanitizeValue(label.Name, &state)
	name, ok := v.(string)
	if !keep || !ok {
		return SessionLabel{}, false
	}
	name = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return SessionLabel{}, false
	}
	if len(name) > 512 {
		name = name[:512]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	// Presentation normalization can remove the indentation that made literal
	// markup safe. Refuse an unstable observation before it enters the cache;
	// unavailable names preserve the last successful evidence.
	again, keep := sanitizeValue(name, &state)
	if !keep || again != name {
		return SessionLabel{}, false
	}
	label.Name = name
	return label, true
}

// Fingerprint hashes only filtered semantic content, never observation time.
func (label SessionLabel) Fingerprint() string {
	filtered, ok := FilterSessionLabel(label)
	if !ok {
		return ""
	}
	b, _ := json.Marshal(filtered)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Evidence retains the current native label without native paths or response data.
func (label SessionLabel) Evidence(at time.Time, harness string) SupplementalEvidence {
	payload := map[string]any{"native_session_id": label.NativeID, "state": string(label.State), "source": string(label.Source), "contract": label.Contract}
	if label.State == SessionLabelPresent {
		payload["name"] = label.Name
	}
	return SupplementalEvidence{Kind: EvidenceKindSessionLabels, ObservedAt: at, Provenance: labelProvenance(harness), Payload: payload}
}

func labelFromEvidence(e SupplementalEvidence) (SessionLabel, bool) {
	if e.Kind != EvidenceKindSessionLabels || !safeLabelToken(labelEvidenceHarness(e), 64) {
		return SessionLabel{}, false
	}
	for key := range e.Payload {
		if !map[string]bool{"native_session_id": true, "state": true, "name": true, "source": true, "contract": true}[key] {
			return SessionLabel{}, false
		}
	}
	l := SessionLabel{}
	var state, source string
	fields := []struct {
		key    string
		target *string
	}{{"native_session_id", &l.NativeID}, {"state", &state}, {"source", &source}, {"contract", &l.Contract}}
	for _, field := range fields {
		value, ok := e.Payload[field.key].(string)
		if !ok {
			return SessionLabel{}, false
		}
		*field.target = value
	}
	l.State, l.Source = SessionLabelState(state), SessionLabelSource(source)
	if value, exists := e.Payload["name"]; exists {
		name, ok := value.(string)
		if !ok {
			return SessionLabel{}, false
		}
		l.Name = name
	}
	return FilterSessionLabel(l)
}

// CurrentSessionLabel returns only evidence matching its owning bundle.
func CurrentSessionLabel(bundle SourceBundle) (SessionLabel, time.Time, bool) {
	if !safeLabelToken(bundle.Capture.Harness.Name, 64) {
		return SessionLabel{}, time.Time{}, false
	}
	for i := len(bundle.SupplementalEvidence) - 1; i >= 0; i-- {
		e := bundle.SupplementalEvidence[i]
		if l, ok := labelFromEvidence(e); ok && l.NativeID == bundle.NativeSessionID && e.Provenance == labelProvenance(bundle.Capture.Harness.Name) {
			return l, e.ObservedAt, true
		}
	}
	return SessionLabel{}, time.Time{}, false
}

// ValidateSessionLabels prevents a label from changing another source's name.
// An ordinary source and a self-contained history source both carry at most
// one label for their own native session.
func ValidateSessionLabels(bundle SourceBundle) error {
	ordinary := bundle.History == nil && bundle.SchemaVersion == SourceSchemaVersion
	history := bundle.History != nil && bundle.SchemaVersion == HistorySourceSchemaVersion
	seen := false
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind != EvidenceKindSessionLabels {
			continue
		}
		l, ok := labelFromEvidence(e)
		if seen || !ok || !reflect.DeepEqual(l.Evidence(e.ObservedAt, bundle.Capture.Harness.Name).Payload, e.Payload) || e.Provenance != labelProvenance(bundle.Capture.Harness.Name) || l.NativeID != bundle.NativeSessionID || (!ordinary && !history) {
			return errors.New("session label does not match its owning source")
		}
		seen = true
	}
	return nil
}

func safeLabelToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	filtered, keep := SanitizeValue(value, &PrivacyState{AddGap: func(string, int, string) {}})
	return keep && filtered == value
}

func labelProvenance(harness string) string { return "native:" + harness + ":session_labels" }

func labelEvidenceHarness(e SupplementalEvidence) string {
	if !strings.HasPrefix(e.Provenance, "native:") || !strings.HasSuffix(e.Provenance, ":session_labels") {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(e.Provenance, "native:"), ":session_labels")
}
