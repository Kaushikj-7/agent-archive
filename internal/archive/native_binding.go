package archive

import (
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
	Agent           string    `json:"agent"`
	Version         int       `json:"version"`
	NativeID        string    `json:"native_id"`
	RootID          string    `json:"root_id,omitempty"`
	ParentID        string    `json:"parent_id,omitempty"`
	NativeCreatedAt time.Time `json:"native_created_at"`
	Cwd             string    `json:"cwd"`
	StreamID        string    `json:"stream_id,omitempty"`
	Path            string    `json:"path"`
	RootRevision    string    `json:"root_revision"`
}

// Validate checks bounded opaque identities separately from physical paths.
func (b *NativeSourceBinding) Validate() error {
	if b == nil {
		return nil
	}
	if b.Version != 1 || b.Agent == "codex" || !nativeIdentity(b.Agent, b.NativeID) || !nativeToken(b.RootRevision) || b.NativeCreatedAt.IsZero() || !nativePath(b.Cwd) || !nativePath(b.Path) || b.RootID != "" && !nativeToken(b.RootID) || b.ParentID != "" && !nativeToken(b.ParentID) || b.StreamID != "" && !nativeToken(b.StreamID) {
		return errors.New("invalid native source binding")
	}
	return nil
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
