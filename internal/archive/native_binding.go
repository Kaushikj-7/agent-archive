package archive

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

// NativeRoot is a reviewed local data home, independent of discovery consent.
// Setup changes Revision whenever it reconciles either path. Collection must
// use Canonical rather than deriving a home from its launch environment.
type NativeRoot struct {
	Agent      string `json:"agent"`
	Configured string `json:"configured"`
	Canonical  string `json:"canonical"`
	Revision   string `json:"revision"`
}

// Validate checks identity and bounded absolute path spelling without I/O.
func (r NativeRoot) Validate() error {
	if !nativeIdentity(r.Agent, r.Revision) || !nativePath(r.Configured) || !nativePath(r.Canonical) {
		return errors.New("invalid native source root")
	}
	return nil
}

// NativeSourceBinding records validated original facts, never capture consent.
// Relocation is deliberately unsupported: continuity needs a separately reviewed
// native proof before any existing locator or root revision can change.
type NativeSourceBinding struct {
	Agent           string            `json:"agent"`
	Version         int               `json:"version"`
	NativeID        string            `json:"native_id"`
	RootID          string            `json:"root_id,omitempty"`
	ParentID        string            `json:"parent_id,omitempty"`
	NativeCreatedAt time.Time         `json:"native_created_at"`
	Cwd             string            `json:"cwd"`
	StreamID        string            `json:"stream_id,omitempty"`
	Path            string            `json:"path"`
	RootRevision    string            `json:"root_revision"`
	Checkpoint      *NativeCheckpoint `json:"checkpoint,omitempty"`
}

// Validate checks bounded opaque identities separately from physical paths.
func (b *NativeSourceBinding) Validate() error {
	if b == nil {
		return nil
	}
	if (b.Version != 1 && b.Version != 2) || b.Version == 1 && b.Checkpoint != nil || b.Version == 2 && (b.Agent != "muse" || b.Checkpoint == nil || b.Checkpoint.Validate() != nil) || b.Agent == "codex" || !nativeIdentity(b.Agent, b.NativeID) || !nativeToken(b.RootRevision) || b.NativeCreatedAt.IsZero() || !nativePath(b.Cwd) || !nativePath(b.Path) || b.RootID != "" && !nativeToken(b.RootID) || b.ParentID != "" && !nativeToken(b.ParentID) || b.StreamID != "" && !nativeToken(b.StreamID) {
		return errors.New("invalid native source binding")
	}
	return nil
}

// NativeCheckpoint is the last durably accepted complete raw native validation.
// It stays local; it is not a publication checksum or acknowledgement.
type NativeCheckpoint struct {
	Version   uint8  `json:"version"`
	Domain    string `json:"domain"`
	Algorithm string `json:"algorithm"`
	Boundary  int64  `json:"boundary"`
	SHA256    string `json:"sha256"`
}

// Validate refuses unknown or incomplete proof formats.
func (c *NativeCheckpoint) Validate() error {
	if c == nil || c.Version != 1 || c.Domain != "muse-ordinary" || c.Algorithm != "sha256" || c.Boundary <= 0 || len(c.SHA256) != 64 || strings.ToLower(c.SHA256) != c.SHA256 {
		return errors.New("invalid native validation checkpoint")
	}
	if _, err := hex.DecodeString(c.SHA256); err != nil {
		return errors.New("invalid native validation checkpoint")
	}
	return nil
}

type nativeCheckpointField string

const (
	checkpointVersion   nativeCheckpointField = "version"
	checkpointDomain    nativeCheckpointField = "domain"
	checkpointAlgorithm nativeCheckpointField = "algorithm"
	checkpointBoundary  nativeCheckpointField = "boundary"
	checkpointSHA256    nativeCheckpointField = "sha256"
)

// UnmarshalJSON refuses unknown checkpoint fields before any writer can drop them.
func (c *NativeCheckpoint) UnmarshalJSON(raw []byte) error {
	if len(raw) > 512 {
		return errors.New("invalid native validation checkpoint")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid native validation checkpoint")
	}
	seen := map[nativeCheckpointField]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		fieldName, ok := token.(string)
		key := nativeCheckpointField(fieldName)
		if !ok || seen[key] || (key != checkpointVersion && key != checkpointDomain && key != checkpointAlgorithm && key != checkpointBoundary && key != checkpointSHA256) {
			return errors.New("invalid native validation checkpoint field")
		}
		seen[key] = true
		var field json.RawMessage
		if err := decoder.Decode(&field); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if len(seen) != 5 {
		return errors.New("incomplete native validation checkpoint")
	}
	type checkpoint NativeCheckpoint
	var value checkpoint
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	next := NativeCheckpoint(value)
	if err := next.Validate(); err != nil {
		return err
	}
	*c = next
	return nil
}

// SameCheckpoint compares the complete protected proof, including absence.
func (b *NativeSourceBinding) SameCheckpoint(previous *NativeSourceBinding) bool {
	var a, c *NativeCheckpoint
	if b != nil {
		a = b.Checkpoint
	}
	if previous != nil {
		c = previous.Checkpoint
	}
	return a == nil && c == nil || a != nil && c != nil && *a == *c
}

// AdvancesCheckpoint checks shape and monotonicity; callers must independently
// prove the previous raw prefix and use the dedicated guarded prepared commit.
func (b *NativeSourceBinding) AdvancesCheckpoint(previous *NativeSourceBinding) bool {
	if b == nil || b.Version != 2 || b.Validate() != nil || !b.PreservesFacts(previous) {
		return false
	}
	if previous == nil {
		return true
	}
	if previous.Version != 2 || previous.Validate() != nil {
		return false
	}
	a, c := b.Checkpoint, previous.Checkpoint
	return a.Boundary > c.Boundary || *a == *c
}

// PreservesFacts refuses loss, replacement or relocation of original authority.
func (b *NativeSourceBinding) PreservesFacts(previous *NativeSourceBinding) bool {
	if previous == nil {
		return true
	}
	return b != nil && b.Agent == previous.Agent && b.Version == previous.Version && b.NativeID == previous.NativeID && b.NativeCreatedAt.Equal(previous.NativeCreatedAt) && b.Cwd == previous.Cwd && b.Path == previous.Path && b.RootRevision == previous.RootRevision && (previous.RootID == "" || b.RootID == previous.RootID) && (previous.ParentID == "" || b.ParentID == previous.ParentID) && (previous.StreamID == "" || b.StreamID == previous.StreamID)
}

func nativeIdentity(agent, id string) bool {
	return len(agent) <= 128 && (agentmeta.SessionKey{Agent: agentmeta.ID(agent), NativeID: id}).Validate() == nil && nativeToken(id)
}

func nativeToken(s string) bool {
	return s != "" && len(s) <= 4096 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}

func nativePath(s string) bool {
	return nativeToken(s) && filepath.IsAbs(s) && filepath.Clean(s) == s
}

// validateNativeBinding checks the private binding against its owning registration.
func (r SessionRegistration) validateNativeBinding() error {
	if r.NativeBinding != nil {
		if r.CodexBinding != nil || !r.ReadsTranscriptFile() || r.NativeBinding.Agent != r.Harness.Name || r.NativeBinding.NativeID != r.NativeSessionID || r.TranscriptPath != r.NativeBinding.Path || r.ParentNativeSessionID != "" && r.ParentNativeSessionID != r.NativeBinding.ParentID || r.NativeRootSessionID != "" && r.NativeRootSessionID != r.NativeBinding.RootID {
			return errors.New("native binding requires matching registration and locator")
		}
		if err := r.NativeBinding.Validate(); err != nil {
			return err
		}
	}
	return nil
}
