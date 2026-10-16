package codex

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// LabelAPIContract pins opted-in metadata API interpretation.
const LabelAPIContract = "codex-api-159.2-v1"

// NativeLabelProvider uses an explicitly selected injected host, then safe files.
// It never inventories, admits, starts or resumes native threads.
type NativeLabelProvider struct{ Host agentapi.LabelHostFactory }

// LabelContextVersion includes both API and fallback interpretation.
func (NativeLabelProvider) LabelContextVersion() string {
	return LabelAPIContract + "/" + LabelContract
}

// LabelRequestGroup delegates pure home priority to the file capability.
func (NativeLabelProvider) LabelRequestGroup(env agentapi.LabelEnvironment, request agentapi.LabelRequest) string {
	return (LabelProvider{}).LabelRequestGroup(env, request)
}

// LabelContext delegates retained producer and ordinary-session guards to files.
func (NativeLabelProvider) LabelContext(b archive.SourceBundle) agentapi.LabelContext {
	proof := (LabelProvider{}).LabelContext(b)
	if !proof.Ordinary || proof.Producer != "0.159.2" {
		return proof
	}
	seen := false
	for _, record := range b.NativeRecords {
		if record["type"] != "session_meta" {
			continue
		}
		payload, _ := record["payload"].(map[string]any)
		if seen || payload["id"] != proof.NativeID {
			proof.APICompatible = false
			return proof
		}
		seen = true
		mode, explicit := payload["history_mode"]
		proof.APICompatible = mode == "legacy" || mode == "paginated" || (!explicit && proof.Legacy && proof.PreviewComplete)
	}
	return proof
}

// LookupLabels shares a lazy serial host per home for this bounded pass only.
func (p NativeLabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	if env.ExternalSQLite || len(env.Homes) > 16 || len(requests) > 64 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	hosts := map[string]*labelRPC{}
	defer func() {
		// Cancel all host lifetimes together before bounded close/reap.
		cancel()
		for _, rpc := range hosts {
			if rpc != nil {
				_ = rpc.host.Close()
			}
		}
	}()
	remaining := 1 << 20
	for _, request := range requests {
		if ctx.Err() != nil || remaining <= 0 {
			break
		}
		if request.Context.Contract == "" {
			request.Context = p.LabelContext(request.Bundle)
		}
		validID := labelNativeUUID.MatchString(request.Registration.NativeSessionID)
		if !validID {
			continue
		}
		home, ok := labelHome(env.Homes, request)
		if !ok || !request.Context.Ordinary || request.Context.Producer != "0.159.2" || !request.Context.APICompatible || !labelDefaultStorage(home) || p.Host == nil {
			continue
		}
		rpc, attempted := hosts[home]
		if !attempted {
			hosts[home] = nil
			host, err := p.Host(ctx, home)
			if err != nil {
				continue
			}
			rpc = &labelRPC{host: host, remaining: &remaining}
			if !rpc.initialize(ctx, home) {
				_ = host.Close()
				continue
			}
			hosts[home] = rpc
		}
		if rpc == nil {
			continue
		}
		readCtx, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		label, ok := rpc.read(readCtx, request.Registration.NativeSessionID)
		stop()
		if ok {
			out[request.Registration.ArchiveSessionID] = label
		} else {
			_ = rpc.host.Close()
			hosts[home] = nil
		}
	}
	// Only unavailable IDs fall back to the name index.
	pending := []agentapi.LabelRequest{}
	for _, request := range requests {
		if _, ok := out[request.Registration.ArchiveSessionID]; !ok {
			pending = append(pending, request)
		}
	}
	maps.Copy(out, (LabelProvider{}).LookupLabels(ctx, env, pending))
	return out
}

type labelRPC struct {
	host      agentapi.LabelTransport
	remaining *int
	id        int
}

func (r *labelRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.id++
	wire, err := json.Marshal(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{r.id, method, params})
	if err != nil {
		return nil, err
	}
	if err = r.host.WriteLine(ctx, wire); err != nil {
		return nil, err
	}
	for {
		line, err := r.host.ReadLine(ctx)
		if err != nil {
			return nil, err
		}
		*r.remaining -= len(line) + 1
		if len(line) > 256<<10 || *r.remaining < 0 {
			return nil, agentapi.ErrLabelBudgetExceeded
		}
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &envelope) != nil {
			return nil, agentapi.ErrLabelProtocolUnavailable
		}
		// Notifications are discarded; server requests never execute local tools.
		if envelope.Method != "" {
			if len(envelope.ID) > 0 {
				return nil, agentapi.ErrLabelRequestRejected
			}
			continue
		}
		var id int
		if json.Unmarshal(envelope.ID, &id) != nil || id != r.id || len(envelope.Error) > 0 || len(envelope.Result) == 0 {
			return nil, agentapi.ErrLabelProtocolUnavailable
		}
		return envelope.Result, nil
	}
}

func (r *labelRPC) initialize(ctx context.Context, home string) bool {
	result, err := r.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "agent_archive_probe", "version": "0.0.0"}, "capabilities": map[string]any{"experimentalApi": false}})
	if err != nil {
		return false
	}
	var init struct {
		Home  string `json:"codexHome"`
		Agent string `json:"userAgent"`
	}
	if json.Unmarshal(result, &init) != nil || init.Home != home || !strings.HasPrefix(init.Agent, "agent_archive_probe/0.159.2 ") {
		return false
	}
	return r.host.WriteLine(ctx, []byte(`{"method":"initialized"}`)) == nil
}

func (r *labelRPC) read(ctx context.Context, id string) (archive.SessionLabel, bool) {
	result, err := r.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return archive.SessionLabel{}, false
	}
	var response struct {
		Thread struct {
			ID    string          `json:"id"`
			Name  json.RawMessage `json:"name"`
			Turns json.RawMessage `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(result, &response) != nil || response.Thread.ID != id || len(response.Thread.Name) == 0 {
		return archive.SessionLabel{}, false
	}
	if len(response.Thread.Turns) > 0 {
		var turns []json.RawMessage
		if json.Unmarshal(response.Thread.Turns, &turns) != nil || turns == nil || len(turns) != 0 {
			return archive.SessionLabel{}, false
		}
	}
	label := archive.SessionLabel{NativeID: id, State: archive.SessionLabelAbsent, Source: archive.SessionLabelAPI, Contract: LabelAPIContract}
	if string(response.Thread.Name) != "null" {
		if json.Unmarshal(response.Thread.Name, &label.Name) != nil {
			return archive.SessionLabel{}, false
		}
		if strings.TrimSpace(label.Name) != "" {
			label.State = archive.SessionLabelPresent
		}
	}
	return archive.FilterSessionLabel(label)
}

func supportedSessionLabel(label archive.SessionLabel) bool {
	if !labelNativeUUID.MatchString(label.NativeID) {
		return false
	}
	if label.Contract == LabelAPIContract {
		return label.Source == archive.SessionLabelAPI
	}
	return supportedLabelContract(label.Contract) && (label.Source == archive.SessionLabelIndex || label.Source == archive.SessionLabelDatabase)
}
