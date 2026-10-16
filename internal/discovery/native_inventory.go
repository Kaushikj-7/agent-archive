package discovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// NativeLocatorValidator checks only provisional strict ordinary startup. It
// receives the coordinator's counted confined filesystem and shared ledger;
// successful filtering and guarded state commit remain independent requirements.
type NativeLocatorValidator func(context.Context, agentapi.SourceEnvironment, agentapi.SourceRef, string) error

// NativeInventoryCounts accounts application operations, not physical disk I/O.
// Currentness deliberately costs O(selected owners * completed directories).
type NativeInventoryCounts struct {
	Names            int64         `json:"Names"`
	DirentBytes      int64         `json:"DirentBytes"`
	Stats            int64         `json:"Stats"`
	Opens            int64         `json:"Opens"`
	HeaderBytes      int64         `json:"HeaderBytes"`
	HeaderReads      int64         `json:"HeaderReads"`
	Probes           int64         `json:"Probes"`
	UncommittedNames int64         `json:"UncommittedNames"`
	PageReads        int64         `json:"PageReads"`
	GenerationChecks int64         `json:"GenerationChecks"`
	Sweeps           int64         `json:"Sweeps"`
	Selections       int64         `json:"Selections"`
	Checks           int64         `json:"Checks"`
	AcquireStats     int64         `json:"AcquireStats"`
	AcquireOpens     int64         `json:"AcquireOpens"`
	SweepStats       int64         `json:"SweepStats"`
	SweepOpens       int64         `json:"SweepOpens"`
	CandidateStats   int64         `json:"CandidateStats"`
	CandidateOpens   int64         `json:"CandidateOpens"`
	Active           time.Duration `json:"Active"`
}

type nativeGeneration struct {
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	Mode      uint32 `json:"mode"`
	Size      int64  `json:"size"`
	Mtime     int64  `json:"mtime"`
	CtimeSec  int64  `json:"ctime_sec"`
	CtimeNano int64  `json:"ctime_nano"`
}

type nativePageKind string

const (
	nativeCompletePage nativePageKind = "COMPLETEPAGE"
	nativeEOF          nativePageKind = "EOF"
)

type nativePage struct {
	Kind  nativePageKind `json:"kind"`
	Start int64          `json:"start"`
	End   int64          `json:"end"`
	Names int            `json:"names"`
	Bytes int            `json:"bytes"`
}

type nativeDirectory struct {
	Path       string           `json:"path"`
	Generation nativeGeneration `json:"generation"`
	Pages      []nativePage     `json:"pages"`
	Children   []string         `json:"children"`
	Matches    []string         `json:"matches"`
}

type nativeProgress struct {
	Version     int                `json:"version"`
	Layout      string             `json:"layout"`
	Classifier  int                `json:"classifier"`
	Root        archive.NativeRoot `json:"root"`
	Query       string             `json:"query"`
	Owners      []string           `json:"owners"`
	Next        int                `json:"next"`
	Directories []nativeDirectory  `json:"directories"`
}

// NativeInventory is one serial pass-owned shared namespace census. Durable
// pages are trusted installation-produced historical observations, never current
// uniqueness or consent. Only live checks mint tokens, kept in this object.
type NativeInventory struct {
	ctx       context.Context
	root      archive.NativeRoot
	budget    *agentapi.NativeReadBudget
	validate  NativeLocatorValidator
	path      string
	expected  []byte
	progress  nativeProgress
	eligible  []string
	counts    NativeInventoryCounts
	remaining time.Duration
	deadline  time.Time
	charge    int64
	tokens    map[string]nativeSelection
	nonce     string
	stopped   bool
	closed    bool
	err       error
}

type nativeSelection struct {
	token    string
	relative string
	stamp    nativeGeneration
}

// NewNativeInventory restores an exact historical cohort and freezes current eligibility.
// The caller owns the collector lock and must Close after source leases/passes.
// No native traversal occurs when no supported pending queries exist.
func NewNativeInventory(ctx context.Context, home string, roots []archive.NativeRoot, queries []agentapi.LocatorRequest, budget *agentapi.NativeReadBudget, validate NativeLocatorValidator) (*NativeInventory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	constructorStart := time.Now()
	l := &NativeInventory{ctx: ctx, path: filepath.Join(home, "discovery-catalog.json"), budget: budget, validate: validate, remaining: Budget, tokens: map[string]nativeSelection{}}
	if budget == nil || validate == nil {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	for _, root := range roots {
		if root.Agent == "muse" {
			if l.root.Agent != "" || root.Validate() != nil {
				return nil, agentapi.ErrInvalidSourceBinding
			}
			l.root = root
		}
	}
	if len(queries) > maxCatalog {
		return nil, errCatalogTooLarge
	}
	valid := 0
	for _, q := range queries {
		if q.Root != l.root || q.Parent != nil {
			return nil, agentapi.ErrInvalidSourceBinding
		}
		if nativeOwner(q.NativeID) {
			valid++
		}
	}
	if valid == 0 {
		return l, nil
	}
	defer func() { elapsed := time.Since(constructorStart); l.remaining -= elapsed; l.counts.Active += elapsed }()
	// Preflight bounds directories/pages/owners/edges before typed allocation.
	// Within existing cache headroom: <=1MiB wire implies <=3MiB total for typed
	// vectors (pinned capacity/cell bounds) and decoded string payload. Keep <=1MiB expected
	// member, <=2MiB encoder/copy or decoder buffers, <=1MiB validation maps,
	// <=128KiB current owner vector and <=128KiB compact live-token state.
	// One tentative <=256-name page and fixed scratch fit the remaining room.
	// Whole-catalog read/map/merge scratch is separately reserved before I/O.
	// Tests pin the typed cell/growth and shared 84+8+query composition bounds.
	if !l.reserve(8 * (maxCatalogBytes / 32)) {
		return nil, errCatalogBudget
	}
	owners := make([]string, 0, valid)
	for _, q := range queries {
		if nativeOwner(q.NativeID) {
			owners = append(owners, q.NativeID)
		}
	}
	sort.Strings(owners)
	owners = slices.Compact(owners)
	release, err := nativeCatalogScratch(l.path, 0, budget)
	if err != nil {
		return nil, l.failConstruction(err)
	}
	raw, err := boundedCatalogBytes(l.path)
	defer release()
	if ctx.Err() != nil {
		return nil, l.failConstruction(ctx.Err())
	}
	if err != nil && !errors.Is(err, errCatalogTooLarge) {
		return nil, l.failConstruction(err)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		if len(object["native_inventory"]) > maxCatalogBytes/32 {
			return nil, l.failConstruction(errCatalogTooLarge)
		}
		l.expected = object["native_inventory"]
	}
	sum := sha256.Sum256([]byte(strings.Join(owners, "\x00")))
	query := hex.EncodeToString(sum[:])
	restored, ok := decodeNativeProgress(l.expected)
	l.eligible = owners
	if ok && string(object["version"]) == "6" && restored.Root == l.root {
		l.progress = restored
		l.skipInactive()
		if l.progress.Next == len(l.progress.Owners) {
			if restored.Query == query && slices.Equal(restored.Owners, owners) {
				l.progress.Next = 0
			} else {
				l.reset(owners, query)
			}
		}
	} else {
		l.reset(owners, query)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, l.failConstruction(err)
	}
	l.nonce = hex.EncodeToString(nonce[:])
	return l, nil
}

func (l *NativeInventory) reserve(n int64) bool {
	if l.budget.ReserveUnscoped(n) {
		l.charge += n
		return true
	}
	return false
}

func (l *NativeInventory) reset(owners []string, query string) {
	l.progress = nativeProgress{Version: 1, Layout: "muse-root-v1", Classifier: 1, Root: l.root, Query: query, Owners: owners, Directories: []nativeDirectory{{Path: "."}}}
	clear(l.tokens)
}

func (l *NativeInventory) resetCoverage() {
	next := l.progress.Next
	l.reset(l.progress.Owners, l.progress.Query)
	l.progress.Next = next
}

// Counts reports all work performed so far, including rejected names and checks.
func (l *NativeInventory) Counts() NativeInventoryCounts { return l.counts }

func nativeOwner(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func nativeChild(parent, name string) bool {
	if strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return false
	}
	depth := 0
	if parent != "." {
		depth = len(strings.Split(parent, "/"))
	}
	switch depth {
	case 0:
		return len(name) == 4 && digits(name)
	case 1:
		return len(name) == 2 && name >= "01" && name <= "12"
	case 2:
		_, err := time.Parse("2006/01/02", parent+"/"+name)
		return err == nil
	}
	return false
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func nativeDepth(path string) int {
	if path == "." {
		return 0
	}
	return len(strings.Split(path, "/"))
}

func nativeDirPath(path string) bool {
	if path == "." {
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) > 3 {
		return false
	}
	parent := "."
	for _, p := range parts {
		if !nativeChild(parent, p) {
			return false
		}
		if parent == "." {
			parent = p
		} else {
			parent += "/" + p
		}
	}
	return true
}

func completeNative(d nativeDirectory) bool {
	return len(d.Pages) > 0 && d.Pages[len(d.Pages)-1].Kind == nativeEOF
}

func decodeNativeProgress(raw []byte) (nativeProgress, bool) {
	var p nativeProgress
	if len(raw) == 0 || len(raw) > maxCatalogBytes/32 || !nativeShape(raw) {
		return p, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil {
		return nativeProgress{}, false
	}
	return p, validNativeProgress(p)
}

func validNativeProgress(p nativeProgress) bool {
	if p.Version != 1 || p.Layout != "muse-root-v1" || p.Classifier != 1 || p.Root.Validate() != nil || p.Root.Agent != "muse" || len(p.Query) != 64 || len(p.Owners) == 0 || len(p.Owners) > maxCatalog || p.Next < 0 || p.Next > len(p.Owners) || len(p.Directories) == 0 || len(p.Directories) > maxDirectories {
		return false
	}
	if !validNativeScope(p) {
		return false
	}
	seen := map[string]bool{}
	edges := map[string]bool{}
	pages, matches := 0, 0
	for i, d := range p.Directories {
		matches += len(d.Matches)
		if matches > maxCatalog {
			return false
		}
		if !validNativeDirectory(d, i, seen, edges, &pages, p.Owners) {
			return false
		}
	}
	return len(seen) == len(edges)+1
}

func validNativeScope(p nativeProgress) bool {
	for i, id := range p.Owners {
		if !nativeOwner(id) || i > 0 && p.Owners[i-1] >= id {
			return false
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(p.Owners, "\x00")))
	return hex.EncodeToString(sum[:]) == p.Query
}

func validNativePage(page nativePage, index, last int, offset int64) bool {
	if page.Start != offset || page.Start < 0 || page.End < 0 || index < last && page.Kind == nativeEOF {
		return false
	}
	if page.Kind == nativeEOF {
		return page.Bytes == 0 && page.Names == 0 && page.End == page.Start
	}
	return page.Kind == nativeCompletePage && page.Bytes > 0 && page.Bytes <= 4096 && page.Names >= 0 && page.Names <= 256 && page.Names <= page.Bytes/16 && page.End != page.Start
}

func validNativePages(d nativeDirectory, pages *int) (int, bool) {
	if len(d.Pages) > 0 && (d.Generation.Inode == 0 || !os.FileMode(d.Generation.Mode).IsDir() || d.Generation.Mode&uint32(os.ModeSymlink) != 0 || d.Generation.CtimeSec == 0 || d.Generation.CtimeNano < 0 || d.Generation.CtimeNano >= 1_000_000_000) {
		return 0, false
	}
	offset := int64(0)
	names := 0
	for j, page := range d.Pages {
		*pages++
		if *pages > maxCatalog || !validNativePage(page, j, len(d.Pages)-1, offset) {
			return 0, false
		}
		offset = page.End
		names += page.Names
	}
	return names, true
}

func validNativeDirectory(d nativeDirectory, index int, seen, edges map[string]bool, pages *int, owners []string) bool {
	if !nativeDirPath(d.Path) || seen[d.Path] || index == 0 && d.Path != "." || index > 0 && !edges[d.Path] || len(d.Children) > maxDirectories || len(d.Matches) > maxCatalog {
		return false
	}
	seen[d.Path] = true
	names, ok := validNativePages(d, pages)
	if !ok || len(d.Children)+len(d.Matches) > names {
		return false
	}
	for _, child := range d.Children {
		if !nativeChild(d.Path, filepath.Base(child)) || filepath.ToSlash(filepath.Dir(child)) != d.Path || edges[child] {
			return false
		}
		edges[child] = true
	}
	matches := map[string]bool{}
	for _, id := range d.Matches {
		if nativeDepth(d.Path) != 3 || !slices.Contains(owners, id) || matches[id] {
			return false
		}
		matches[id] = true
	}
	return true
}

func (l *NativeInventory) allowed(ctx context.Context) error {
	if l.closed {
		return agentapi.ErrClosed
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.remaining <= Budget/4 || !l.deadline.IsZero() && time.Now().After(l.deadline) {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("native inventory allowance spent"))
	}
	return nil
}

func (l *NativeInventory) timed(start time.Time) {
	elapsed := time.Since(start)
	l.remaining -= elapsed
	l.counts.Active += elapsed
	l.deadline = time.Time{}
}

func (l *NativeInventory) metadata() error {
	if err := l.allowed(l.ctx); err != nil {
		return err
	}
	// Existing directory cap bounds retained evidence; also bound per-pass checks.
	if l.counts.Stats+l.counts.Opens+32 > int64(maxDirectories*16) {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("native inventory metadata allowance spent"))
	}
	return nil
}

func generation(info os.FileInfo) (nativeGeneration, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Ino == 0 {
		return nativeGeneration{}, agentapi.ErrInvalidSourceBinding
	}
	sec, nano := nativeChangeTime(s)
	if sec == 0 {
		return nativeGeneration{}, agentapi.ErrInvalidSourceBinding
	}
	// #nosec G115 -- device numbers are equality-only identity bits; conversion is injective on each platform's dev_t domain.
	device := uint64(s.Dev)
	return nativeGeneration{Device: device, Inode: uint64(s.Ino), Mode: uint32(info.Mode()), Size: info.Size(), Mtime: info.ModTime().UnixNano(), CtimeSec: sec, CtimeNano: nano}, nil
}

// directory opens through a confined root and rejects every descendant symlink.
// Full named/descriptor generations must agree; ctime is no recursive counter.
func (l *NativeInventory) directory(path string) (f *os.File, g nativeGeneration, err error) {
	if err = l.metadata(); err != nil {
		return nil, g, err
	}
	l.counts.Opens++
	root, err := os.OpenRoot(l.root.Canonical)
	if err != nil {
		return nil, g, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	l.counts.Stats++
	// #nosec G703 -- validated committed root; independently compare named and rooted descriptor identity below.
	named, err := os.Lstat(l.root.Canonical)
	if err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
		return nil, g, agentapi.ErrInvalidSourceBinding
	}
	l.counts.Stats++
	rootInfo, rootErr := root.Lstat(".")
	if rootErr != nil || !os.SameFile(named, rootInfo) {
		return nil, g, transcriptio.ErrChanged
	}
	current := "."
	parts := []string{}
	if path != "." {
		parts = strings.Split(path, "/")
	}
	for _, part := range append([]string{"."}, parts...) {
		if err = l.metadata(); err != nil {
			return nil, g, err
		}
		if part != "." {
			current = filepath.Join(current, part)
		}
		l.counts.Stats++
		named, err = root.Lstat(current)
		if err != nil {
			return nil, g, err
		}
		if !named.IsDir() || named.Mode()&os.ModeSymlink != 0 {
			return nil, g, agentapi.ErrInvalidSourceBinding
		}
	}
	g, err = generation(named)
	if err != nil {
		return nil, g, err
	}
	l.counts.Opens++
	f, err = root.Open(path)
	if err != nil {
		return nil, g, err
	}
	l.counts.Stats++
	opened, e := f.Stat()
	if e == nil {
		var other nativeGeneration
		other, e = generation(opened)
		if other != g {
			e = transcriptio.ErrChanged
		}
	}
	l.counts.Stats++
	after, e2 := root.Lstat(path)
	if e2 == nil {
		var other nativeGeneration
		other, e2 = generation(after)
		if other != g {
			e2 = transcriptio.ErrChanged
		}
	}
	// Recheck the named root and every ancestor after the descriptor acquisition.
	l.counts.Stats++
	// #nosec G703 -- rechecking the same validated committed root detects pathname replacement.
	rootAfter, rootErr := os.Lstat(l.root.Canonical)
	if rootErr != nil || !os.SameFile(rootInfo, rootAfter) || rootAfter.Mode()&os.ModeSymlink != 0 {
		e = errors.Join(e, transcriptio.ErrChanged)
	}
	current = "."
	for _, part := range append([]string{"."}, parts...) {
		if part != "." {
			current = filepath.Join(current, part)
		}
		l.counts.Stats++
		ancestor, aerr := root.Lstat(current)
		if aerr != nil || !ancestor.IsDir() || ancestor.Mode()&os.ModeSymlink != 0 {
			e = errors.Join(e, transcriptio.ErrChanged)
		}
	}
	if e = errors.Join(e, e2); e != nil {
		return nil, g, errors.Join(e, f.Close())
	}
	return f, g, nil
}

func (l *NativeInventory) acquire(ctx context.Context) error {
	defer l.accountNativeOperations(&l.counts.AcquireStats, &l.counts.AcquireOpens, l.counts.Stats, l.counts.Opens)
	for index := 0; index < len(l.progress.Directories); index++ {
		if err := l.allowed(ctx); err != nil {
			return err
		}
		d := &l.progress.Directories[index]
		if completeNative(*d) {
			continue
		}
		if l.counts.Names+256 > 2048 {
			return nil
		}
		f, g, err := l.directory(d.Path)
		if err != nil {
			continue
		}
		if len(d.Pages) > 0 && g != d.Generation {
			_ = f.Close()
			l.resetCoverage()
			return nil
		}
		offset := int64(0)
		if len(d.Pages) > 0 {
			offset = d.Pages[len(d.Pages)-1].End
		}
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			l.resetCoverage()
			return nil
		}
		buffer := make([]byte, 4096)
		l.counts.PageReads++
		n, err := syscall.ReadDirent(int(f.Fd()), buffer)
		l.counts.DirentBytes += int64(n)
		_, _, names := syscall.ParseDirent(buffer[:max(n, 0)], -1, nil)
		l.counts.Names += int64(len(names))
		next, e := f.Seek(0, io.SeekCurrent)
		l.counts.Stats++
		after, e2 := f.Stat()
		var afterGeneration nativeGeneration
		if e2 == nil {
			afterGeneration, e2 = generation(after)
		}
		closeErr := f.Close()
		check, finalGeneration, checkErr := l.directory(d.Path)
		observedMismatch := nativeGenerationChanged(g, afterGeneration, e2) || nativeGenerationChanged(g, finalGeneration, checkErr) || n > 0 && next == offset
		if check != nil {
			checkErr = errors.Join(checkErr, check.Close())
		}
		if err = errors.Join(err, e, e2, closeErr, checkErr); err != nil || observedMismatch || len(names) > 256 {
			l.counts.UncommittedNames += int64(len(names))
			if observedMismatch {
				l.resetCoverage()
			}
			return nil
		}
		if err := l.recordablePage(len(names)); err != nil {
			return err
		}
		if len(d.Pages) >= maxCatalog {
			l.counts.UncommittedNames += int64(len(names))
			l.stopped = true
			return nil
		}
		prior := *d
		priorPages, priorChildren, priorMatches := len(d.Pages), len(d.Children), len(d.Matches)
		priorDirectories := len(l.progress.Directories)
		d.Generation = g
		kind := nativeCompletePage
		if n == 0 {
			kind = nativeEOF
		}
		d.Pages = append(d.Pages, nativePage{Kind: kind, Start: offset, End: next, Names: len(names), Bytes: n})
		// Work on acquired complete batches only. Do not discard a tail/cookie.
		children := []nativeDirectory{}
		for _, name := range names {
			if nativeChild(d.Path, name) {
				child := filepath.ToSlash(filepath.Join(d.Path, name))
				d.Children = append(d.Children, child)
				children = append(children, nativeDirectory{Path: child})
			} else if nativeDepth(d.Path) == 3 && slices.Contains(l.progress.Owners, name) {
				d.Matches = append(d.Matches, name)
			}
		}
		l.progress.Directories = append(l.progress.Directories, children...)
		if !l.withinRetainedLimits() {
			l.counts.UncommittedNames += int64(len(names))
			prior.Pages = prior.Pages[:priorPages]
			prior.Children = prior.Children[:priorChildren]
			prior.Matches = prior.Matches[:priorMatches]
			l.progress.Directories = l.progress.Directories[:priorDirectories]
			l.progress.Directories[index] = prior
			l.stopped = true
			return nil
		}
		if n > 0 {
			index--
		}
	}
	return nil
}

// A checked page may be recorded without further I/O after the work cutoff,
// while original cancellation and the total allowance continue to apply.
func (l *NativeInventory) recordablePage(names int) error {
	if err := l.ctx.Err(); err != nil {
		l.counts.UncommittedNames += int64(names)
		return err
	}
	if time.Now().After(l.deadline.Add(Budget / 4)) {
		l.counts.UncommittedNames += int64(names)
		return agentapi.Wrap(agentapi.Unavailable, errors.New("native inventory allowance spent"))
	}
	return nil
}

func (l *NativeInventory) accountNativeOperations(stats, opens *int64, beforeStats, beforeOpens int64) {
	*stats += l.counts.Stats - beforeStats
	*opens += l.counts.Opens - beforeOpens
}

func nativeGenerationChanged(before, after nativeGeneration, err error) bool {
	return err == nil && before != after
}

// At most one <=256-name complete page can be tentative. Refuse that whole
// page before another allocation batch, keeping a supported prior checkpoint.
func (l *NativeInventory) withinRetainedLimits() bool {
	pages, matches := 0, 0
	for _, d := range l.progress.Directories {
		pages += len(d.Pages)
		matches += len(d.Matches)
	}
	if pages > maxCatalog || matches > maxCatalog || len(l.progress.Directories) > maxDirectories {
		return false
	}
	raw, err := json.Marshal(l.progress)
	// Reserve every scheduling cursor digit before persisting the page.
	return err == nil && len(raw)+len(strconv.Itoa(len(l.progress.Owners))) <= maxCatalogBytes/32
}

func (l *NativeInventory) coverage(ctx context.Context) error {
	defer l.accountNativeOperations(&l.counts.SweepStats, &l.counts.SweepOpens, l.counts.Stats, l.counts.Opens)
	l.counts.Sweeps++
	for _, d := range l.progress.Directories {
		if !completeNative(d) {
			return agentapi.Wrap(agentapi.Unavailable, errors.New("native namespace coverage incomplete"))
		}
		if err := l.allowed(ctx); err != nil {
			return err
		}
		l.counts.GenerationChecks++
		f, g, err := l.directory(d.Path)
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			return err
		}
		if g != d.Generation {
			l.resetCoverage()
			return transcriptio.ErrChanged
		}
	}
	return nil
}

func (l *NativeInventory) candidate(ctx context.Context, id string) (agentapi.SourceRef, nativeGeneration, int, error) {
	defer l.accountNativeOperations(&l.counts.CandidateStats, &l.counts.CandidateOpens, l.counts.Stats, l.counts.Opens)
	var ref agentapi.SourceRef
	var stamp nativeGeneration
	found := 0
	for _, d := range l.progress.Directories {
		if !slices.Contains(d.Matches, id) {
			continue
		}
		parent := filepath.Join(d.Path, id)
		f, g, err := l.directory(parent)
		if err != nil {
			return ref, stamp, found, err
		}
		if err = f.Close(); err != nil {
			return ref, stamp, found, err
		}
		path := filepath.Join(l.root.Canonical, parent, "session.jsonl")
		info, err := (nativeCountedOpener{owner: l}).Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			check, g2, e := l.directory(parent)
			if check != nil {
				e = errors.Join(e, check.Close())
			}
			if e != nil || g2 != g {
				return ref, stamp, found, transcriptio.ErrChanged
			}
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return ref, stamp, found, agentapi.ErrInvalidSourceBinding
		}
		if l.counts.Probes >= HeaderProbes {
			return ref, stamp, found, agentapi.Wrap(agentapi.Unavailable, errors.New("native startup allowance spent"))
		}
		l.counts.Probes++
		selected := agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id}
		env := agentapi.SourceEnvironment{NativeRoot: &l.root, ReadBudget: l.budget, Files: nativeCountedOpener{owner: l}, Policy: transcriptio.OpenPolicy{Root: l.root.Canonical, RejectSymlinks: true}}
		if err = l.validate(ctx, env, selected, id); err != nil {
			return ref, stamp, found, err
		}
		after, err := (nativeCountedOpener{owner: l}).Lstat(path)
		if err != nil {
			return ref, stamp, found, err
		}
		stamp, err = generation(info)
		afterStamp, e := generation(after)
		if err != nil || e != nil || stamp != afterStamp {
			return ref, stamp, found, transcriptio.ErrChanged
		}
		check, g2, e := l.directory(parent)
		if check != nil {
			e = errors.Join(e, check.Close())
		}
		if e != nil || g2 != g {
			return ref, stamp, found, transcriptio.ErrChanged
		}
		ref = selected
		found++
	}
	return ref, stamp, found, nil
}

// Locate shares acquisition, but each newly selected owner pays its own fresh
// complete metadata sweep from the SAME remaining allowance. No TTL authorizes it.
func (l *NativeInventory) Locate(ctx context.Context, request agentapi.LocatorRequest) (agentapi.LocatorResult, error) {
	pending := agentapi.LocatorResult{Status: agentapi.LocatorPending}
	if err := l.allowed(ctx); err != nil {
		return pending, err
	}
	if request.Root != l.root || request.Parent != nil || !slices.Contains(l.eligible, request.NativeID) {
		return agentapi.LocatorResult{Status: agentapi.LocatorRefused}, nil
	}
	if !slices.Contains(l.progress.Owners, request.NativeID) || l.stopped || l.progress.Next == len(l.progress.Owners) || request.NativeID != l.progress.Owners[l.progress.Next] {
		return pending, nil
	}
	start := time.Now()
	l.deadline = start.Add(l.remaining - Budget/4)
	defer l.timed(start)
	workCtx, cancel := context.WithDeadline(ctx, l.deadline)
	defer cancel()
	ctx = workCtx
	l.counts.Selections++
	if err := l.acquire(ctx); err != nil {
		return pending, err
	}
	for _, d := range l.progress.Directories {
		if !completeNative(d) {
			return pending, nil
		}
	}
	// Keep the next real work item when the shared probe allowance is spent.
	// Never consume all later owners' fair slots with no-request Pending calls.
	if l.counts.Probes+2 > HeaderProbes {
		l.stopped = true
		return pending, nil
	}
	if err := l.coverage(ctx); err != nil {
		l.stopped = true
		return pending, err
	}
	l.progress.Next++
	l.skipInactive()
	ref, stamp, n, err := l.candidate(ctx, request.NativeID)
	if err != nil {
		l.stopped = true
		return pending, err
	}
	if n > 1 {
		return agentapi.LocatorResult{Status: agentapi.LocatorAmbiguous}, nil
	}
	if n == 0 {
		return pending, nil
	}
	// Recheck after candidate probing as well as when the lease later checks.
	if err := l.coverage(ctx); err != nil {
		l.stopped = true
		return pending, err
	}
	token := fmt.Sprintf("%s:%d", l.nonce, len(l.tokens)+1)
	relative, err := filepath.Rel(l.root.Canonical, ref.Path)
	if err != nil {
		return pending, err
	}
	l.tokens[request.NativeID] = nativeSelection{token: token, relative: strings.Clone(relative), stamp: stamp}
	return agentapi.LocatorResult{Status: agentapi.LocatorFound, Candidates: []agentapi.SourceRef{ref}, Revision: token}, nil
}

// Check accepts only an exact token minted by this still-live collector epoch.
func (l *NativeInventory) Check(ctx context.Context, id, token string) error {
	if err := l.allowed(ctx); err != nil {
		return err
	}
	l.counts.Checks++
	selected, ok := l.tokens[id]
	if !ok || token == "" || token != selected.token || !slices.Contains(l.eligible, id) || !slices.Contains(l.progress.Owners, id) {
		return agentapi.ErrInvalidSourceBinding
	}
	start := time.Now()
	l.deadline = start.Add(l.remaining - Budget/4)
	defer l.timed(start)
	workCtx, cancel := context.WithDeadline(ctx, l.deadline)
	defer cancel()
	ctx = workCtx
	if err := l.coverage(ctx); err != nil {
		return err
	}
	ref, stamp, n, err := l.candidate(ctx, id)
	if err != nil {
		return err
	}
	expected := agentapi.SourceRef{Kind: archive.SourceKindFile, Key: id, Path: filepath.Join(l.root.Canonical, selected.relative)}
	if n != 1 || ref != expected || stamp != selected.stamp {
		return transcriptio.ErrChanged
	}
	return l.coverage(ctx)
}

// Close checkpoints pages/progress atomically under expected-member ownership,
// using the original context. Cancellation releases memory without extending work.
func (l *NativeInventory) Close() error {
	if l.closed {
		return l.err
	}
	l.closed = true
	defer l.release()
	if len(l.progress.Owners) == 0 || l.ctx.Err() != nil {
		return nil
	}
	if l.remaining <= 0 {
		return nil
	}
	start := time.Now()
	defer l.timed(start)
	persistCtx, cancel := context.WithTimeout(l.ctx, l.remaining)
	defer cancel()
	raw, err := json.Marshal(l.progress)
	if err != nil {
		return err
	}
	if len(raw) > maxCatalogBytes/32 || !validNativeProgress(l.progress) {
		return errors.New("native inventory observations exceed checkpoint contract")
	}
	l.err = mergeNativeCatalog(persistCtx, l.path, l.expected, raw, true, l.budget)
	return l.err
}

// Failed construction releases only memory; it never rewrites historical state.
func (l *NativeInventory) failConstruction(err error) error {
	l.closed = true
	l.release()
	return err
}

func (l *NativeInventory) release() {
	l.tokens = nil
	l.progress = nativeProgress{}
	l.eligible = nil
	l.expected = nil
	l.validate = nil
	l.nonce = ""
	l.path = ""
	l.root = archive.NativeRoot{}
	l.budget.ReleaseUnscoped(l.charge)
	l.charge = 0
}

type nativeCountedOpener struct{ owner *NativeInventory }

func (o nativeCountedOpener) Lstat(path string) (os.FileInfo, error) {
	if err := o.owner.metadata(); err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(o.owner.root.Canonical, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	o.owner.counts.Opens++
	root, err := os.OpenRoot(o.owner.root.Canonical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	o.owner.counts.Stats++
	return root.Lstat(relative)
}

func (o nativeCountedOpener) EvalSymlinks(path string) (string, error) {
	relative, err := filepath.Rel(o.owner.root.Canonical, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", agentapi.ErrInvalidSourceBinding
	}
	current := o.owner.root.Canonical
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	for _, part := range append([]string{"."}, parts...) {
		if part != "." {
			current = filepath.Join(current, part)
		}
		info, err := o.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", agentapi.ErrInvalidSourceBinding
		}
	}
	return path, nil
}

func (o nativeCountedOpener) OpenRegular(path string) (transcriptio.File, error) {
	before, err := o.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, transcriptio.ErrNotRegularFile
	}
	if _, err = o.EvalSymlinks(path); err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(o.owner.root.Canonical, path)
	if err != nil {
		return nil, err
	}
	o.owner.counts.Opens++
	root, err := os.OpenRoot(o.owner.root.Canonical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	o.owner.counts.Opens++
	f, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	o.owner.counts.Stats++
	after, err := f.Stat()
	if err != nil || !transcriptio.SameObservation(before, after) {
		return nil, errors.Join(transcriptio.ErrChanged, err, f.Close())
	}
	return &nativeCountedFile{File: f, owner: o.owner}, nil
}

type nativeCountedFile struct {
	transcriptio.File
	owner *NativeInventory
	read  int64
}

func (f *nativeCountedFile) Stat() (os.FileInfo, error) {
	if err := f.owner.metadata(); err != nil {
		return nil, err
	}
	f.owner.counts.Stats++
	return f.File.Stat()
}

func (f *nativeCountedFile) ReadAt(b []byte, offset int64) (int, error) {
	if err := f.owner.metadata(); err != nil {
		return 0, err
	}
	if f.read+int64(len(b)) > sourcefacts.HeaderBytes {
		return 0, agentapi.Wrap(agentapi.Unavailable, errors.New("native startup byte allowance spent"))
	}
	n, err := f.File.ReadAt(b, offset)
	f.owner.counts.HeaderBytes += int64(n)
	f.owner.counts.HeaderReads++
	f.read += int64(n)
	return n, err
}

// PendingOrder returns disposable scheduling hints, rotated to the next frozen
// native owner. It grants no source/currentness authority. The caller may only
// reorder matching pending slots within this immutable root/query domain.
func (l *NativeInventory) PendingOrder() []string {
	if l.closed || len(l.progress.Owners) == 0 {
		return nil
	}
	order := make([]string, 0, len(l.progress.Owners))
	for i := range l.progress.Owners {
		id := l.progress.Owners[(l.progress.Next+i)%len(l.progress.Owners)]
		if slices.Contains(l.eligible, id) {
			order = append(order, id)
		}
	}
	return order
}

// Inactive historical slots are scheduling holes, never source actions. New
// current owners wait until the exact historical cohort has finished its cycle.
func (l *NativeInventory) skipInactive() {
	for l.progress.Next < len(l.progress.Owners) && !slices.Contains(l.eligible, l.progress.Owners[l.progress.Next]) {
		l.progress.Next++
	}
}
