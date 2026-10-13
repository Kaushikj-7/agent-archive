package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type framingFiles struct {
	transcriptio.OS
	bytes *int64
}
type framingFile struct {
	transcriptio.File
	bytes *int64
}

func (f framingFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return framingFile{file, f.bytes}, nil
}
func (f framingFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	*f.bytes += int64(n)
	return n, err
}

type unframedFile struct{ agentapi.FileInput }

func TestOrdinaryHashMeasuresCompleteRecordFraming(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "abc", "abc\n", "abc\n12345\nx", "\n\n", strings.Repeat("x", 65537) + "\n"} {
		h := &ordinaryDigest{Hash: sha256.New()}
		for start := 0; start < len(raw); start += 7 {
			if _, err := h.Write([]byte(raw[start:min(start+7, len(raw))])); err != nil {
				t.Fatal(err)
			}
		}
		want := sha256.Sum256([]byte(raw))
		if !bytes.Equal(h.Sum(nil), want[:]) {
			t.Fatal("digest changed")
		}
		var longest int64
		for _, line := range strings.Split(raw, "\n") {
			longest = max(longest, int64(len(line)))
		}
		if got := max(h.longest, h.line); got != longest {
			t.Fatalf("longest=%d want=%d", got, longest)
		}
	}
}

func TestVerifiedFramingRemovesOneReadAndKeepsChecks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("visible ", 20000))
	var readBytes int64
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget, Files: framingFiles{bytes: &readBytes}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	file := snap.Input().File
	start := readBytes
	got, err := nativeScannerCharge(t.Context(), file, agentapi.ReadLimits{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	verifiedBytes := readBytes - start
	start = readBytes
	want, err := nativeScannerCharge(t.Context(), unframedFile{file}, agentapi.ReadLimits{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryBytes := readBytes - start
	if got != want || ordinaryBytes-verifiedBytes != file.Length() {
		t.Fatalf("capacity=%d/%d bytes=%d/%d length=%d", got, want, verifiedBytes, ordinaryBytes, file.Length())
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.(agentapi.VerifiedRecordFraming).LongestVerifiedRecord(t.Context()); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed receipt: %v", err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("leaked %d bytes", used)
	}
}

func TestVerifiedFramingRechecksEachReceipt(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"rewrite", "replace", "mode", "append", "cancel"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, "safe visible text")
			info, err := os.Stat(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := pass.Close(); err != nil {
					t.Error(err)
				}
			})
			snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			framing := snap.Input().File.(agentapi.VerifiedRecordFraming)
			longest, err := framing.LongestVerifiedRecord(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch change {
			case "rewrite":
				raw = bytes.Replace(raw, []byte("safe visible text"), []byte("evil visible text"), 1)
				if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(ref.Path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "replace":
				if err := os.Rename(ref.Path, ref.Path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(ref.Path, 0640); err != nil {
					t.Fatal(err)
				}
			case "append":
				f, err := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := io.WriteString(f, strings.Repeat("x", 2*int(longest))+"\n")
				if err := errors.Join(writeErr, f.Close()); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := framing.LongestVerifiedRecord(ctx)
			if change == "append" {
				if err != nil || got != longest {
					t.Fatalf("append receipt=%d err=%v", got, err)
				}
			} else if err == nil {
				t.Fatal("reused stale framing receipt")
			}
		})
	}
}

func TestVerifiedFramingRetainsOversizedRewritePrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("x", 1024))
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	})
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// Remove every newline without changing the captured byte length.
	if err := os.WriteFile(ref.Path, bytes.ReplaceAll(raw, []byte("\n"), []byte(" ")), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = nativeScannerCharge(t.Context(), snap.Input().File, agentapi.ReadLimits{RecordBytes: 512}, budget)
	if !errors.Is(err, archive.ErrRecordTooLarge) {
		t.Fatalf("rewrite precedence: %v", err)
	}
}

func BenchmarkOrdinaryInspectionFraming(b *testing.B) {
	dir := b.TempDir()
	texts := make([]string, 128)
	for i := range texts {
		texts[i] = strings.Repeat("Synthetic explanation. ", 700)
	}
	ref, _ := historyFile(b, dir, threadA, threadA, 0, nil, texts...)
	for _, mode := range []string{"cold-original-framing", "cold-verified-framing", "warm-proof"} {
		b.Run(mode, func(b *testing.B) {
			var readBytes int64
			budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
			pass, err := (SourceProvider{}).OpenPass(b.Context(), agentapi.SourceEnvironment{ReadBudget: budget, Files: framingFiles{bytes: &readBytes}})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := pass.Close(); err != nil {
					b.Error(err)
				}
			})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				snap, err := pass.Read(b.Context(), ref, agentapi.ReadLimits{})
				if err != nil {
					b.Fatal(err)
				}
				proof := snap.(agentapi.VerifiedContentSnapshot)
				if _, err := proof.ContentProof(b.Context()); err != nil {
					b.Fatal(err)
				}
				if mode != "warm-proof" {
					in := snap.Input()
					if mode == "cold-original-framing" {
						in.File = unframedFile{in.File}
					}
					if _, err := (Filter{}).InspectFilteredImport(b.Context(), in, agentapi.FilterContext{Filename: ref.Path, Limits: agentapi.ReadLimits{ReadBudget: budget}}); err != nil {
						b.Fatal(err)
					}
				}
				if err := proof.VerifyContent(b.Context()); err != nil {
					b.Fatal(err)
				}
				if err := snap.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(readBytes)/float64(b.N), "read-bytes/op")
		})
	}
}

func TestVerifiedFramingMatchesFallbackCapacityAndLimits(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{"", `{"type":"event_msg","payload":{"message":"tail"}}`, `{"type":"event_msg"`} {
		t.Run(tail, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("x", 100000))
			if err := os.WriteFile(ref.Path, append(raw, []byte(tail)...), 0600); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
			pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := pass.Close(); err != nil {
					t.Error(err)
				}
			})
			snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			file := snap.Input().File
			for _, limit := range []agentapi.ReadLimits{{}, {RecordBytes: 512}, {RecordBytes: 64 << 10}, {RecordBytes: 128 << 10}, {RawBytes: 1}} {
				before, _ := budget.Charged()
				got, gotErr := nativeScannerCharge(t.Context(), file, limit, budget)
				want, wantErr := nativeScannerCharge(t.Context(), unframedFile{file}, limit, budget)
				if got != want || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
					t.Fatalf("limit=%+v got=%d/%v want=%d/%v", limit, got, gotErr, want, wantErr)
				}
				if after, _ := budget.Charged(); after != before {
					t.Fatalf("preflight charge leaked: %d/%d", after, before)
				}
			}
		})
	}
}
