package muse

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const (
	boundaryChunk              = 64 << 10
	boundaryCaseBytes    int64 = 640 << 20
	boundarySessionBytes       = agentapi.PassReadBudget - agentapi.PassCacheReserve
	boundaryMarker             = "MUSE_BOUNDARY_PAYLOAD"
)

type boundaryCase struct {
	name   string
	family string
	target int64
}

func boundaryCases() []boundaryCase {
	var cases []boundaryCase
	for _, family := range []struct {
		name  string
		limit int64
	}{
		{"R", archive.MaxRawTranscriptBytes},
		{"P", archive.MaxRecordBytes},
		{"S", archive.MaxSourceLineBytes},
	} {
		for i, suffix := range []string{"below", "at", "above"} {
			cases = append(cases, boundaryCase{family.name + "_" + suffix, family.name, family.limit + int64(i) - 1})
		}
	}
	return cases
}

// Full controls are deliberately absent from ordinary CI. A separately reviewed
// process selects ONE case and ONE phase; absence is not full-boundary coverage.
// The owning supervisor checks the finite corpus (including failures) against
// 2 GiB before admission, pins this source and inputs, and preserves failed roots.
// This is a controlled-emitter bound, not an OS quota or publication campaign.
func TestMuseFullByteBoundary(t *testing.T) {
	phase := os.Getenv("AGENT_ARCHIVE_MUSE_BOUNDARY_PHASE")
	if phase == "" {
		return
	}
	if os.Getenv("AGENT_ARCHIVE_PERF") != "1" {
		t.Fatal("full boundaries require explicit PERF=1")
	}
	if phase != "generate" && phase != "verify" {
		t.Fatal("unknown boundary phase")
	}
	root := os.Getenv("AGENT_ARCHIVE_MUSE_BOUNDARY_ROOT")
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		t.Fatal("absolute clean owned case root required")
	}
	selected := os.Getenv("AGENT_ARCHIVE_MUSE_BOUNDARY_CASE")
	for _, c := range boundaryCases() {
		if c.name != selected {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Run(phase, func(t *testing.T) {
				if phase == "generate" {
					boundaryGenerate(t, root, c)
				} else {
					boundaryVerify(t, root, c)
				}
			})
		})
		return
	}
	t.Fatal("unknown boundary case")
}

// This small real snapshot/Filter/codec control runs in ordinary CI. It does not
// assert public root admission, collector publication or full-size coverage.
func TestMuseBoundarySmallPositive(t *testing.T) {
	bundle := boundarySmallBundle(t)
	var plain bytes.Buffer
	if err := archive.EncodeSource(&plain, bundle); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(plain.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	boundaryDecode(t, &compressed, bundle, bytes.SplitN(plain.Bytes(), []byte("\n"), 2)[0])
}

type boundaryManifest struct {
	Case           string `json:"case"`
	Family         string `json:"family"`
	Target         int64  `json:"target"`
	InputBytes     int64  `json:"input_bytes"`
	SHA256         string `json:"sha256"`
	Rows           int    `json:"rows"`
	MaxPhysicalRow int64  `json:"max_physical_row"`
	// S input is the scalar, not a fabricated source record or capture. The
	// verification phase uses the real encoder to create the complete source.
	ScalarOnly bool `json:"scalar_only"`
}

// Marshal only a small existing ordinary name-change shape. Padding is safe
// ASCII inside its serialized scalar, never blank lines or out-of-JSON bytes.
func boundaryRow(t *testing.T, sequence int) (prefix, suffix []byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema_version": 1, "id": fmt.Sprintf("00000000-0000-4000-8000-%012x", sequence+0x100000),
		"stream":   map[string]any{"kind": "session", "id": "00000000-0000-7000-8000-000000000001"},
		"sequence": sequence, "recorded_at": 1792083859982704, "record_type": "event", "durability": "durable", "causation_id": nil,
		"payload_type": "session.name.changed", "payload_schema_version": 1,
		"payload": map[string]any{"session_id": "00000000-0000-7000-8000-000000000001", "new_name": boundaryMarker, "previous_name": nil, "source": "user", "operation_id": "synthetic"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(raw, []byte(boundaryMarker)) != 1 {
		t.Fatal("ambiguous serialized scalar")
	}
	i := bytes.Index(raw, []byte(boundaryMarker))
	return raw[:i], append(raw[i+len(boundaryMarker):], '\n')
}

// boundaryWriter rejects before writing outside its controlled allowance. Its
// only padding allocation is 64 KiB; codec internal allocations are separate.
type boundaryWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (w *boundaryWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit-w.n {
		return 0, errors.New("controlled case byte allowance exhausted")
	}
	total := 0
	for len(p) > 0 {
		n, err := w.w.Write(p[:min(len(p), boundaryChunk)])
		w.n += int64(n)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
func boundaryPadding(t *testing.T, w io.Writer, n int64) {
	t.Helper()
	if n < 0 {
		t.Fatal("negative scalar size")
	}
	block := bytes.Repeat([]byte{'x'}, boundaryChunk)
	for n > 0 {
		part := block[:min(int64(len(block)), n)]
		boundaryWrite(t, w, part)
		n -= int64(len(part))
	}
}
func boundaryWrite(t *testing.T, w io.Writer, p []byte) {
	t.Helper()
	if n, err := w.Write(p); err != nil || n != len(p) {
		t.Fatalf("write %d/%d: %v", n, len(p), err)
	}
}
func boundaryNewFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	return f
}

func boundaryGenerate(t *testing.T, root string, c boundaryCase) {
	t.Helper()
	bundle := boundarySmallBundle(t)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Never remove this root in a test cleanup: failures and their real bytes
	// are retained until the owned supervisor has joined every process.
	boundaryWrite(t, boundaryNewFile(t, filepath.Join(root, "owner.json")), []byte("{\"purpose\":\"muse-full-boundary-v1\",\"case\":\""+c.name+"\"}\n"))
	f := boundaryNewFile(t, filepath.Join(root, "input"))
	h := sha256.New()
	w := &boundaryWriter{w: io.MultiWriter(f, h), limit: boundaryCaseBytes - (1 << 20)}
	m := boundaryManifest{Case: c.name, Family: c.family, Target: c.target, ScalarOnly: c.family == "S"}
	if c.family == "S" {
		index := boundaryNameIndex(t, bundle)
		var calibration bytes.Buffer
		if err := archive.EncodeSource(&calibration, bundle); err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSuffix(calibration.Bytes(), []byte("\n")), []byte("\n"))
		fields := bundle.NativeRecords[index]["fields"].(map[string]any)
		old := fields["new_name"].(string)
		boundaryPadding(t, w, int64(len(old))+c.target-int64(len(lines[index+1])))
	} else {
		prefix := readSynthetic(t, "synthetic-exec-empty.jsonl")
		boundaryWrite(t, w, prefix)
		for _, line := range bytes.Split(bytes.TrimSuffix(prefix, []byte("\n")), []byte("\n")) {
			m.Rows++
			m.MaxPhysicalRow = max(m.MaxPhysicalRow, int64(len(line)+1))
		}
		remaining := c.target - int64(len(prefix))
		if c.family == "P" {
			remaining = c.target
		}
		for seq := 5; remaining > 0; seq++ {
			pre, post := boundaryRow(t, seq)
			minimum := int64(len(pre) + len(post))
			size := remaining
			if c.family == "R" {
				size = min(size, 128<<10)
				// Leave room for the next serialized row rather than padding
				// outside JSON when the exact file remainder is small.
				if rest := remaining - size; rest > 0 && rest < minimum+32 {
					size -= minimum + 32 - rest
				}
			}
			if size < minimum {
				t.Fatal("row cannot fit exact remainder")
			}
			boundaryWrite(t, w, pre)
			boundaryPadding(t, w, size-minimum)
			boundaryWrite(t, w, post)
			remaining -= size
			m.Rows++
			m.MaxPhysicalRow = max(m.MaxPhysicalRow, size)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	m.InputBytes = w.n
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	boundaryWrite(t, boundaryNewFile(t, filepath.Join(root, "manifest.json")), append(raw, '\n'))
	t.Logf("generated case=%s actual_bytes=%d sha256=%s rows=%d maximum_physical_row=%d scalar_only=%t", c.name, m.InputBytes, m.SHA256, m.Rows, m.MaxPhysicalRow, m.ScalarOnly)
}

func boundaryInput(t *testing.T, root string, c boundaryCase) boundaryManifest {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("owned root: %v", err)
	}
	marker := filepath.Join(root, "owner.json")
	info, err = os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256 || info.Mode().Perm() != 0600 {
		t.Fatal("invalid owner marker file")
	}
	owner, err := os.ReadFile(marker)
	if err != nil || string(owner) != "{\"purpose\":\"muse-full-boundary-v1\",\"case\":\""+c.name+"\"}\n" {
		t.Fatal("owner marker mismatch")
	}
	manifest := filepath.Join(root, "manifest.json")
	info, err = os.Lstat(manifest)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || info.Mode().Perm() != 0600 {
		t.Fatal("invalid manifest file")
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m boundaryManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatal("manifest trailing data")
	}
	if m.Case != c.name || m.Family != c.family || m.Target != c.target || m.ScalarOnly != (c.family == "S") {
		t.Fatal("case binding mismatch")
	}
	input := filepath.Join(root, "input")
	info, err = os.Lstat(input)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != m.InputBytes || m.InputBytes <= 0 || m.InputBytes > archive.MaxRawTranscriptBytes+1 {
		t.Fatal("invalid real input boundary")
	}
	if c.family == "R" && m.InputBytes != c.target {
		t.Fatal("raw file target mismatch")
	}
	if c.family == "P" && m.InputBytes != int64(len(readSynthetic(t, "synthetic-exec-empty.jsonl")))+c.target {
		t.Fatal("physical record target mismatch")
	}
	if c.family == "R" && (m.Rows < 4 || m.MaxPhysicalRow > 128<<10) {
		t.Fatal("raw fixture row bound mismatch")
	}
	if c.family == "P" && (m.Rows != 4 || m.MaxPhysicalRow != c.target) {
		t.Fatal("physical fixture row bound mismatch")
	}
	if c.family == "S" && (m.Rows != 0 || m.MaxPhysicalRow != 0) {
		t.Fatal("scalar fixture claimed native rows")
	}
	f, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.LimitReader(f, m.InputBytes+1), make([]byte, boundaryChunk))
	if err != nil || n != m.InputBytes || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		t.Fatal("actual input digest mismatch")
	}
	return m
}

type boundaryReads struct{ calls, bytes int64 }
type boundaryOpener struct {
	transcriptio.OS
	reads *boundaryReads
}

func (o boundaryOpener) OpenRegular(path string) (transcriptio.File, error) {
	f, err := o.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return boundaryReadFile{File: f, reads: o.reads}, nil
}

type boundaryReadFile struct {
	transcriptio.File
	reads *boundaryReads
}

func (f boundaryReadFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.reads.calls++
	f.reads.bytes += int64(n)
	return n, err
}

func boundaryNative(t *testing.T, path string, family string, target int64, digest string) archive.SourceBundle {
	t.Helper()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	end := budget.BeginScope(boundarySessionBytes)
	reads := &boundaryReads{}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: boundaryOpener{reads: reads}, ReadBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
		used, peak := budget.Charged()
		t.Logf("family=%s actual_ReadAt_calls=%d actual_ReadAt_bytes=%d ledger_live=%d ledger_peak=%d", family, reads.calls, reads.bytes, used, peak)
		if used != 0 || peak > boundarySessionBytes {
			t.Error("native ledger not released or scope exceeded")
		}
		end()
	}()
	snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path}, agentapi.ReadLimits{ReadBudget: budget})
	if family == "R" && target > archive.MaxRawTranscriptBytes {
		var raw *agentapi.RawLimitError
		if !errors.As(err, &raw) || raw.Size != target || raw.Limit != archive.MaxRawTranscriptBytes || snapshot != nil {
			t.Fatalf("real raw gate: snapshot=%v error=%v", snapshot, err)
		}
		return archive.SourceBundle{}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	if family == "R" {
		actual, ok := snapshot.(*sourceSnapshot)
		if !ok || snapshot.Input().File.Length() != target || actual.proof.Boundary != target || hex.EncodeToString(actual.proof.SHA256[:]) != digest || reads.bytes < target {
			t.Fatal("real snapshot hash/boundary mismatch")
		}
		return archive.SourceBundle{}
	}
	beforeFilter := *reads
	out, release, err := (Filter{}).FilterLeased(t.Context(), snapshot.Input(), agentapi.FilterContext{Limits: agentapi.ReadLimits{ReadBudget: budget}, StartedAt: time.Unix(2000000100, 0)}, budget)
	defer release()
	filterBytes := reads.bytes - beforeFilter.bytes
	t.Logf("snapshot_ReadAt_bytes=%d Filter_ReadAt_calls=%d Filter_ReadAt_bytes=%d", beforeFilter.bytes, reads.calls-beforeFilter.calls, filterBytes)
	if family == "P" {
		if !errors.Is(err, agentapi.ErrReadBudget) || errors.Is(err, archive.ErrRecordTooLarge) || len(out.Records) != 0 || len(out.Text) != 0 {
			t.Fatalf("expected early budget refusal and no retained output: records=%d error=%v", len(out.Records), err)
		}
		if !budget.ScopeExceeded() || filterBytes <= 0 || filterBytes >= target {
			t.Fatal("record did not refuse at session scope before reading the large record")
		}
		return archive.SourceBundle{}
	}
	if err != nil || len(out.Records) == 0 || len(out.SessionIDs) != 1 {
		t.Fatalf("small real Filter positive: %v", err)
	}
	// bundleFor JSON-roundtrips into detached records while the Filter loan is
	// still live. Only then may the deferred release and ledger check run.
	return bundleFor(t, out)
}

func boundarySmallBundle(t *testing.T) archive.SourceBundle {
	t.Helper()
	var raw bytes.Buffer
	raw.Write(readSynthetic(t, "synthetic-exec-empty.jsonl"))
	pre, post := boundaryRow(t, 5)
	raw.Write(pre)
	raw.WriteString(boundaryMarker)
	raw.Write(post)
	path := filepath.Join(t.TempDir(), "small.jsonl")
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	bundle := boundaryNative(t, path, "small", 0, "")
	if bundle.History != nil {
		t.Fatal("unexpected history; do not erase codec provenance")
	}
	boundaryNameIndex(t, bundle)
	return bundle
}
func boundaryNameIndex(t *testing.T, bundle archive.SourceBundle) int {
	t.Helper()
	for i, row := range bundle.NativeRecords {
		if row["type"] == "session.name.changed" {
			fields, ok := row["fields"].(map[string]any)
			if !ok || fields["new_name"] != boundaryMarker {
				t.Fatal("unexpected retained name shape")
			}
			return i
		}
	}
	t.Fatal("real Filter did not retain name scalar")
	return 0
}
func boundaryDecode(t *testing.T, r io.Reader, bundle archive.SourceBundle, header []byte) {
	t.Helper()
	records, headers := 0, 0
	err := archive.DecodeSource(r, archive.DecodeOptions{MaxUncompressedBytes: boundaryCaseBytes}, func(line archive.SourceLine) error {
		switch line.Kind {
		case archive.SourceLineHeader:
			actual, err := json.Marshal(line.Header)
			if err != nil || !bytes.Equal(actual, header) {
				return errors.New("decoded complete source header changed")
			}
			headers++
		case archive.SourceLineNativeRecord:
			if records >= len(bundle.NativeRecords) || !reflect.DeepEqual(line.NativeRecord, bundle.NativeRecords[records]) {
				return errors.New("decoded retained record changed")
			}
			records++
		default:
			return errors.New("unexpected codec line kind")
		}
		return nil
	})
	if err != nil || records != len(bundle.NativeRecords) || headers != 1 {
		t.Fatalf("whole source round trip: headers=%d records=%d: %v", headers, records, err)
	}
}

func boundaryVerify(t *testing.T, root string, c boundaryCase) {
	t.Helper()
	m := boundaryInput(t, root, c)
	if c.family != "S" {
		boundaryNative(t, filepath.Join(root, "input"), c.family, c.target, m.SHA256)
		return
	}
	bundle := boundarySmallBundle(t)
	index := boundaryNameIndex(t, bundle)
	var calibration bytes.Buffer
	if err := archive.EncodeSource(&calibration, bundle); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(calibration.Bytes(), []byte("\n")), []byte("\n"))
	before := int64(0)
	for _, line := range lines[:index+1] {
		before += int64(len(line) + 1)
	}
	fields := bundle.NativeRecords[index]["fields"].(map[string]any)
	old := fields["new_name"].(string)
	if m.InputBytes != int64(len(old))+c.target-int64(len(lines[index+1])) {
		t.Fatal("scalar calibration mismatch")
	}
	scalar, err := os.ReadFile(filepath.Join(root, "input"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range scalar {
		if b != 'x' {
			t.Fatal("noncanonical scalar")
		}
	}
	fields["new_name"] = string(scalar)
	f := boundaryNewFile(t, filepath.Join(root, "private-source.jsonl.gz"))
	compressed := &boundaryWriter{w: f, limit: boundaryCaseBytes - m.InputBytes - (1 << 20)}
	gz := gzip.NewWriter(compressed)
	plain := &boundaryWriter{w: gz, limit: boundaryCaseBytes - m.InputBytes - (1 << 20)}
	err = archive.EncodeSource(plain, bundle)
	if c.target > archive.MaxSourceLineBytes {
		want := fmt.Sprintf("source line of %d bytes exceeds the %d byte line limit", c.target, archive.MaxSourceLineBytes)
		if err == nil || err.Error() != want || plain.n != before {
			t.Fatalf("oversized codec line leaked or wrong refusal: bytes=%d want_prefix=%d error=%v", plain.n, before, err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		want := int64(calibration.Len()) + c.target - int64(len(lines[index+1]))
		if plain.n != want {
			t.Fatalf("encoded source byte boundary=%d want=%d", plain.n, want)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if c.target <= archive.MaxSourceLineBytes {
		r, err := os.Open(filepath.Join(root, "private-source.jsonl.gz"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := r.Close(); err != nil {
				t.Error(err)
			}
		}()
		boundaryDecode(t, r, bundle, lines[0])
	}
	t.Logf("codec_only=true JSON_line_excluding_LF=%d private_plain_bytes=%d gzip_bytes=%d full_Muse_capture=false", c.target, plain.n, compressed.n)
}
