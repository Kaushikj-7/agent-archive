package agentapi

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// MaxNativeAdmissionIntentBytes is the existing private Muse v2 wire bound.
const MaxNativeAdmissionIntentBytes = 32 << 10

type nativeRootEvent string

const (
	nativeRootStart  nativeRootEvent = "SessionStart"
	nativeRootPrompt nativeRootEvent = "UserPromptSubmit"
	nativeRootStop   nativeRootEvent = "Stop"
	nativeRootEnd    nativeRootEvent = "SessionEnd"
)

// ValidNativeAdmissionIntentShape checks closed wire shape and size only. It
// grants no source, owner, window, project or publication authority.
func ValidNativeAdmissionIntentShape(i AdmissionIntent) bool {
	if !validNativeRootEnvelope(i) {
		return false
	}
	if i.TranscriptPath != "" || i.CodexPolicyToken != "" || i.CursorVersion != "" || i.ComposerMode != "" || i.LastHead != nil {
		return false
	}
	switch i.NativeMode {
	case NativeStartupCandidate:
		if i.Replay != nil && i.Replay.RunID != "" && !archive.IsReplayRunID(i.Replay.RunID) {
			return false
		}
		if i.Event != string(nativeRootStart) || i.NativeOwnerID != "" || i.NativeWindowGeneration != "" || len(i.Effects) != 0 {
			return false
		}
	case NativeOwnerContinuation:
		if i.Replay != nil || !nativeRootHex(i.NativeOwnerID, 32) || !nativeRootHex(i.NativeWindowGeneration, 32) || len(i.Effects) != 1 || !nativeRootContinuationEvent(i, i.Effects[0].Event) {
			return false
		}
	default:
		return false
	}
	raw, err := json.MarshalIndent(i, "", "  ")
	return err == nil && len(raw)+1 <= MaxNativeAdmissionIntentBytes
}

func validNativeRootEnvelope(i AdmissionIntent) bool {
	if i.Version != 2 || i.Harness != "muse" || i.NativeAdmission == nil || i.NativeAdmission.Validate() != nil || !nativeRootID(i.NativeSessionID) || !nativeRootPath(i.ProjectRoot, true) || !nativeRootPath(i.ObservedCwd, false) || !nativeRootHex(i.DestinationID, 64) || i.PauseGeneration != "" && !nativeRootHex(i.PauseGeneration, 32) || i.ObservedAt.IsZero() {
		return false
	}
	return true
}

func nativeRootContinuationEvent(i AdmissionIntent, e LifecycleEvent) bool {
	if e.Session.Agent != agentmeta.Muse || e.Session.NativeID != i.NativeSessionID || e.Session.Version != "" || e.Session.Mode != NativeModeUnspecified || e.ProjectRoot != i.ObservedCwd || e.NativeHookContract != "muse-root-hooks-v1" || e.NativeEvent != i.Event || e.Reason != "native_root_observation" {
		return false
	}
	if !nativeRootEmptyEffect(e) {
		return false
	}
	switch nativeRootEvent(i.Event) {
	case nativeRootStart, nativeRootPrompt:
		return e.Kind == EventTurnStart
	case nativeRootStop, nativeRootEnd:
		return e.Kind == EventStop
	default:
		return false
	}
}

func nativeRootEmptyEffect(e LifecycleEvent) bool {
	return e.Source == (SourceRef{}) && e.Locator == LocatorNone && e.Start == (StartEvidence{}) && !e.NewOnly && e.Child == nil && len(e.Evidence) == 0 && e.Deferred == DeferredNone && e.CodexProjectRoot == "" && e.CodexCwd == "" && e.CodexPolicyToken == ""
}

func nativeRootID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		alnum := ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
		if !alnum && (i == 0 || ch != '_' && ch != '-') {
			return false
		}
	}
	return true
}

func nativeRootPath(path string, canonical bool) bool {
	return len(path) <= 4096 && utf8.ValidString(path) && filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n") && (!canonical || filepath.Clean(path) == path)
}

func nativeRootHex(value string, n int) bool {
	if len(value) != n || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
