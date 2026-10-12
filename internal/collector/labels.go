package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func labelScope(reg archive.SessionRegistration, env agentapi.LabelEnvironment, machine string) string {
	b, _ := json.Marshal(struct {
		Machine     string                    `json:"machine"`
		ID          string                    `json:"id"`
		Path        string                    `json:"path"`
		Root        string                    `json:"root"`
		Destination string                    `json:"destination"`
		Env         agentapi.LabelEnvironment `json:"env"`
	}{machine, reg.NativeSessionID, reg.TranscriptPath, reg.DiscoveryRoot, reg.DestinationID, env})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (p *pass) observeLabels(ctx context.Context) {
	if p.opts.Labels == nil || ctx.Err() != nil {
		return
	}
	if p.labelLocal == nil {
		p.labelLocal, p.closeLabelResources = p.local.WithReadBudget(ctx, (&sessionScan{opts: p.opts}).readBudget())
	}
	providers := map[string]agentapi.LabelProvider{}
	cache, err := p.local.LoadLabels()
	if err != nil {
		addError(p.result.Errors, "session-labels", err)
		cache = state.LabelCache{Version: 1, Entries: map[string]state.LabelEntry{}}
	}
	eligible := map[string]archive.SessionRegistration{}
	ids := []string{}
	for _, reg := range p.registrations {
		if !p.labelEligible(reg) {
			continue
		}
		_, ok := providers[reg.Harness.Name]
		if !ok {
			provider, ok := p.opts.Labels.LookupLabels(reg.Harness.Name)
			if !ok {
				continue
			}
			providers[reg.Harness.Name] = provider
		}
		eligible[reg.ArchiveSessionID] = reg
		ids = append(ids, reg.ArchiveSessionID)
	}
	sort.Strings(ids)
	p.opts.labels = map[string]state.LabelEntry{}
	p.labelStates = map[string]*state.Published{}
	for id, entry := range cache.Entries {
		reg, ok := eligible[id]
		if !ok || entry.Scope != labelScope(reg, p.opts.LabelEnvironment, p.opts.MachineID) {
			delete(cache.Entries, id)
			continue
		}
		checksum, stamp, err := p.local.LabelRevision(id)
		if err != nil {
			if errors.Is(err, state.ErrDurableStorageRecovery) {
				addError(p.result.Errors, id, err)
				p.unreadable[id] = true
			}
			delete(cache.Entries, id)
			continue
		}
		if entry.Label.State != "" && stamp == entry.SourceStamp && (checksum == "" || checksum == entry.SourceChecksum) {
			p.opts.labels[id] = entry
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	requests := p.prepareLabelRequests(ctx, providers, &cache, ids, eligible)
	requests = prioritizeLabelRequests(requests, providers, p.opts.LabelEnvironment, &cache)
	results := lookupLabelRequests(ctx, providers, p.opts.LabelEnvironment, requests)
	for _, request := range requests {
		id := request.Registration.ArchiveSessionID
		entry := cache.Entries[id]
		label, ok := archive.FilterSessionLabel(results[id])
		if ok && label.NativeID == request.Registration.NativeSessionID {
			// An index miss is weaker than a previously canonical database observation.
			if !weakerLabelAbsence(label, entry.Label) {
				if entry.Label.Fingerprint() != label.Fingerprint() {
					entry.Label, entry.ObservedAt = label, p.now
				}
			}
			entry.Failures = 0
			entry.NextAt = p.now.Add(time.Minute)
		} else {
			if entry.Failures < 6 {
				entry.Failures++
			}
			entry.NextAt = p.now.Add(time.Minute * time.Duration(1<<(entry.Failures-1)))
		}
		cache.Entries[id] = entry
		if entry.Label.State != "" {
			p.opts.labels[id] = entry
		}
	}
	// The cache is a bounded set of observations, never a native inventory.
	if len(cache.Entries) > state.MaxLabelCacheEntries {
		keys := make([]string, 0, len(cache.Entries))
		for id := range cache.Entries {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		for _, id := range keys[:len(keys)-state.MaxLabelCacheEntries] {
			delete(cache.Entries, id)
		}
	}
	pruneLabelTargetCursors(&cache)
	if err := p.local.SaveLabels(cache); err != nil {
		addError(p.result.Errors, "session-labels", err)
	}
}

func (p *pass) labelEligible(reg archive.SessionRegistration) bool {
	if reg.HistoricalProject != nil || reg.CaptureFrozen || reg.Imported() || p.unreadable[reg.ArchiveSessionID] || (p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg)) {
		return false
	}
	if p.local.GenerationCaptureAllowed(reg) != nil {
		return false
	}
	// A valid prior publication cannot authorize naming while protected
	// session work is unknown. Check before provider acquisition and before
	// either a cold context or a warm observation can enter this pass.
	if p.opts.sourcePasses == nil || p.opts.sourcePasses.env.ReadBudget == nil {
		addError(p.result.Errors, reg.ArchiveSessionID, errRetainedBudget)
		p.unreadable[reg.ArchiveSessionID] = true
		return false
	}
	scratch, closeScratch := p.local.WithReadBudget(p.ctx, (&sessionScan{opts: p.opts}).readBudget())
	err := scratch.CheckDurableSessionRead(reg.ArchiveSessionID)
	closeScratch()
	if err != nil {
		addError(p.result.Errors, reg.ArchiveSessionID, err)
		p.unreadable[reg.ArchiveSessionID] = true
		return false
	}
	return true
}

func (p *pass) prepareLabelRequests(ctx context.Context, providers map[string]agentapi.LabelProvider, cache *state.LabelCache, ids []string, eligible map[string]archive.SessionRegistration) []agentapi.LabelRequest {
	requests := []agentapi.LabelRequest{}
	start := sort.SearchStrings(ids, cache.Cursor)
	if start < len(ids) && ids[start] == cache.Cursor {
		start++
	}
	for n := 0; n < len(ids) && len(requests) < 64 && ctx.Err() == nil; n++ {
		id := ids[(start+n)%len(ids)]
		reg := eligible[id]
		entry := cache.Entries[id]
		cache.Cursor = id
		if entry.NextAt.After(p.now) {
			continue
		}
		checksum, stamp, err := p.local.LabelRevision(id)
		if err != nil {
			if errors.Is(err, state.ErrDurableStorageRecovery) {
				addError(p.result.Errors, id, err)
				p.unreadable[id] = true
			}
			continue
		}
		provider := providers[reg.Harness.Name]
		interpretation := "generic-label-context-v1"
		if builder, ok := provider.(agentapi.LabelContextProvider); ok {
			interpretation = builder.LabelContextVersion()
		}
		revision := sha256.Sum256([]byte(interpretation + "/" + archive.FilterVersion + "/" + p.opts.parserVersionFor(reg.Harness.Name)))
		contract := hex.EncodeToString(revision[:])
		validContext := entry.Context.Contract == contract && entry.Context.NativeID == reg.NativeSessionID && stamp == entry.SourceStamp && (checksum == "" || checksum == entry.SourceChecksum)
		if !validContext {
			published, n, err := p.labelLocal.LoadLabelPublication(id, (16<<20)-p.labelBytes)
			p.labelBytes += n
			if p.opts.labelReadObserver != nil {
				p.opts.labelReadObserver(n)
			}
			if err != nil {
				if errors.Is(err, state.ErrDurableStorageRecovery) {
					addError(p.result.Errors, id, err)
					p.unreadable[id] = true
				}
				continue
			}
			p.labelStates[id] = published
			bundle, _, found := published.LastPublished()
			if !found || !labelPublicationOwned(published, bundle, reg, p.opts.MachineID) {
				delete(cache.Entries, id)
				delete(p.opts.labels, id)
				continue
			}
			if bundle.History != nil || bundle.SchemaVersion == archive.HistorySourceSchemaVersion {
				delete(cache.Entries, id)
				delete(p.opts.labels, id)
				continue
			}
			source, known := published.LastPublishedSource()
			if !known {
				continue
			}
			if builder, ok := provider.(agentapi.LabelContextProvider); ok {
				entry.Context = builder.LabelContext(bundle)
			} else {
				entry.Context = agentapi.LabelContext{NativeID: bundle.NativeSessionID, Ordinary: bundle.History == nil && bundle.SchemaVersion == archive.SourceSchemaVersion, Producer: bundle.Capture.Harness.Version}
			}
			entry.Context.Contract = contract
			entry.SourceChecksum = source.SHA256
			entry.SourceStamp = stamp
			if entry.Label.State == "" {
				if label, at, ok := archive.CurrentSessionLabel(bundle); ok {
					entry.Label, entry.ObservedAt = label, at
				}
			}
		}
		requests = append(requests, agentapi.LabelRequest{Registration: reg, Context: entry.Context})
		entry.Scope = labelScope(reg, p.opts.LabelEnvironment, p.opts.MachineID)
		cache.Entries[id] = entry
	}
	return requests
}

// labelPublicationOwned checks retained ownership before native lookup or label debt.
// Missing legacy metadata is unavailable until ordinary capture/refresh supplies it.
func labelPublicationOwned(published *state.Published, bundle archive.SourceBundle, reg archive.SessionRegistration, machine string) bool {
	if bundle.ArchiveSessionID != reg.ArchiveSessionID || bundle.NativeSessionID != reg.NativeSessionID || bundle.Capture.Harness.Name != reg.Harness.Name {
		return false
	}
	metadata, found, err := published.LastPublishedMetadata()
	if err != nil || !found || metadata.SchemaVersion != archive.MetadataSchemaVersion || metadata.History != nil || bundle.SchemaVersion != archive.SourceSchemaVersion || bundle.History != nil || metadata.SessionID != reg.ArchiveSessionID || metadata.NativeSessionID != reg.NativeSessionID || metadata.MachineID != machine || metadata.Harness.Name != reg.Harness.Name {
		return false
	}
	source, known := published.LastPublishedSource()
	return known && metadata.SourceBundle == source
}

func (s *sessionScan) applyLabels(evidence []archive.SupplementalEvidence) []archive.SupplementalEvidence {
	entry, ok := s.opts.labels[s.id()]
	if !ok {
		return evidence
	}
	// Retained-source renames bypass NewSourceBundle; use its canonical UTC
	// observation encoding here too, without changing the cached instant.
	return archive.MergeSupplementalEvidence(evidence, []archive.SupplementalEvidence{entry.Label.Evidence(entry.ObservedAt.UTC(), s.reg.Harness.Name)})
}

func withoutSessionLabels(evidence []archive.SupplementalEvidence) []archive.SupplementalEvidence {
	out := make([]archive.SupplementalEvidence, 0, len(evidence))
	for _, item := range evidence {
		if item.Kind != archive.EvidenceKindSessionLabels {
			out = append(out, item)
		}
	}
	return out
}

func labelFingerprint(bundle archive.SourceBundle) string {
	label, _, ok := archive.CurrentSessionLabel(bundle)
	if !ok {
		return ""
	}
	return label.Fingerprint()
}

// refreshLabels publishes changed external evidence from the retained source.
// It does not read a transcript or renew conversation activity/retention time.
func (s *sessionScan) refreshLabels() (sessionOutcome, bool, error) {
	entry, ok := s.opts.labels[s.id()]
	if !ok || s.req.Token != "" {
		return outcomeSkipped, false, nil
	}
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return outcomeSkipped, false, err
	}
	last, found, err := s.lastPublication(key)
	if err != nil {
		return outcomeSkipped, false, err
	}
	if !found || !labelPublicationOwned(s.published, last.bundle, s.reg, s.opts.MachineID) || !sourceEvidenceWithinPolicy(last.bundle.SupplementalEvidence, s.opts.skillEvidence()) || labelFingerprint(last.bundle) == entry.Label.Fingerprint() {
		return outcomeSkipped, false, nil
	}
	reader, hasReader := newSourceReader(s.reg, s.opts)
	if hasReader {
		_, err := reader.Signature(s.ctx)
		if err == nil && last.bundle.Capture.FilterVersion != archive.FilterVersion {
			// Refilter available native input normally before refreshing its
			// names; retained missing input keeps its original filter provenance.
			return outcomeSkipped, false, nil
		}
		if err == nil && !s.sourceSettled(reader) {
			return outcomeSkipped, false, nil
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			// Only known missing native input permits retained-source refresh.
			if !errors.Is(err, os.ErrNotExist) {
				return outcomeSkipped, false, nil
			}
		} else if err != nil {
			return outcomeSkipped, false, err
		}
	}
	// The narrow summary is only a lookup fast path. Before source-backed
	// publication, verify all retained bytes against the committed reference.
	if err := s.verifyLabelSource(last.bundle, last.metadata.SourceBundle); err != nil {
		return outcomeSkipped, true, err
	}
	candidate := last.bundle
	candidate.SupplementalEvidence = s.applyLabels(candidate.SupplementalEvidence)
	if err := archive.ValidateSessionLabels(candidate); err != nil {
		return outcomeSkipped, false, err
	}
	observed := missingSource(s.reg)
	if hasReader {
		if current, err := reader.Signature(s.ctx); err == nil {
			observed = &current
		}
	}
	outcome, err := s.publish(sourceRead{observed: *observed}, candidate)
	return outcome, true, err
}

// Verification output ends before rendering the renamed source. Both compression
// passes reserve their scratch and output through the shared retained ledger.
func (s *sessionScan) verifyLabelSource(bundle archive.SourceBundle, reference archive.SourceReference) error {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	compressed, err := s.compressSource(bundle)
	if err != nil {
		return err
	}
	if compressed.SHA256 != reference.SHA256 || len(compressed.Bytes) != reference.CompressedBytes {
		return errors.New("retained source cannot be verified for session name refresh")
	}
	return nil
}

func (s *sessionScan) publishedLabel() string {
	bundle, _, _ := s.published.LastPublished()
	return labelFingerprint(bundle)
}

func (p *pass) releaseLabelResources() {
	p.labelStates = nil
	if p.closeLabelResources != nil {
		p.closeLabelResources()
		p.closeLabelResources = nil
	}
	p.labelLocal = nil
}

func prioritizeLabelRequests(requests []agentapi.LabelRequest, providers map[string]agentapi.LabelProvider, env agentapi.LabelEnvironment, cache *state.LabelCache) []agentapi.LabelRequest {
	if len(requests) == 0 {
		return requests
	}
	start := 0
	for i, request := range requests {
		if request.Registration.ArchiveSessionID != cache.PriorityCursor {
			continue
		}
		start = (i + 1) % len(requests)
		previousGroup := labelRequestGroup(providers, env, request)
		if previousGroup != "" {
			for n := 1; n < len(requests); n++ {
				candidate := (i + n) % len(requests)
				if labelRequestGroup(providers, env, requests[candidate]) != previousGroup {
					start = candidate
					break
				}
			}
		}
		break
	}
	ordered := append(append(make([]agentapi.LabelRequest, 0, len(requests)), requests[start:]...), requests[:start]...)
	rotateLabelGroupTargets(ordered, providers, env, cache)
	cache.PriorityCursor = ordered[0].Registration.ArchiveSessionID
	return ordered
}

func labelRequestGroup(providers map[string]agentapi.LabelProvider, env agentapi.LabelEnvironment, request agentapi.LabelRequest) string {
	provider, ok := providers[request.Registration.Harness.Name].(agentapi.LabelRequestGrouper)
	if !ok {
		return ""
	}
	key := provider.LabelRequestGroup(env, request)
	if key == "" || len(key) > 256 {
		return ""
	}
	sum := sha256.Sum256([]byte(request.Registration.Harness.Name + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

func weakerLabelAbsence(next, previous archive.SessionLabel) bool {
	if next.State != archive.SessionLabelAbsent {
		return false
	}
	return (next.Source == archive.SessionLabelIndex && previous.Source == archive.SessionLabelDatabase) || (next.Source != archive.SessionLabelAPI && previous.Source == archive.SessionLabelAPI)
}

func lookupLabelRequests(ctx context.Context, providers map[string]agentapi.LabelProvider, env agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	results := map[string]archive.SessionLabel{}
	groups := map[string][]agentapi.LabelRequest{}
	harnesses := []string{}
	for _, request := range requests {
		name := request.Registration.Harness.Name
		if len(groups[name]) == 0 {
			harnesses = append(harnesses, name)
		}
		groups[name] = append(groups[name], request)
	}
	for _, name := range harnesses {
		if ctx.Err() != nil {
			break
		}
		maps.Copy(results, providers[name].LookupLabels(ctx, env, groups[name]))
	}
	return results
}

// Each first group receives the next member's scheduling opportunity, even
// when unavailable-target backoff has expired before the following pass.
func rotateLabelGroupTargets(requests []agentapi.LabelRequest, providers map[string]agentapi.LabelProvider, env agentapi.LabelEnvironment, cache *state.LabelCache) {
	positions := map[string][]int{}
	for i, request := range requests {
		if key := labelRequestGroup(providers, env, request); key != "" {
			positions[key] = append(positions[key], i)
		}
	}
	for key, indices := range positions {
		start := 0
		for i, index := range indices {
			if requests[index].Registration.ArchiveSessionID == cache.TargetCursors[key] {
				start = (i + 1) % len(indices)
				break
			}
		}
		members := make([]agentapi.LabelRequest, len(indices))
		for i, index := range indices {
			members[i] = requests[index]
		}
		for i, index := range indices {
			requests[index] = members[(start+i)%len(members)]
		}
	}
	key := labelRequestGroup(providers, env, requests[0])
	if key == "" {
		return
	}
	if cache.TargetCursors == nil {
		cache.TargetCursors = map[string]string{}
	}
	if _, found := cache.TargetCursors[key]; !found && len(cache.TargetCursors) >= state.MaxLabelTargetCursors {
		keys := make([]string, 0, len(cache.TargetCursors))
		for existing := range cache.TargetCursors {
			keys = append(keys, existing)
		}
		sort.Strings(keys)
		delete(cache.TargetCursors, keys[0])
	}
	cache.TargetCursors[key] = requests[0].Registration.ArchiveSessionID
}

func pruneLabelTargetCursors(cache *state.LabelCache) {
	for key, id := range cache.TargetCursors {
		if _, ok := cache.Entries[id]; !ok {
			delete(cache.TargetCursors, key)
		}
	}
}
