package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const metadataBytes = 16 << 20

const metadataHeaderBytes = 64 << 10

const metadataScratch = 3 * metadataHeaderBytes

type metadataCounts struct {
	entries     int
	physical    int
	directories int
	headers     int
	sweeps      int
	stats       int
	rootOpens   int
	fileOpens   int
	resolutions int
	joins       int
	steps       int
	requested   int64
	returned    int64
	operations  int64
}

type metadataFact struct {
	source   SourceDescriptor
	identity codexmeta.CodexIdentity
	stamp    metadataStamp
	header   [32]byte
	stable   [32]byte
}

type metadataDirectory struct {
	dir    directory
	stamp  string
	digest string
}

type metadataRoot struct {
	home string
	path string
	info os.FileInfo
}

type metadataCursor struct {
	file    *os.File
	reader  io.ReaderAt
	source  SourceDescriptor
	info    os.FileInfo
	line    []byte
	scratch int64
}

type metadataInventory struct {
	owner          *CodexRolloutLookup
	started        bool
	complete       bool
	failure        error
	roots          []metadataRoot
	queue          []directory
	batch          []SourceEntry
	next           directory
	directories    []metadataDirectory
	directoryIndex map[string]int
	facts          []metadataFact
	threads        map[string][]int
	rollouts       map[string][]int
	cursor         *metadataCursor
	charge         int64
	headerBytes    int64
	batchCharge    int64
	remaining      time.Duration
	deadline       time.Time
	interned       map[string]string
	counts         metadataCounts
}

// MetadataInventory requests a full pass-local metadata epoch without reading
// native files. Its private view supplies caller-owned validation slices and
// physical evidence, never capture or ancestor-content permission.
func (l *CodexRolloutLookup) MetadataInventory() agentapi.CodexRolloutLookup {
	if l.metadata == nil {
		l.metadata = &metadataInventory{owner: l}
	}
	return l.metadata
}

// metadataTypeSize bounds the conversion before reserving concrete backing slots.
func metadataTypeSize[T any]() int64 {
	size := reflect.TypeFor[T]().Size()
	if size > 1<<20 {
		panic("unexpected metadata representation size")
	}
	return int64(size)
}

// metadataStamp retains exact filesystem identity without retaining an allocated
// FileInfo and its platform Stat_t behind every metadata fact.
type metadataStamp struct {
	dev   uint64
	ino   uint64
	size  int64
	mtime int64
	mode  os.FileMode
}

func metadataDevice(value any) (uint64, bool) {
	switch dev := value.(type) {
	case int32:
		if dev < 0 {
			return 0, false
		}
		return uint64(dev), true
	case uint64:
		return dev, true
	default:
		return 0, false
	}
}

func stampMetadata(info os.FileInfo) metadataStamp {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return metadataStamp{}
	}
	dev, valid := metadataDevice(stat.Dev)
	if !valid {
		return metadataStamp{}
	}
	return metadataStamp{dev, uint64(stat.Ino), info.Size(), info.ModTime().UnixNano(), info.Mode()}
}

func (s metadataStamp) same(info os.FileInfo) bool {
	return info != nil && s.ino != 0 && s == stampMetadata(info)
}

func (m *metadataInventory) intern(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if prior, ok := m.interned[value]; ok {
		return prior, nil
	}
	// String storage rounds to allocator size; 96 bytes conservatively covers
	// one map entry, bucket/directory growth and key/value headers.
	if !m.reserve(96 + int64((len(value)+15)&^15)) {
		return "", metadataLimit()
	}
	m.interned[value] = value
	return value, nil
}

func (m *metadataInventory) internIdentity(id codexmeta.CodexIdentity) (codexmeta.CodexIdentity, error) {
	for _, value := range []*string{&id.ThreadID, &id.RootID, &id.ParentID, &id.ForkID, &id.RolloutID} {
		interned, err := m.intern(*value)
		if err != nil {
			return id, err
		}
		*value = interned
	}
	mode, err := m.intern(string(id.HistoryMode))
	if err != nil {
		return id, err
	}
	id.HistoryMode = codexmeta.HistoryMode(mode)
	if id.HistoryBase != nil {
		value := *id.HistoryBase
		interned, err := m.intern(value.RolloutID)
		if err != nil {
			return id, err
		}
		value.RolloutID = interned
		id.HistoryBase = &value
		if !m.reserve(64) {
			return id, metadataLimit()
		}
	}
	if id.ForkOrdinal != nil || id.SubagentOrdinal != nil {
		if !m.reserve(32) {
			return id, metadataLimit()
		}
	}
	return id, nil
}

// beginOperation charges one fixed full-epoch allowance, independently of the
// ordinary requested lane. Slice renewal and Duration never replenish it.
func (m *metadataInventory) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if m.owner.closed {
		return ctx, nil, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	started := time.Now()
	if !m.started {
		if err := m.start(ctx); err != nil {
			m.remaining -= time.Since(started)
			return ctx, nil, m.fail(err)
		}
	}
	if m.remaining <= 0 {
		return ctx, nil, m.fail(metadataLimit())
	}
	m.deadline = started.Add(m.remaining)
	operation, cancel := context.WithDeadline(ctx, m.deadline)
	return operation, func() { cancel(); m.remaining -= time.Since(started) }, nil
}

// operationFailure keeps caller cancellation distinct from the fixed owned limit.
func (m *metadataInventory) operationFailure(parent context.Context, err error) error {
	if canceled := parent.Err(); canceled != nil {
		return canceled
	}
	if !m.deadline.IsZero() && !time.Now().Before(m.deadline) {
		return m.fail(metadataLimit())
	}
	return err
}

func metadataUnavailable() error {
	return agentapi.Wrap(agentapi.Unavailable, errors.New("native metadata inventory incomplete"))
}

func metadataChanged() error {
	return agentapi.Wrap(agentapi.Changed, errors.New("native metadata inventory changed"))
}

func metadataLimit() error {
	return agentapi.Wrap(agentapi.Limit, errors.New("native metadata inventory limit"))
}

// reserve charges the inventory, which lives for the pass, against the
// shared ledger only (agentapi.PassCacheReserve holds its metadataBytes):
// it is no one session's, whichever session's validation built it.
func (m *metadataInventory) reserve(n int64) bool {
	if n < 0 || m.charge+m.headerBytes+n > metadataBytes || !m.owner.readBudget.ReserveUnscoped(n) {
		return false
	}
	m.charge += n
	return true
}

func (m *metadataInventory) closeCursor() error {
	if m.cursor == nil {
		return nil
	}
	err := m.cursor.file.Close()
	scratch := m.cursor.scratch
	m.cursor = nil
	m.owner.readBudget.ReleaseUnscoped(scratch)
	m.charge -= scratch
	return err
}

func (m *metadataInventory) close() error {
	err := m.closeCursor()
	m.owner.readBudget.ReleaseUnscoped(m.charge)
	m.charge = 0
	m.facts = nil
	m.batch = nil
	m.queue = nil
	m.threads = nil
	m.interned = nil
	m.rollouts = nil
	m.directories = nil
	m.directoryIndex = nil
	m.roots = nil
	m.next = directory{}
	m.batchCharge = 0
	return err
}

func (m *metadataInventory) fail(err error) error {
	m.complete = false
	m.failure = errors.Join(err, m.closeCursor())
	return m.failure
}

func (m *metadataInventory) rootCurrent(r metadataRoot) bool {
	m.counts.resolutions++
	resolved, err := filepath.EvalSymlinks(r.home)
	m.counts.stats++
	info, statErr := os.Lstat(r.path)
	return err == nil && statErr == nil && resolved == r.path && info.IsDir() && os.SameFile(r.info, info)
}

func (m *metadataInventory) rootsCurrent(ctx context.Context) error {
	for _, r := range m.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !m.rootCurrent(r) {
			return metadataChanged()
		}
	}
	return nil
}

func (m *metadataInventory) start(ctx context.Context) error {
	m.started = true
	m.remaining = 30 * time.Second
	m.interned = map[string]string{}
	// Reserve every backing slot before allocating; no geometric growth or old
	// backing array coexistence is omitted from the shared ledger.
	if !m.reserve(metadataTypeSize[metadataFact]()*16384 + 2048*256 + 128<<10) {
		return metadataLimit()
	}
	m.facts = make([]metadataFact, 0, 16384)
	m.queue = make([]directory, 0, 2048)
	m.directories = make([]metadataDirectory, 0, 2048)
	m.threads = map[string][]int{}
	m.rollouts = map[string][]int{}
	m.directoryIndex = map[string]int{}
	if len(m.owner.homes) > 16 {
		return metadataLimit()
	}
	uniqueRoots := 0
	for _, home := range m.owner.homes {
		if !filepath.IsAbs(home) || len(home) > 4096 {
			return metadataUnavailable()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		m.counts.resolutions++
		path, err := filepath.EvalSymlinks(home)
		m.counts.stats++
		info, statErr := os.Lstat(path)
		if err != nil || statErr != nil || !info.IsDir() {
			return metadataUnavailable()
		}
		// Census and targeted current queries share the constructor-approved set.
		if !slices.Contains(m.owner.roots, path) {
			return metadataChanged()
		}
		if !m.reserve(int64(len(home) + len(path) + 512)) {
			return metadataLimit()
		}
		duplicate := slices.ContainsFunc(m.roots, func(prior metadataRoot) bool { return prior.path == path })
		m.roots = append(m.roots, metadataRoot{home, path, info})
		if duplicate {
			continue
		} // Retain every spelling fence, enumerate each physical root once.
		uniqueRoots++
		for _, store := range (codexAdapter{}).InitialDirectories() {
			m.queue = append(m.queue, directory{Root: path, Path: store})
		}
	}
	if uniqueRoots != len(m.owner.roots) {
		return metadataChanged()
	}
	return nil
}

// open binds every parent descriptor, original home spelling and file observation.
// os.Root confinement alone would still permit an in-root intermediate symlink.
func (m *metadataInventory) open(source SourceDescriptor) (*os.File, error) {
	var authority *metadataRoot
	for i := range m.roots {
		if m.roots[i].path == source.Root {
			authority = &m.roots[i]
			break
		}
	}
	if authority == nil || !m.rootCurrent(*authority) {
		return nil, metadataChanged()
	}
	relative, err := filepath.Rel(source.Root, source.Locator)
	if err != nil || relative == "." || !local.PathWithin(source.Locator, source.Root) {
		return nil, metadataChanged()
	}
	m.counts.rootOpens++
	root, err := os.OpenRoot(source.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	m.counts.stats++
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(authority.info, rootInfo) {
		return nil, metadataChanged()
	}
	parts := strings.Split(relative, string(filepath.Separator))
	parent := root
	for _, part := range parts[:len(parts)-1] {
		m.counts.stats++
		before, err := parent.Lstat(part)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, metadataChanged()
		}
		m.counts.rootOpens++
		next, err := parent.OpenRoot(part)
		if err != nil {
			return nil, err
		}
		defer func() { _ = next.Close() }()
		m.counts.stats += 2
		after, err := next.Stat(".")
		named, e := parent.Lstat(part)
		if err != nil || e != nil || !sameMetadataInfo(before, after) || !sameMetadataInfo(before, named) {
			return nil, metadataChanged()
		}
		parent = next
	}
	name := parts[len(parts)-1]
	m.counts.stats++
	before, err := parent.Lstat(name)
	if err != nil || !before.Mode().IsRegular() {
		return nil, metadataChanged()
	}
	m.counts.fileOpens++
	f, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	m.counts.stats += 2
	after, err := f.Stat()
	named, e := parent.Lstat(name)
	if err != nil || e != nil || !transcriptio.SameObservation(before, after) || !transcriptio.SameObservation(before, named) || !m.rootCurrent(*authority) {
		_ = f.Close()
		return nil, metadataChanged()
	}
	return f, nil
}

func sameMetadataInfo(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// step performs at most 1024 application header reads and yields within 100ms.
// One exact descriptor and one bounded partial line survive a successful yield.
func (m *metadataInventory) step(ctx context.Context) error {
	m.counts.steps++
	until := time.Now().Add(100 * time.Millisecond)
	operations := 0
	if err := m.rootsCurrent(ctx); err != nil {
		return err
	}
	for operations < 1024 && time.Now().Before(until) && !m.complete {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.cursor != nil {
			operations++
			if err := m.readHeaderByte(ctx); err != nil {
				return err
			}
			continue
		}
		if len(m.batch) > 0 {
			entry := m.batch[0]
			m.batch[0] = SourceEntry{}
			m.batch = m.batch[1:]
			if err := m.accountEntry(entry); err != nil {
				return err
			}
			if entry.Directory != "" {
				if len(m.directories)+len(m.queue) >= 2048 {
					return metadataLimit()
				}
				m.queue = append(m.queue, directory{Root: m.next.Root, Path: entry.Directory})
			} else if entry.Source.Locator != "" {
				if !m.reserve(metadataScratch) {
					return metadataLimit()
				}
				f, err := m.open(entry.Source)
				if err != nil {
					m.owner.readBudget.ReleaseUnscoped(metadataScratch)
					m.charge -= metadataScratch
					return err
				}
				m.counts.stats++
				info, err := f.Stat()
				if err != nil {
					_ = f.Close()
					m.owner.readBudget.ReleaseUnscoped(metadataScratch)
					m.charge -= metadataScratch
					return err
				}
				m.cursor = &metadataCursor{file: f, reader: f, source: entry.Source, info: info, line: make([]byte, 0, metadataHeaderBytes), scratch: metadataScratch}
			}
			continue
		}
		if m.batchCharge != 0 {
			m.owner.readBudget.ReleaseUnscoped(m.batchCharge)
			m.charge -= m.batchCharge
			m.batchCharge = 0
		}
		if len(m.queue) == 0 {
			m.complete = true
			break
		}
		d := m.queue[0]
		m.queue = m.queue[1:]
		// The same bounded adapter/directory worker supplies ordinary requested and
		// explicit full metadata lanes. The full lane starts its own complete round,
		// never promoting the ordinary cache or requested proof to an epoch.
		batch, err := (codexAdapter{metadataCounts: &m.counts, metadataReserve: func(n int64) bool {
			if !m.reserve(n) {
				return false
			}
			m.batchCharge += n
			return true
		}}).Enumerate(ctx, d.Root, d.Path, d.Offset)
		if err != nil && agentapi.Failure(err) == agentapi.Limit {
			return err
		}
		if err != nil || batch.coverage == nil || batch.coverage.Unavailable {
			return metadataUnavailable()
		}
		key := d.Root + "\x00" + d.Path
		i, found := m.directoryIndex[key]
		if !found {
			if len(m.directories) >= 2048 || !m.reserve(int64(len(key)+512)) {
				return metadataLimit()
			}
			i = len(m.directories)
			m.directoryIndex[key] = i
			m.directories = append(m.directories, metadataDirectory{dir: directory{Root: d.Root, Path: d.Path}, stamp: batch.coverage.Stamp})
			m.counts.directories++
		}
		proof := &m.directories[i]
		if proof.stamp != batch.coverage.Stamp {
			return metadataChanged()
		}
		proof.digest = appendCoverageDigest(proof.digest, batch.coverage.Entries)
		m.batch = batch.Entries
		m.next = d
		if !batch.Complete {
			d.Offset = batch.Continuation
			m.queue = append(m.queue, d)
		}
	}
	return nil
}

// accountEntry keeps physical acquisition distinct from bounded fingerprints.
func (m *metadataInventory) accountEntry(entry SourceEntry) error {
	m.counts.entries++
	// Fingerprints include directories and unrelated entries; preserve a
	// total traversal ceiling separately from physical candidate acquisition.
	if m.counts.entries > 16384+2048 {
		return metadataLimit()
	}
	if entry.Source.Locator != "" || entry.unknownMetadata {
		if m.counts.physical >= 16384 {
			return metadataLimit()
		}
		m.counts.physical++
	}
	if entry.unsafeMetadata || entry.unknownMetadata {
		return metadataUnavailable()
	}
	return nil
}

func (m *metadataInventory) readHeaderByte(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c := m.cursor
	if len(c.line) >= metadataHeaderBytes || m.headerBytes+m.charge+1 > metadataBytes {
		return metadataLimit()
	}
	var b [1]byte
	m.counts.requested++
	m.counts.operations++
	n, err := c.reader.ReadAt(b[:], int64(len(c.line)))
	m.counts.returned += int64(n)
	m.headerBytes += int64(n)
	if err != nil || n != 1 {
		return metadataUnavailable()
	}
	c.line = append(c.line, b[0])
	if b[0] == '\n' {
		if err := m.finishHeader(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (m *metadataInventory) finishHeader(ctx context.Context) error {
	c := m.cursor
	if err := ctx.Err(); err != nil {
		return err
	}
	m.counts.stats += 2
	after, err := c.file.Stat()
	named, e := os.Lstat(c.source.Locator)
	if err != nil || e != nil || !sameMetadataInfo(c.info, after) || !sameMetadataInfo(c.info, named) {
		return metadataChanged()
	}
	if err := m.rootsCurrent(ctx); err != nil {
		return err
	}
	// Reserve transient representations before decoding. The fixed schema wraps
	// arbitrary Git/source/subagent RawMessage maps; related records can bypass
	// ordinary producer caps. Per input byte, allowances cover map slots (52),
	// raw copies (6), decoded strings (16), encoder buffers (24), and scanner
	// state (16): 114 rounded to 128, plus the fixed header/schema allowance.
	// This is application representation accounting, not a process heap bound.
	needed := max(int64(metadataScratch), int64(metadataHeaderBytes)+128*int64(len(c.line)))
	if !m.reserve(needed - c.scratch) {
		return metadataLimit()
	}
	c.scratch = needed
	meta, created, found, err := codexmeta.ParseCodexMeta(c.line)
	if err != nil || !found || created.IsZero() {
		return metadataUnavailable()
	}
	identity, outcome := meta.Identity(c.source.Locator)
	capture := meta.CaptureOutcome(c.source.Locator)
	if outcome != "" || (capture != codexmeta.NativeFormat && capture != codexmeta.ChildHistoryPending && capture != codexmeta.ForkHistoryPending && capture != codexmeta.RelatedHistoryPending) {
		return metadataUnavailable()
	}
	// Stable logical facts omit revision-local physical pagination information.
	logical := identity
	logical.RolloutID = ""
	logical.HistoryBase = nil
	logical.HistoryMode = ""
	stable, err := json.Marshal(struct {
		Identity codexmeta.CodexIdentity `json:"Identity"`
		Created  time.Time               `json:"Created"`
		Cwd      string                  `json:"Cwd"`
		Version  string                  `json:"Version"`
		Producer string                  `json:"Producer"`
		Source   json.RawMessage         `json:"Source"`
		Git      codexmeta.GitInfo       `json:"Git"`
	}{logical, created, meta.Cwd, meta.Version, meta.Originator, meta.Source, meta.Git})
	if err != nil {
		return err
	}
	stableHash := sha256.Sum256(stable)
	m.counts.joins++
	if indices := m.threads[identity.ThreadID]; len(indices) > 0 && m.facts[indices[0]].stable != stableHash {
		return metadataUnavailable()
	}
	// A physical UUID has one immutable identity, even across logical buckets.
	// Separate rollout revisions may have different pagination; copies of one
	// rollout cannot. Metadata agreement does not compare or authorize bodies.
	rolloutKey := strings.ToLower(identity.RolloutID)
	if indices := m.rollouts[rolloutKey]; len(indices) > 0 {
		prior := m.facts[indices[0]]
		if prior.stable != stableHash || !sameMetadataPagination(prior.identity, identity) {
			return metadataUnavailable()
		}
	}
	// The fact array is reserved at start. Per-entry allowance covers two
	// map/index entries, slice capacity/allocator rounding and the unique locator.
	// Parsed temporary metadata uses the separately reserved bounded scratch.
	if !m.reserve(192 + int64((len(c.source.Locator)+15)&^15)) {
		return metadataLimit()
	}
	var internErr error
	identity, internErr = m.internIdentity(identity)
	if internErr != nil {
		return internErr
	}
	rolloutKey, internErr = m.intern(rolloutKey)
	if internErr != nil {
		return internErr
	}
	c.source.StableKey = identity.RolloutID

	i := len(m.facts)
	m.facts = append(m.facts, metadataFact{c.source, identity, stampMetadata(c.info), sha256.Sum256(c.line), stableHash})
	m.threads[identity.ThreadID] = append(m.threads[identity.ThreadID], i)
	m.rollouts[rolloutKey] = append(m.rollouts[rolloutKey], i)
	m.counts.headers++
	m.counts.joins++
	return m.closeCursor()
}

// sameMetadataPagination compares physical links without equating distinct rollouts.
func sameMetadataPagination(a, b codexmeta.CodexIdentity) bool {
	mode := func(value codexmeta.HistoryMode) codexmeta.HistoryMode {
		if value == "" {
			return codexmeta.CodexHistoryLegacy
		}
		return value
	}
	if mode(a.HistoryMode) != mode(b.HistoryMode) {
		return false
	}
	if a.HistoryBase == nil || b.HistoryBase == nil {
		return a.HistoryBase == b.HistoryBase
	}
	return strings.EqualFold(a.HistoryBase.RolloutID, b.HistoryBase.RolloutID) &&
		a.HistoryBase.EndOrdinal == b.HistoryBase.EndOrdinal &&
		a.HistoryBase.EndByteOffset == b.HistoryBase.EndByteOffset
}

func (m *metadataInventory) ensure(ctx context.Context) error {
	if m.failure != nil {
		return m.failure
	}
	if !m.started {
		if err := m.start(ctx); err != nil {
			return m.fail(err)
		}
	}
	for !m.complete {
		if err := m.step(ctx); err != nil {
			return m.fail(err)
		}
	}
	return nil
}

func (m *metadataInventory) validate(ctx context.Context) error {
	m.counts.sweeps++
	if err := m.rootsCurrent(ctx); err != nil {
		return err
	}
	for _, proof := range m.directories {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Recheck membership on the captured directory and every physical source
		// below it in one sweep. Directory identity/mtime/size fences additions,
		// removals and moves; individual file observations fence append/rewrite.
		// Irrelevant file payload changes cannot alter native metadata membership.
		m.counts.stats++
		if directoryCoverageStamp(proof.dir.Root, proof.dir.Path) != proof.stamp {
			return metadataChanged()
		}
	}
	for _, fact := range m.facts {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.counts.stats++
		info, err := os.Lstat(fact.source.Locator)
		if err != nil || !fact.stamp.same(info) {
			return metadataChanged()
		}
	}
	return m.rootsCurrent(ctx)
}

func (m *metadataInventory) BeginValidationSlice(ctx context.Context, limits agentapi.CodexValidationLimits) (agentapi.CodexRolloutSlice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.Steps <= 0 {
		limits.Steps = 512
	}
	if limits.Duration <= 0 {
		limits.Duration = 30 * time.Second
	}
	if limits.Steps > 512 || limits.Duration > 60*time.Second {
		return nil, metadataLimit()
	}
	operation, done, err := m.beginOperation(ctx)
	if err != nil {
		if ctx.Err() != nil || m.owner.closed {
			return nil, err
		}
		return &metadataSlice{inventory: m, remaining: limits.Steps, expires: time.Now().Add(limits.Duration), failure: err}, nil
	}
	defer done()
	err = m.ensure(operation)
	if err == nil {
		err = m.validate(operation)
	}
	err = m.operationFailure(ctx, err)
	// A caller-local failed sweep stays in its slice, but exhaustion of the
	// fixed epoch leaves incomplete evidence even after enumeration finished.
	if err != nil && (m.failure != nil || (ctx.Err() == nil && agentapi.Failure(err) == agentapi.Limit)) {
		err = m.fail(err)
	}
	return &metadataSlice{inventory: m, remaining: limits.Steps, expires: time.Now().Add(limits.Duration), failure: err}, nil
}

func (m *metadataInventory) NativeReadBudget() *agentapi.NativeReadBudget { return m.owner.readBudget }

func (m *metadataInventory) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	s, e := m.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
	if e != nil {
		return agentapi.CodexRolloutSet{}, e
	}
	defer func() { _ = s.Close() }()
	return s.Thread(ctx, id)
}

func (m *metadataInventory) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	s, e := m.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
	if e != nil {
		return nil, e
	}
	defer func() { _ = s.Close() }()
	return s.Rollout(ctx, id)
}

func (m *metadataInventory) Check(ctx context.Context, id, revision string) error {
	s, e := m.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
	if e != nil {
		return e
	}
	defer func() { _ = s.Close() }()
	return s.Check(ctx, id, revision)
}

func (m *metadataInventory) current(ctx context.Context, id string) (*agentapi.SourceRef, error) {
	var current *agentapi.SourceRef
	for _, root := range m.owner.roots {
		view, err := m.owner.indexDeadline(ctx, root, true, m.deadline)
		if err != nil {
			if agentapi.Failure(err) == agentapi.Limit {
				return nil, err
			}
			if _, statErr := os.Lstat(filepath.Join(root, "state_5.sqlite")); !errors.Is(statErr, os.ErrNotExist) {
				return nil, metadataUnavailable()
			}
			continue
		}
		m.owner.queries += 2
		path, found, err := currentLocator(ctx, view.db, id)
		if err != nil {
			return nil, metadataUnavailable()
		}
		if !found {
			continue
		}
		valid := false
		for _, i := range m.threads[id] {
			if m.facts[i].source.Locator == path && m.facts[i].source.Root == root {
				valid = true
				break
			}
		}
		if !valid && local.PathWithin(path, root) && currentLocatorMissing(path) {
			continue
		}
		if !valid || (current != nil && current.Path != path) {
			return nil, metadataChanged()
		}
		current = &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id}
	}
	return current, nil
}

func (m *metadataInventory) refs(indices []int) []agentapi.SourceRef {
	refs := make([]agentapi.SourceRef, 0, len(indices))
	for _, i := range indices {
		fact := m.facts[i]
		refs = append(refs, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: fact.source.Locator, Key: fact.identity.ThreadID})
	}
	slices.SortFunc(refs, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	return refs
}

func (m *metadataInventory) thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	current, err := m.current(ctx, id)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}

	hash := sha256.New()
	for _, i := range m.threads[id] {
		f := m.facts[i]
		_, _ = io.WriteString(hash, f.source.Locator)
		_, _ = hash.Write(f.header[:])
		_, _ = hash.Write(f.stable[:])
	}
	if current != nil {
		_, _ = io.WriteString(hash, current.Path)
	}
	return agentapi.CodexRolloutSet{Current: current, Complete: m.complete, Revision: hex.EncodeToString(hash.Sum(nil))}, nil
}

type metadataSlice struct {
	inventory *metadataInventory
	remaining int
	expires   time.Time
	failure   error
	closed    bool
	charge    int64
}

func (s *metadataSlice) Valid(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return agentapi.ErrClosed
	}
	if s.remaining <= 0 || !time.Now().Before(s.expires) {
		return metadataLimit()
	}
	if s.inventory.owner.closed {
		return agentapi.ErrClosed
	}
	return s.failure
}

func (s *metadataSlice) step(ctx context.Context) error {
	if err := s.Valid(ctx); err != nil {
		return err
	}
	s.remaining--
	return nil
}

func (s *metadataSlice) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if err := s.step(ctx); err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	op, done, err := s.inventory.beginOperation(ctx)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	defer done()
	set, err := s.inventory.thread(op, id)
	err = s.inventory.operationFailure(ctx, err)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	refs, err := s.refs(s.inventory.threads[id])
	err = s.inventory.operationFailure(ctx, err)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	set.Candidates = refs
	return set, nil
}

func (s *metadataSlice) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	if err := s.step(ctx); err != nil {
		return nil, err
	}
	_, done, err := s.inventory.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	var indices []int
	// Physical UUIDs have a fixed width; unknown inputs need no copied key.
	if len(id) == 36 {
		indices = s.inventory.rollouts[strings.ToLower(id)]
	}
	refs, err := s.refs(indices)
	if err := s.inventory.operationFailure(ctx, err); err != nil {
		return nil, err
	}
	return refs, nil
}

func (s *metadataSlice) Check(ctx context.Context, id, revision string) error {
	if err := s.step(ctx); err != nil {
		return err
	}
	op, done, err := s.inventory.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer done()
	set, err := s.inventory.thread(op, id)
	err = s.inventory.operationFailure(ctx, err)
	if err != nil {
		return err
	}
	if revision == "" || revision != set.Revision {
		return metadataChanged()
	}
	return nil
}

func (s *metadataSlice) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if !s.inventory.owner.closed {
		s.inventory.owner.readBudget.ReleaseUnscoped(s.charge)
		s.inventory.charge -= s.charge
	}
	s.charge = 0
	return nil
}

func (s *metadataSlice) refs(indices []int) ([]agentapi.SourceRef, error) {
	charge := int64(len(indices)) * metadataTypeSize[agentapi.SourceRef]()
	if !s.inventory.reserve(charge) {
		return nil, metadataLimit()
	}
	s.charge += charge
	return s.inventory.refs(indices), nil
}

func (s *metadataSlice) NativeReadBudget() *agentapi.NativeReadBudget {
	return s.inventory.owner.readBudget
}
