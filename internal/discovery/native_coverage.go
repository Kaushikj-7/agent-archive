package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

const maxCoverageRequests = 256

const maxCoverageRequestBytes = 1 << 20

const maxCoverageBytes = 16 << 20

type coveragePhase string

const (
	coverageObserve  coveragePhase = "observe"
	coverageValidate coveragePhase = "validate"
	coverageComplete coveragePhase = "complete"
)

// coverageInventory is bounded REQUESTED evidence, not a global native index.
// Enumerating stores only directory digests and requested matching facts. A
// second shared streaming pass validates ALL entry fingerprints before complete
// evidence becomes usable. A rollout's fingerprint is its membership only (see
// memberFingerprint), so appends by running sessions cannot fail every epoch;
// validation instead rechecks each rollout's header identity against the
// recorded candidates. Stamp comparison has the ordinary filesystem model:
// same-stamp out-of-band rewrites and changes after observation are not detected.
type coverageInventory struct {
	Version      int                          `json:"version"`
	Epoch        uint64                       `json:"epoch"`
	Phase        coveragePhase                `json:"phase"`
	Roots        []string                     `json:"roots"`
	Requests     map[string]coverageRequest   `json:"requests"`
	Directories  map[string]coverageDirectory `json:"directories"`
	Validation   []directory                  `json:"validation"`
	Sequence     uint64                       `json:"sequence,omitempty"`
	FinalOffset  int                          `json:"final_offset,omitempty"`
	hintBytes    int64
	factBound    int64
	proofEpoch   uint64
	reserveFacts func(int64) bool
	Failed       bool `json:"failed,omitempty"`
	// MemberChecks records that this epoch's validation marks candidate
	// membership. A validating checkpoint from an earlier writer lacks the
	// marks, so finishing it would drop every candidate it already validated.
	MemberChecks bool `json:"member_checks,omitempty"`
}

type coverageRequest struct {
	Order          uint64                       `json:"order,omitempty"`
	DeliveredEpoch uint64                       `json:"delivered_epoch,omitempty"`
	AttemptEpoch   uint64                       `json:"attempt_epoch,omitempty"`
	TargetEpoch    uint64                       `json:"target_epoch"`
	CompleteEpoch  uint64                       `json:"complete_epoch,omitempty"`
	Overflow       bool                         `json:"overflow,omitempty"`
	Candidates     map[string]coverageCandidate `json:"candidates"`
}

type coverageCandidate struct {
	Source   SourceDescriptor        `json:"source"`
	Stamp    Fingerprint             `json:"stamp"`
	Identity codexmeta.CodexIdentity `json:"identity"`
	// Member records that this epoch's validation listed the locator and
	// reread the same identity. Restored cache facts can name a rollout that
	// has since been renamed into archived_sessions/; such a candidate is not
	// a member and is dropped when validation completes.
	Member bool `json:"member,omitempty"`
}

type coverageDirectory struct {
	Directory        directory `json:"directory"`
	Stamp            string    `json:"stamp"`
	Digest           string    `json:"digest"`
	Offset           int64     `json:"offset"`
	Complete         bool      `json:"complete"`
	Validated        bool      `json:"validated"`
	ValidationDigest string    `json:"validation_digest,omitempty"`
}

// coverageBatch keeps one bounded getdents batch's fingerprints. These include
// irrelevant regular files, directories and symlinks, so an unrelated header
// rewrite cannot hide behind an unchanged directory mtime during validation.
type coverageBatch struct {
	Stamp       string
	Entries     []string
	Unavailable bool
}

func entryFingerprint(name string, info os.FileInfo) string {
	identity := ""
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	raw := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", name, identity, info.Mode(), info.Size(), info.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// memberFingerprint identifies a rollout file's directory membership: its
// name, file identity and type. Size and modification time are left out so an
// append by a running Codex session does not fail the whole-home proof; the
// validation pass rechecks that rollout's header identity instead.
func memberFingerprint(name string, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	raw := fmt.Sprintf("member\x00%s\x00%d:%d\x00%d", name, stat.Dev, stat.Ino, info.Mode())
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func appendCoverageDigest(prior string, entries []string) string {
	digest, err := hex.DecodeString(prior)
	if err != nil || len(digest) != sha256.Size {
		digest = make([]byte, sha256.Size)
	}
	for _, entry := range entries {
		sum := sha256.Sum256(append(digest, []byte(entry)...))
		digest = sum[:]
	}
	return hex.EncodeToString(digest)
}

func newCoverage(roots []string) *coverageInventory {
	return &coverageInventory{Version: 1, Epoch: 1, Phase: coverageObserve, Roots: slices.Clone(roots), Requests: map[string]coverageRequest{}, Directories: map[string]coverageDirectory{}}
}

func (c *coverageInventory) validate(roots []string) error {
	if c == nil {
		return nil
	}
	if c.Version != 1 {
		return errors.New("native coverage requires a newer writer")
	}
	c.factBound = 0
	if c.Epoch == 0 || !slices.Equal(c.Roots, roots) || (c.Phase != coverageObserve && c.Phase != coverageValidate && c.Phase != coverageComplete) || len(c.Requests) > maxCoverageRequests || len(c.Directories) > maxDirectories || len(c.Validation) > maxDirectories || c.FinalOffset < 0 || c.FinalOffset > len(c.Directories) {
		return errors.New("invalid native coverage checkpoint")
	}
	if c.totalByteBound() > maxCoverageBytes {
		return errors.New("native coverage byte limit")
	}
	for _, request := range c.Requests {
		if request.byteBound() > maxCoverageRequestBytes || len(request.Candidates) > 64 {
			return errors.New("native coverage request limit")
		}
	}
	for _, entry := range c.Directories {
		if entry.Offset < 0 || !slices.Contains(roots, entry.Directory.Root) || filepath.IsAbs(entry.Directory.Path) || filepath.Clean(entry.Directory.Path) != entry.Directory.Path {
			return errors.New("invalid native coverage directory")
		}
	}
	if err := c.validateCompleteRequests(); err != nil {
		return err
	}
	if err := c.validateCompleteProof(); err != nil {
		return err
	}
	if c.Phase == coverageValidate && !c.MemberChecks {
		c.Failed = true
	}
	if c.Phase == coverageComplete && !c.Failed {
		c.proofEpoch = c.Epoch
	}
	return nil
}

func (c *coverageInventory) request(id string) bool {
	if _, present := c.Requests[id]; present {
		return true
	}
	if len(id) > 4096 {
		return false
	}
	victim := ""
	if len(c.Requests) >= maxCoverageRequests {
		// Retire only work that has finished a complete observation/validation
		// attempt. Unfinished requests survive every capacity refusal.
		for key, request := range c.Requests {
			if request.AttemptEpoch == 0 || request.DeliveredEpoch != request.AttemptEpoch {
				continue
			}
			if victim == "" || request.Order < c.Requests[victim].Order || request.Order == c.Requests[victim].Order && key < victim {
				victim = key
			}
		}
		if victim == "" {
			return false
		}
	}
	removed := int64(0)
	if victim != "" {
		removed = 6*int64(len(victim)) + c.Requests[victim].byteBound() + 16
	}
	if c.totalByteBound()-removed+6*int64(len(id))+512 > maxCoverageBytes {
		return false
	}
	if !c.reserve(6*int64(len(id)) + 512 + 16) {
		return false
	}
	if victim != "" {
		delete(c.Requests, victim)
	}
	c.Sequence++
	c.factBound = 0
	// A request entering mid-round waits for an entire following epoch. Earlier
	// observations cannot prove absence for something that was not requested yet.
	target := c.Epoch + 1
	if c.Phase == coverageObserve && len(c.Directories) == 0 {
		target = c.Epoch
	}
	c.Requests[id] = coverageRequest{Order: c.Sequence, TargetEpoch: target, Candidates: map[string]coverageCandidate{}}
	return true
}

func (c *coverageInventory) observe(source SourceDescriptor, stamp Fingerprint, id codexmeta.CodexIdentity) bool {
	changed := false
	for key, request := range c.Requests {
		if key != id.ThreadID && key != id.RolloutID || c.Phase != coverageObserve || request.TargetEpoch > c.Epoch {
			continue
		}
		if _, present := request.Candidates[source.Locator]; !present && len(request.Candidates) >= 64 {
			changed = changed || !request.Overflow
			request.Overflow = true
			c.Requests[key] = request
			continue
		}
		candidate := coverageCandidate{Source: source, Stamp: stamp, Identity: id}
		added := candidate.byteBound() + 6*int64(len(source.Locator)) + 16
		prior := int64(0)
		if old, found := request.Candidates[source.Locator]; found {
			prior = old.byteBound() + 6*int64(len(source.Locator)) + 16
		}
		if request.byteBound()+added-prior > maxCoverageRequestBytes || c.totalByteBound()+added-prior > maxCoverageBytes || !c.reserve(max(added-prior, 0)) {
			changed = changed || !request.Overflow
			request.Overflow = true
		} else {
			old, found := request.Candidates[source.Locator]
			changed = changed || !found || old.Source != source || old.Stamp != stamp || !reflect.DeepEqual(old.Identity, id)
			request.Candidates[source.Locator] = candidate
			c.factBound += added - prior
		}
		c.Requests[key] = request
	}
	return changed
}

func coverageKey(d directory) string { return filepath.Join(d.Root, d.Path) }

// recordBatch advances only when the caller consumed the whole batch. Retried
// getdents batches keep the old cookie and do not duplicate the digest chain.
func (c *coverageInventory) recordBatch(d directory, b coverageBatch, next int64, complete, advanced bool) {
	if c.Phase != coverageObserve || !advanced {
		return
	}
	key := coverageKey(d)
	prior, present := c.Directories[key]
	if b.Unavailable || len(b.Stamp) != 64 || !present && len(c.Directories) >= maxDirectories {
		c.Failed = true
		return
	}
	if !present {
		if c.totalByteBound()+directoryByteBound(d)+1024 > maxCoverageBytes {
			c.Failed = true
			return
		}
		if d.Offset != 0 {
			c.Failed = true
			return
		}
		if !c.reserve(6*int64(len(key)) + 2*directoryByteBound(d) + 1040) {
			c.Failed = true
			return
		}
		prior = coverageDirectory{Directory: d, Stamp: b.Stamp}
		c.factBound = 0
	}
	if prior.Offset != d.Offset || prior.Stamp != b.Stamp {
		c.Failed = true
		return
	}
	prior.Digest = appendCoverageDigest(prior.Digest, b.Entries)
	prior.Offset = next
	prior.Complete = complete
	c.Directories[key] = prior
}

func (c *coverageInventory) reserve(n int64) bool {
	return n == 0 || c.reserveFacts == nil || c.reserveFacts(n)
}

func (c *coverageInventory) beginValidation() {
	if c.Phase != coverageObserve {
		return
	}
	c.Phase = coverageValidate
	c.MemberChecks = true
	c.Validation = nil
	for _, entry := range c.Directories {
		if !entry.Complete {
			c.Failed = true
		}
		d := entry.Directory
		d.Offset = 0
		c.Validation = append(c.Validation, d)
	}
	slices.SortFunc(c.Validation, func(a, b directory) int {
		if coverageKey(a) < coverageKey(b) {
			return -1
		}
		if coverageKey(a) > coverageKey(b) {
			return 1
		}
		return 0
	})
}

func (c *coverageInventory) validateBatch(d directory, b coverageBatch, complete bool) {
	if c.Phase != coverageValidate {
		return
	}
	key := coverageKey(d)
	prior, present := c.Directories[key]
	if !present || b.Unavailable || prior.Stamp != b.Stamp {
		c.Failed = true
		return
	}
	prior.ValidationDigest = appendCoverageDigest(prior.ValidationDigest, b.Entries)
	if complete {
		prior.Validated = prior.ValidationDigest == prior.Digest
		if !prior.Validated {
			c.Failed = true
		}
	}
	c.Directories[key] = prior
}

// checkMember fails validation unless a rollout's identity, read at
// validation time, agrees with the candidates its observation recorded for
// every request that epoch covers. Membership digests omit rollout sizes and
// times, so this is what detects a header rewritten in place mid-epoch.
func (c *coverageInventory) checkMember(locator string, id *codexmeta.CodexIdentity) {
	if c.Phase != coverageValidate || c.Failed {
		return
	}
	if id != nil && (id.ThreadID == "" || id.RolloutID == "" || identityByteBound(*id) > 4096) {
		id = nil // Observe never records such an identity as a candidate.
	}
	for key, request := range c.Requests {
		if request.TargetEpoch > c.Epoch || request.Overflow {
			continue
		}
		matches := id != nil && (id.ThreadID == key || id.RolloutID == key)
		candidate, recorded := request.Candidates[locator]
		if matches != recorded || matches && !reflect.DeepEqual(candidate.Identity, *id) {
			c.Failed = true
			return
		}
		if matches && !candidate.Member {
			candidate.Member = true
			request.Candidates[locator] = candidate
		}
	}
}

func (c *coverageInventory) finishValidation() bool {
	if c.Phase != coverageValidate || len(c.Validation) != 0 {
		return false
	}
	for _, entry := range c.Directories {
		if !entry.Validated {
			c.Failed = true
		}
	}
	c.Phase = coverageComplete
	if err := c.validateCompleteProof(); err != nil {
		c.Failed = true
	} else if !c.Failed {
		c.proofEpoch = c.Epoch
	}
	for key, request := range c.Requests {
		if request.TargetEpoch <= c.Epoch && !c.Failed {
			request.AttemptEpoch = c.Epoch
		}
		if request.TargetEpoch <= c.Epoch && !request.Overflow && !c.Failed {
			// Every listed rollout claiming this key was recorded and marked, so
			// an unmarked candidate names a locator that is no longer listed.
			for locator, candidate := range request.Candidates {
				if !candidate.Member {
					delete(request.Candidates, locator)
					c.factBound = 0
				}
			}
		}
		if request.TargetEpoch <= c.Epoch && !request.Overflow && !c.Failed {
			request.CompleteEpoch = c.Epoch
		} else {
			request.CompleteEpoch = 0
		}
		c.Requests[key] = request
	}
	return !c.Failed
}

func (c *coverageInventory) restart() {
	c.factBound = 0
	c.Epoch++
	c.proofEpoch = 0
	c.Phase = coverageObserve
	c.Failed = false
	c.MemberChecks = false
	c.Directories = map[string]coverageDirectory{}
	c.Validation = nil
	c.FinalOffset = 0
	for key, request := range c.Requests {
		request.Candidates = map[string]coverageCandidate{}
		request.CompleteEpoch = 0
		request.Overflow = false
		c.Requests[key] = request
	}
}

func directoryCoverageStamp(root, path string) string {
	info, err := os.Lstat(filepath.Join(root, path))
	if errors.Is(err, os.ErrNotExist) {
		sum := sha256.Sum256([]byte("absent"))
		return hex.EncodeToString(sum[:])
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return entryFingerprint("", info)
}

func advanceCoverageValidation(ctx context.Context, c *catalog, h *Health, adapter SourceAdapter, deadline time.Time, o Options) {
	coverage := c.Coverage
	for coverage.Phase == coverageValidate && len(coverage.Validation) > 0 && h.Entries < 2048 && time.Now().Before(deadline) && !scanStopped(ctx, o) {
		d := coverage.Validation[0]
		batch, err := adapter.Enumerate(ctx, d.Root, d.Path, d.Offset)
		if err != nil || batch.coverage == nil {
			coverage.Failed = true
			coverage.Validation = coverage.Validation[1:]
			continue
		}
		if !checkCoverageMembers(ctx, c, h, adapter, o.Rollouts, batch.Entries) {
			break // this batch is validated again on a later run
		}
		h.Entries += len(batch.Entries)
		coverage.validateBatch(d, *batch.coverage, batch.Complete)
		if batch.Complete {
			coverage.Validation = coverage.Validation[1:]
		} else {
			d.Offset = batch.Continuation
			coverage.Validation[0] = d
		}
	}
	if coverage.Phase == coverageValidate && len(coverage.Validation) == 0 {
		finishCoverageDirectoryCheck(ctx, coverage, h, deadline, o)
	}
}

// checkCoverageMembers rechecks each rollout in a validation batch against
// the requested candidates. A rollout whose stamp still matches its cached
// observation reuses that identity; a changed one has its header read again.
// It reports false, without failing coverage, when the probe budget is spent
// or a header read raced a write, so the batch is validated again later.
func checkCoverageMembers(ctx context.Context, c *catalog, h *Health, adapter SourceAdapter, rollouts *CodexRolloutLookup, entries []SourceEntry) bool {
	for _, entry := range entries {
		if entry.Source.Locator == "" {
			continue
		}
		if prior, hit := c.Cache[entry.Source.Locator]; hit && prior.SourceFingerprint == entry.CoverageFingerprint && prior.Size == entry.Fingerprint.Size && prior.Mtime == entry.Fingerprint.Mtime {
			c.Coverage.checkMember(entry.Source.Locator, prior.Observation.Identity)
			continue
		}
		if !discoveryProbeAvailable(h, rollouts) {
			return false
		}
		const headerCharge = int64(sourcefacts.HeaderBytes + 128<<10)
		if rollouts != nil && !rollouts.readBudget.Reserve(headerCharge) {
			return false
		}
		observation := adapter.Inspect(ctx, entry.Source)
		if rollouts != nil {
			rollouts.readBudget.Release(headerCharge)
			rollouts.probes++
		}
		h.Probes++
		h.Bytes += observation.Bytes
		h.NativeReadBytes += observation.NativeReadBytes
		h.NativeReadOperations += observation.NativeReadOperations
		if ctx.Err() != nil || observation.Outcome == outcomeChanged {
			// An append raced the header read; read it again next pass.
			return false
		}
		c.Coverage.checkMember(entry.Source.Locator, observation.Identity)
	}
	return true
}

func finishCoverageDirectoryCheck(ctx context.Context, c *coverageInventory, h *Health, deadline time.Time, o Options) {
	keys := make([]string, 0, len(c.Directories))
	for key := range c.Directories {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for c.FinalOffset < len(keys) && h.Entries < 2048 && time.Now().Before(deadline) && !scanStopped(ctx, o) {
		entry := c.Directories[keys[c.FinalOffset]]
		if directoryCoverageStamp(entry.Directory.Root, entry.Directory.Path) != entry.Stamp {
			c.Failed = true
		}
		c.FinalOffset++
		h.Entries++
	}
	if c.FinalOffset == len(keys) {
		c.finishValidation()
	}
}

// validateCompleteProof cross-checks the persisted epoch structure. A phase
// flag alone is never absence authority after damaged local state is restored.
func (c *coverageInventory) validateCompleteProof() error {
	if c.Failed {
		return nil
	}
	if c.Phase != coverageComplete {
		return nil
	}
	if len(c.Roots) == 0 || len(c.Directories) == 0 || len(c.Validation) != 0 || c.FinalOffset != len(c.Directories) {
		return errors.New("incomplete native coverage proof")
	}
	for _, root := range c.Roots {
		for _, path := range []string{"sessions", "archived_sessions"} {
			if _, present := c.Directories[filepath.Join(root, path)]; !present {
				return errors.New("native coverage root proof missing")
			}
		}
	}
	for key, entry := range c.Directories {
		if key != coverageKey(entry.Directory) || !entry.Complete || !entry.Validated || entry.ValidationDigest != entry.Digest || len(entry.Stamp) != 64 || len(entry.Digest) != 64 {
			return errors.New("native coverage directory proof inconsistent")
		}
		for _, value := range []string{entry.Stamp, entry.Digest} {
			raw, err := hex.DecodeString(value)
			if err != nil || len(raw) != sha256.Size {
				return errors.New("native coverage digest invalid")
			}
		}
	}
	return c.validateCompleteRequests()

}

func (c *coverageInventory) validateCompleteRequests() error {
	for id, request := range c.Requests {
		if request.TargetEpoch == 0 || request.AttemptEpoch > c.Epoch || request.DeliveredEpoch > request.AttemptEpoch || request.Order > c.Sequence {
			return errors.New("native coverage request scheduling inconsistent")
		}
		if request.CompleteEpoch != 0 && (request.CompleteEpoch != c.Epoch || request.TargetEpoch > c.Epoch || request.Overflow) {
			return errors.New("native coverage request proof inconsistent")
		}
		for path, candidate := range request.Candidates {
			if path != candidate.Source.Locator || candidate.Source.Kind != "" || !slices.Contains(c.Roots, candidate.Source.Root) || !filepath.IsAbs(path) || filepath.Clean(path) != path || (!local.PathWithin(path, filepath.Join(candidate.Source.Root, "sessions")) && !local.PathWithin(path, filepath.Join(candidate.Source.Root, "archived_sessions"))) || candidate.Identity.ThreadID != id && candidate.Identity.RolloutID != id {
				return errors.New("native coverage candidate proof inconsistent")
			}
		}
	}
	return nil
}
