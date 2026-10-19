package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runHookCommand implements the hidden `_hook` entry point hooks.Merge
// installs into each harness's own hook configuration. A hook has a short
// timeout (2s, per hooks.Merge) and must never block the user's turn, so
// this always exits 0; a problem is reported to stderr only, matching the
// spec's failure table ("Hook cannot write a request: task continues,
// diagnostic is available outside model context"). That includes a panic:
// Go exits 2 on one, which Claude Code treats as a blocking error (it
// erases the prompt on UserPromptSubmit and feeds the stack trace to the
// model on Stop), so a panic is recovered, recorded, and exits 0 too.
func runHookCommand(args []string, stdin io.Reader, stderr io.Writer, env Env) (code int) {
	var (
		home            string
		harness         = new(string)
		payload         map[string]any
		batch           []agentapi.LifecycleEvent
		diagnostic      agentapi.HookDiagnosticDecoder
		hookEnvironment map[string]string
	)
	defer func() {
		if r := recover(); r != nil {
			terminal.Printf(stderr, "agent-archive: hook: internal error: %v\n", r)
			root := ""
			if diagnostic != nil {
				func() {
					defer func() { _ = recover() }()
					candidate := diagnostic.DiagnosticProject(agentapi.HookInput{Environment: hookEnvironment, Payload: payload})
					if len(candidate) <= 16<<20 && utf8.ValidString(candidate) && filepath.IsAbs(candidate) {
						root = candidate
					}
				}()
			}
			if root == "" && len(batch) > 0 {
				root = batch[0].ProjectRoot
			}
			capture.RecordFailure(home, *harness, root)
			code = 0
		}
	}()
	fs := flag.NewFlagSet("_hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness = fs.String("harness", "", "harness name (codex, claude, cursor)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	// A hook that sends no or malformed JSON is treated as a no-op, not an
	// error: some hook events (per the harness's own docs) carry no useful
	// fields at all, and we must never fail loudly on the harness's input.
	registry := env.agentRegistry()
	decoder, ok := registry.LookupDecoder(*harness)
	if !ok {
		return 0
	}
	*harness = agentmeta.Canonical(registry.Catalog(), *harness)
	limited := &io.LimitedReader{R: stdin, N: (16 << 20) + 1}
	var err error
	if _, bounded := decoder.(agentapi.NativeHookContractProvider); bounded {
		var raw []byte
		raw, err = io.ReadAll(limited)
		if err == nil && limited.N > 0 {
			payload, err = strictHookPayload(raw)
		}
	} else {
		err = json.NewDecoder(limited).Decode(&payload)
	}
	if err != nil || payload == nil || limited.N <= 0 {
		return 0
	}

	// Resolved without creating it: a hook left behind after the data
	// directory was deleted has nothing to record and must not recreate it.
	home, err = env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: hook: resolve home: %v\n", err)
		return 0
	}
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	hookEnvironment, err = readHookEnvironment(decoder, env.lookupEnv)
	if err != nil {
		return 0
	}
	diagnostic, _ = decoder.(agentapi.HookDiagnosticDecoder)
	var now time.Time
	var clockFailure any
	func() {
		defer func() { clockFailure = recover() }()
		now = env.now()
	}()
	if clockFailure != nil {
		// Decode only diagnostic facts when the clock fails. Never admit this
		// batch, and preserve the original failure if the decoder also panics.
		func() {
			defer func() { _ = recover() }()
			batch, _ = decoder.Decode(context.Background(), agentapi.HookInput{Environment: hookEnvironment, Payload: payload, ObservedAt: time.Now()})
		}()
		panic(clockFailure)
	}
	batch, err = decoder.Decode(context.Background(), agentapi.HookInput{Environment: hookEnvironment, Payload: payload, ObservedAt: now})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: hook: %v\n", err)
		return 0
	}
	if err := capture.HandleBatch(home, *harness, batch, now, capture.WithRepoKey(env.repoKeyResolver()), capture.WithGitHead(env.gitHeadResolver()), capture.WithReplay(env.getenv(archive.ReplayEnv))); err != nil {
		terminal.Printf(stderr, "agent-archive: hook: %v\n", err)
	}

	return 0
}

// readHookEnvironment accepts only the reviewed native declaration, without an
// environment scan. No path re-reads or truncates native identity observations.
func readHookEnvironment(decoder agentapi.HookDecoder, lookup func(string) (string, bool)) (map[string]string, error) {
	provider, ok := decoder.(agentapi.NativeHookContractProvider)
	if !ok {
		return nil, nil
	}
	contract := provider.NativeHookContract()
	if contract.Contract != "muse-root-hooks-v1" || contract.MaxEnvironmentValueBytes != 128 || len(contract.EnvironmentKeys) != 2 || contract.EnvironmentKeys[0] != "MUSE_SESSION_ID" || contract.EnvironmentKeys[1] != "MUSE_THREAD_ID" {
		return nil, errors.New("unsupported native hook environment contract")
	}
	values := make(map[string]string, 2)
	for _, key := range contract.EnvironmentKeys {
		value, set := lookup(key)
		if len(value) > contract.MaxEnvironmentValueBytes || !utf8.ValidString(value) {
			return nil, errors.New("invalid native hook environment value")
		}
		if set {
			values[key] = value
		}
	}
	return values, nil
}

func strictHookPayload(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid hook UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	nodes := 0
	value, err := strictHookValue(d, 0, &nodes)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing hook input")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("hook input requires object")
	}
	for key := range object {
		for _, selector := range []string{"hook_event_name", "session_id", "transcript_path", "cwd", "source", "reason"} {
			if strings.EqualFold(key, selector) && key != selector {
				return nil, errors.New("ambiguous hook selector")
			}
		}
	}
	return object, nil
}

func strictHookValue(d *json.Decoder, depth int, nodes *int) (any, error) {
	*nodes++
	if depth > 32 || *nodes > 200000 {
		return nil, errors.New("hook structure exceeds bound")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, errors.New("invalid hook key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate hook key")
			}
			value, err := strictHookValue(d, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		_, err = d.Token()
		return object, err
	case '[':
		var array []any
		for d.More() {
			value, err := strictHookValue(d, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err = d.Token()
		return array, err
	default:
		return nil, errors.New("invalid hook delimiter")
	}
}
