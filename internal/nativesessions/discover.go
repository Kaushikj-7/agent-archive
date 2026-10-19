package nativesessions

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// FileSystem is the small read-only native filesystem port.
type FileSystem interface {
	DirectoryReader
	transcriptio.Opener
}

// OS implements native reads on the host filesystem.
type OS struct{ transcriptio.OS }

// ReadDir enumerates one directory.
func (OS) ReadDir(path string) ([]fs.DirEntry, error) { return readDirectory(path) }

// Candidate is a compact verified identity, not an archive registration.
type Candidate struct {
	Ref        Ref
	NativeID   string
	Directory  string
	StartedAt  time.Time
	ModifiedAt time.Time
	Stamp      transcriptio.Stamp
}

// Scope limits candidates to a checkout and its descendants.
type Scope struct {
	Directories []string
	All         bool
}

// Limits bounds traversal, header records and cumulative reads.
type Limits struct {
	Files       int
	HeaderBytes int64
	RecordBytes int64
	TotalBytes  int64
	Workers     int
}

// Coverage records identity discovery independently of later label inspection.
type Coverage struct {
	Enumerated       int
	Inspected        int
	InScope          int
	Skipped          int
	IdentityComplete bool
	Reason           string
	ReservedBytes    int64
	ReadBytes        int64
	timeBounded      bool
	unverifiedNewest time.Time
}

// LatestComplete reports whether no unresolved history can be as recent as at.
// Missing enumeration or an unverified file timestamp always prevents this proof.
func (c *Coverage) LatestComplete(at time.Time) bool {
	return c.IdentityComplete || c.timeBounded && at.After(c.unverifiedNewest)
}

// Result retains the bounded candidate catalog for explicit older batches.
type Result struct {
	Candidates []Candidate
	Coverage   Coverage
}

var errNativeHistory = errors.New("native history is not selectable")

// InspectNative requires transcript content to establish the selected identity.
// Import's compatibility parser deliberately retains its separate semantics.
func InspectNative(ctx context.Context, ports agentapi.NativeHeadersLookup, s transcriptio.Input, ref Ref, window, record int64) (Header, transcriptio.RecordWindow, error) {
	var w transcriptio.RecordWindow
	if ports == nil {
		return Header{}, w, errors.New("native header lookup required")
	}
	inspector, ok := ports.LookupNativeHeaders(ref.Harness)
	if !ok {
		return Header{}, w, errors.New("native header inspection unavailable")
	}
	h, err := inspector.InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: ref.Path, Scan: func(visit func([]byte) bool) error {
		var err error
		w, err = s.Records(ctx, false, window, record, visit)
		return err
	}})
	if (h.SubagentOnly || h.CapturePending != "") && err == nil {
		err = errNativeHistory
	}
	return h, w, err
}

// Discover enumerates bounded references and inspects them with a fixed pool.
// Only the coordinator canonicalizes checkout paths or mutates the catalog.
func Discover(ctx context.Context, ports agentapi.NativeDiscoveryLookup, files FileSystem, roots []StoreRoot, scope Scope, limits Limits) (Result, error) {
	var out Result
	if ports == nil || files == nil || limits.Files <= 0 || limits.HeaderBytes <= 0 || limits.RecordBytes <= 0 || limits.TotalBytes <= 0 || limits.Workers <= 0 {
		return out, errors.New("native discovery requires filesystem and positive limits")
	}
	refs, coverage, err := enumerateNativeRefs(ctx, ports, files, roots, limits.Files)
	if err != nil {
		return out, err
	}
	out.Coverage = coverage
	out.Coverage.timeBounded = coverage.IdentityComplete
	var answers []headerAnswer
	for len(refs) > 0 {
		jobs, consumed, err := reserveNativeHeaders(ctx, files, refs, limits, &out.Coverage)
		if err != nil {
			return out, err
		}
		if len(jobs) == 0 {
			break
		}
		batch, err := inspectNativeHeaders(ctx, ports, files, jobs, limits)
		if err != nil {
			return out, err
		}
		// Release reservations only after the entire deterministic batch finishes.
		// Later headers can use the unused bytes without exceeding the shared cap.
		for _, a := range batch {
			out.Coverage.ReadBytes += a.bytes
		}
		out.Coverage.ReservedBytes = out.Coverage.ReadBytes
		answers = append(answers, batch...)
		refs = refs[consumed:]
	}
	out.Candidates, err = scopeNativeCandidates(ctx, files, scope, answers, &out.Coverage)
	out.Coverage.InScope = len(out.Candidates)
	if !out.Coverage.IdentityComplete && out.Coverage.Reason == "" {
		out.Coverage.Reason = "unverified identities or checkouts"
	}
	sort.Slice(out.Candidates, func(i, j int) bool { return candidateBefore(out.Candidates[i], out.Candidates[j]) })
	return out, err
}

func candidateBefore(a, b Candidate) bool {
	if !a.ModifiedAt.Equal(b.ModifiedAt) {
		return a.ModifiedAt.After(b.ModifiedAt)
	}
	if a.Ref.Harness != b.Ref.Harness {
		return a.Ref.Harness < b.Ref.Harness
	}
	if a.NativeID != b.NativeID {
		return a.NativeID < b.NativeID
	}
	return a.Ref.Path < b.Ref.Path
}

func enumerateNativeRefs(ctx context.Context, ports agentapi.DiscoveryLookup, files FileSystem, roots []StoreRoot, capFiles int) ([]Ref, Coverage, error) {
	c := Coverage{IdentityComplete: true}
	roots = append([]StoreRoot(nil), roots...)
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].Harness != roots[j].Harness {
			return roots[i].Harness < roots[j].Harness
		}
		return roots[i].Path < roots[j].Path
	})
	var refs []Ref
	stores := map[string]bool{}
	seen := map[string]bool{}
	for _, root := range roots {
		canonical, err := files.EvalSymlinks(root.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.IdentityComplete = false
				c.Reason = "unreadable store"
			}
			continue
		}
		root.Path = canonical
		key := root.Harness + "\x00" + canonical
		if stores[key] {
			continue
		}
		stores[key] = true
		remaining := capFiles - len(refs)
		if remaining <= 0 {
			c.IdentityComplete = false
			c.Reason = "file limit"
			break
		}
		provider, ok := ports.LookupDiscovery(root.Harness)
		if !ok {
			c.IdentityComplete = false
			c.Reason = "native discovery unavailable"
			continue
		}
		report, err := provider.Discover(ctx, agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryHandoff, Stage: agentapi.DiscoveryReferences, Roots: []agentapi.NativeStoreRoot{root}, Files: nativeDiscoveryFiles{files}, MaxFiles: remaining}, func(candidate agentapi.DiscoveryCandidate) error {
			ref := Ref{Harness: string(candidate.Session.Agent), Path: candidate.Source.Path, Store: candidate.Root}
			key := ref.Harness + "\x00" + ref.Path
			if !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
			if len(refs) >= capFiles {
				return errEnumerationLimit
			}
			return nil
		})
		if err != nil && !errors.Is(err, errEnumerationLimit) {
			return refs, c, err
		}
		if report.Incomplete || errors.Is(err, errEnumerationLimit) {
			c.IdentityComplete = false
			c.Reason = "incomplete enumeration"
		}

	}
	c.Enumerated = len(refs)
	return refs, c, nil
}

type headerJob struct {
	index  int
	ref    Ref
	window int64
}

type headerAnswer struct {
	c           Candidate
	bytes       int64
	timeBounded bool
	err         error
}

func reserveNativeHeaders(ctx context.Context, files FileSystem, refs []Ref, limits Limits, c *Coverage) ([]headerJob, int, error) {
	var jobs []headerJob
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		info, err := files.Lstat(ref.Path)
		if err != nil {
			c.Skipped++
			c.IdentityComplete = false
			c.timeBounded = false
			continue
		}
		cost := min(max(0, info.Size()), limits.HeaderBytes)
		if cost > limits.TotalBytes-c.ReservedBytes {
			if len(jobs) == 0 {
				c.IdentityComplete = false
				c.timeBounded = false
				c.Reason = "read budget"
			}
			return jobs, i, nil
		}
		c.ReservedBytes += cost
		jobs = append(jobs, headerJob{index: i, ref: ref, window: cost})
	}
	return jobs, len(refs), nil
}

func inspectNativeHeader(ctx context.Context, ports agentapi.NativeHeadersLookup, files FileSystem, j headerJob, limits Limits) headerAnswer {
	s, err := transcriptio.Open(files, j.ref.Path, transcriptio.OpenPolicy{RejectSymlinks: true, Root: j.ref.Store})
	if err != nil {
		return headerAnswer{err: err}
	}
	defer func() { _ = s.Close() }()
	h, w, err := InspectNative(ctx, ports, s, j.ref, j.window, limits.RecordBytes)
	timeBounded := err == nil
	if err == nil && (h.IdentityMismatch || !validNativeID(h.NativeID) || !filepath.IsAbs(h.Directory)) {
		err = errors.New("native identity or checkout unavailable")
	}
	stamp := s.Stamp()
	c := Candidate{Ref: j.ref, NativeID: h.NativeID, Directory: h.Directory, StartedAt: h.StartedAt, ModifiedAt: stamp.ModifiedAt, Stamp: stamp}
	return headerAnswer{c: c, bytes: w.Bytes, timeBounded: timeBounded, err: err}

}

func validNativeID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func inspectNativeHeaders(ctx context.Context, ports agentapi.NativeHeadersLookup, files FileSystem, scheduled []headerJob, limits Limits) ([]headerAnswer, error) {
	type indexedAnswer struct {
		index  int
		answer headerAnswer
	}
	jobs := make(chan headerJob, limits.Workers)
	results := make(chan indexedAnswer, limits.Workers)
	var workers sync.WaitGroup
	for range limits.Workers {
		workers.Go(func() {
			for j := range jobs {
				a := inspectNativeHeader(ctx, ports, files, j, limits)
				select {
				case results <- indexedAnswer{index: j.index, answer: a}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, j := range scheduled {
			select {
			case jobs <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	got := map[int]headerAnswer{}
	for a := range results {
		got[a.index] = a.answer
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	answers := make([]headerAnswer, 0, len(scheduled))
	for _, j := range scheduled {
		answers = append(answers, got[j.index])
	}
	return answers, nil
}

func scopeNativeCandidates(ctx context.Context, files FileSystem, scope Scope, answers []headerAnswer, c *Coverage) ([]Candidate, error) {
	cache := map[string]string{}
	missing := map[string]bool{}
	canonical := func(dir string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if d, ok := cache[dir]; ok {
			return d, nil
		}
		d, err := files.EvalSymlinks(dir)
		if canceled := ctx.Err(); canceled != nil {
			return "", canceled
		}
		if err != nil {
			missing[dir] = errors.Is(err, fs.ErrNotExist)
			d = ""
		}
		cache[dir] = d
		return d, nil
	}
	var dirs []string
	for _, dir := range scope.Directories {
		d, err := canonical(dir)
		if err != nil {
			return nil, err
		}
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if !scope.All && len(dirs) == 0 {
		return nil, errors.New("checkout directory cannot be resolved; use --project DIR")
	}
	var candidates []Candidate
	for _, a := range answers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.Inspected++
		if a.err != nil {
			c.Skipped++
			if !errors.Is(a.err, errNativeHistory) {
				c.recordUnverified(a)
			}
			continue
		}
		var err error
		directory := a.c.Directory
		a.c.Directory, err = canonical(directory)
		if err != nil {
			return nil, err
		}
		if a.c.Directory == "" {
			c.Skipped++
			if !missing[directory] {
				c.recordUnverified(a)
			}
			continue
		}
		if directoryInScope(a.c.Directory, dirs, scope.All) {
			candidates = append(candidates, a.c)
		}
	}
	return candidates, nil
}

func (c *Coverage) recordUnverified(a headerAnswer) {
	c.IdentityComplete = false
	if !a.timeBounded {
		c.timeBounded = false
	}
	if a.c.ModifiedAt.After(c.unverifiedNewest) {
		c.unverifiedNewest = a.c.ModifiedAt
	}
}

func directoryInScope(candidate string, dirs []string, all bool) bool {
	if all {
		return true
	}
	for _, dir := range dirs {
		if local.PathWithin(candidate, dir) {
			return true
		}
	}

	return false
}

var errEnumerationLimit = errors.New("bounded native enumeration stopped")

// Reference enumeration cannot open transcripts before cumulative reservations.
type nativeDiscoveryFiles struct{ files FileSystem }

func (n nativeDiscoveryFiles) ReadDir(path string) ([]fs.DirEntry, error) {
	return n.files.ReadDir(path)
}

func (n nativeDiscoveryFiles) Lstat(path string) (fs.FileInfo, error) { return n.files.Lstat(path) }

func (nativeDiscoveryFiles) Open(string) (io.ReadCloser, error) {
	return nil, errors.New("reference enumeration cannot open transcript")
}
