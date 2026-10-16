package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const labelIndexBytes = 16 << 20

const labelIndexRecords = 1 << 18

var labelNativeUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LabelContract pins native file interpretation independently of generic retained labels.
const LabelContract = string(currentLabelInterpretation)

type labelInterpretation string

// Earlier interpretations read a settled state_5.sqlite beside the index and
// only for Codex 0.159.2; their retained evidence stays readable.
const (
	legacyLabelInterpretation  labelInterpretation = "codex-files-159.2-v1"
	settledLabelInterpretation labelInterpretation = "codex-files-159.2-v2"
	currentLabelInterpretation labelInterpretation = "codex-session-index-v3"
)

// LabelContextVersion identifies the provider's content-free interpretation.
func (LabelProvider) LabelContextVersion() string { return LabelContract }

// LabelProvider reads thread names from Codex's own name index,
// $CODEX_HOME/session_index.jsonl, for admitted top-level threads.
// Construction performs no I/O.
//
// The index is what Codex itself resolves names from: it appends
// {id, thread_name, updated_at} whenever a thread is named or renamed, and the
// last nonempty name for an ID is current. It is a plain append-only file, so
// unlike state_5.sqlite (always in WAL mode while Codex runs) it can be read
// while Codex is open, and its format has not changed across producer
// versions, so no producer version is pinned.
type LabelProvider struct {
	homeLookup func(context.Context, string, []agentapi.LabelRequest) map[string]archive.SessionLabel
}

// RegistrationLabelContext derives the lookup context from the admitted
// registration alone: an index lookup needs only the native thread ID, so the
// collector need not load the retained publication to ask.
func (LabelProvider) RegistrationLabelContext(reg archive.SessionRegistration) (agentapi.LabelContext, bool) {
	if reg.Harness.Name != "codex" || reg.IsChild() || reg.CaptureFrozen || !labelNativeUUID.MatchString(reg.NativeSessionID) {
		return agentapi.LabelContext{}, false
	}
	// The producer is not needed to read the index, and a development build's
	// version is not one the label cache accepts.
	return agentapi.LabelContext{NativeID: reg.NativeSessionID, Ordinary: true, Contract: LabelContract}, true
}

// LookupLabels reads each approved home's index at most once.
func (p LabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	if len(env.Homes) > 16 || len(requests) > 64 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	groups := map[string][]agentapi.LabelRequest{}
	roots := []string{}
	for _, request := range requests {
		if request.Context.Contract == "" {
			if proof, ok := p.RegistrationLabelContext(request.Registration); ok {
				request.Context = proof
			}
		}
		if root, ok := labelHome(env.Homes, request); ok {
			if len(groups[root]) == 0 {
				roots = append(roots, root)
			}
			groups[root] = append(groups[root], request)
		}
	}
	// Visit homes in persistent target order so an exhausted pass rotates priority.
	for _, root := range roots {
		if ctx.Err() != nil {
			break
		}
		lookup := p.homeLookup
		if lookup == nil {
			lookup = lookupLabelHome
		}
		maps.Copy(out, lookup(ctx, root, groups[root]))
	}
	return out
}

// lookupLabelHome resolves each request from one complete read of the home's
// index: the last nonempty name is present, and a complete index without one
// is confirmed absence. An index that cannot be read whole proves nothing.
func lookupLabelHome(ctx context.Context, root string, batch []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	index, complete := readLabelIndex(ctx, root, batch)
	if !complete {
		return out
	}
	for _, request := range batch {
		reg := request.Registration
		name := index[strings.ToLower(reg.NativeSessionID)]
		state := archive.SessionLabelAbsent
		if name != "" {
			state = archive.SessionLabelPresent
		}
		label := archive.SessionLabel{NativeID: reg.NativeSessionID, State: state, Name: name, Source: archive.SessionLabelIndex, Contract: LabelContract}
		if filtered, ok := archive.FilterSessionLabel(label); ok {
			out[reg.ArchiveSessionID] = filtered
		}
	}
	return out
}

// labelHome is the one approved home that owns the registration's rollout.
// Subagent threads are named from their task, not looked up.
func labelHome(homes []string, request agentapi.LabelRequest) (string, bool) {
	r, proof := request.Registration, request.Context
	if r.Harness.Name != "codex" || r.CaptureFrozen || r.IsChild() || !labelNativeUUID.MatchString(r.NativeSessionID) || proof.NativeID != r.NativeSessionID {
		return "", false
	}
	matched := ""
	for _, root := range homes {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			continue
		}
		if !labelOwnedPath(root, r.TranscriptPath, r.TranscriptPath) || (r.DiscoveryRoot != "" && r.DiscoveryRoot != root) {
			continue
		}
		if matched != "" {
			return "", false
		}
		matched = root
	}
	return matched, matched != ""
}

func labelOwnedPath(root, admitted, observed string) bool {
	if admitted == "" || admitted != observed || !filepath.IsAbs(admitted) || filepath.Clean(admitted) != admitted {
		return false
	}
	return local.PathWithin(admitted, filepath.Join(root, "sessions")) || local.PathWithin(admitted, filepath.Join(root, "archived_sessions"))
}

// Unknown quoting/profiles/placement are rejected rather than partly interpreting TOML.
func labelDefaultStorage(root string) bool {
	path := filepath.Join(root, "config.toml")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return true
	}
	f, err := labelOpenRegular(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "sqlite_home") || strings.Contains(line, "profiles") || strings.HasPrefix(line, "profile ") || strings.HasPrefix(line, "profile=") || strings.HasPrefix(line, "\"") || strings.HasPrefix(line, "'") || strings.Contains(strings.SplitN(line, "=", 2)[0], "\\") {
			return false
		}
	}
	return true
}

func readLabelIndex(ctx context.Context, root string, requests []agentapi.LabelRequest) (map[string]string, bool) {
	out := map[string]string{}
	wanted := map[string]bool{}
	for _, request := range requests {
		wanted[strings.ToLower(request.Registration.NativeSessionID)] = true
	}
	path := filepath.Join(root, "session_index.jsonl")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return out, true
	}
	f, err := labelOpenRegular(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return out, true
	}
	if err != nil {
		return out, false
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil || before.Size() > labelIndexBytes {
		return out, false
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, labelIndexBytes+1), 16384)
	for count := 0; ; count++ {
		if count >= labelIndexRecords || ctx.Err() != nil {
			return nil, false
		}
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		// A record too long to be a name is skipped whole; it names nothing
		// requested, since names are short.
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
			if err != nil {
				return nil, false
			}
			continue
		}
		// An incomplete tail cannot establish absence or freshness.
		if err != nil {
			return nil, false
		}
		var entry struct {
			ID      string  `json:"id"`
			Name    string  `json:"thread_name"`
			Updated *string `json:"updated_at"`
		}
		if json.Unmarshal(line, &entry) != nil || !wanted[strings.ToLower(entry.ID)] || strings.TrimSpace(entry.Name) == "" || entry.Updated == nil {
			continue
		}
		out[strings.ToLower(entry.ID)] = strings.TrimSpace(entry.Name)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, false
	}
	return out, true
}

// LabelContext derives safe content-free equality facts once from retained source.
func (LabelProvider) LabelContext(bundle archive.SourceBundle) agentapi.LabelContext {
	proof := agentapi.LabelContext{NativeID: bundle.NativeSessionID, Producer: bundle.Capture.Harness.Version, Contract: LabelContract}
	if bundle.History != nil || bundle.SchemaVersion != archive.SourceSchemaVersion || bundle.Capture.Harness.Name != "codex" {
		return proof
	}
	owned := false
	preview := ""
	for _, record := range bundle.NativeRecords {
		payload, _ := record["payload"].(map[string]any)
		if record["type"] == "session_meta" {
			if payload["id"] != bundle.NativeSessionID {
				return proof
			}
			owned = true
			mode, explicit := payload["history_mode"]
			proof.Legacy = mode == "legacy" || (!explicit && bundle.Capture.FilterVersion == archive.FilterVersion)
		}
		if record["type"] == "event_msg" && payload["type"] == "user_message" {
			if value, ok := payload["message"].(string); ok {
				preview = value
				break
			}
		}
	}
	proof.Ordinary = owned
	proof.PreviewComplete = owned && len(bundle.Capture.Gaps) == 0
	proof.PreviewDigest = labelPreviewDigest(preview)
	return proof
}

func labelPreviewDigest(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

// labelOpenRegular confines native reads to an approved root and never blocks
// on a FIFO. It intentionally has no higher-level preview/discovery dependency.
func labelOpenRegular(root, path string) (*os.File, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, errors.New("invalid label source locator")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("label source root unavailable")
	}
	defer func() { _ = directory.Close() }()
	before, err := directory.Lstat(rel)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("label source is not a regular file")
	}
	file, err := directory.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("label source unavailable")
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, errors.New("label source changed while opening")
	}
	return file, nil
}

func supportedLabelContract(contract string) bool {
	switch labelInterpretation(contract) {
	case currentLabelInterpretation, settledLabelInterpretation, legacyLabelInterpretation:
		return true
	}
	return false
}

// LabelRequestGroup shares lookup priority for targets in one verified native home.
func (p LabelProvider) LabelRequestGroup(env agentapi.LabelEnvironment, request agentapi.LabelRequest) string {
	if request.Context.Contract == "" {
		if proof, ok := p.RegistrationLabelContext(request.Registration); ok {
			request.Context = proof
		}
	}
	root, ok := labelHome(env.Homes, request)
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}
