package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// sourceReadMeter deliberately hides optional capabilities unless forwarded.
type sourceReadMeter struct {
	storage.ObjectStore
	gets, bounded int
	ceiling       int64
}

func (s *sourceReadMeter) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.ObjectStore.Get(ctx, key)
}
func (s *sourceReadMeter) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.bounded++
	s.ceiling = limit
	return s.ObjectStore.(storage.LimitedGetter).GetLimited(ctx, key, limit)
}

func TestLoadSourceBoundsReadsBeforeAllocation(t *testing.T) {
	t.Parallel()
	metadata, _, memory := fixture(t)
	for _, limit := range []int{metadata.SourceBundle.CompressedBytes - 1, 1} {
		meter := &sourceReadMeter{ObjectStore: memory}
		_, err := LoadSource(t.Context(), meter, metadata, Limits{MaxCompressedBytes: limit})
		if !errors.Is(err, storage.ErrObjectTooLarge) || meter.gets != 0 || meter.bounded != 0 {
			t.Fatalf("limit=%d err=%v GET=%d bounded=%d", limit, err, meter.gets, meter.bounded)
		}
	}
	meter := &sourceReadMeter{ObjectStore: memory}
	if _, err := LoadSource(t.Context(), meter, metadata, Limits{MaxCompressedBytes: metadata.SourceBundle.CompressedBytes}); err != nil {
		t.Fatal(err)
	}
	if meter.gets != 0 || meter.bounded != 1 || meter.ceiling != int64(metadata.SourceBundle.CompressedBytes) {
		t.Fatalf("GET=%d bounded=%d ceiling=%d", meter.gets, meter.bounded, meter.ceiling)
	}
	metadata.SourceBundle.CompressedBytes = (32 << 20) + 1
	meter = &sourceReadMeter{ObjectStore: memory}
	if _, err := LoadSource(t.Context(), meter, metadata, Limits{}); !errors.Is(err, storage.ErrObjectTooLarge) || meter.bounded != 0 || meter.gets != 0 {
		t.Fatalf("default limit: %v %+v", err, meter)
	}
}

func TestLoadSourceRejectsNonpositiveDeclaredSizeBeforeReads(t *testing.T) {
	t.Parallel()
	metadata, _, memory := fixture(t)
	for _, size := range []int{0, -1} {
		metadata.SourceBundle.CompressedBytes = size
		meter := &sourceReadMeter{ObjectStore: memory}
		if _, err := LoadSource(t.Context(), meter, metadata, Limits{}); err == nil || meter.gets != 0 || meter.bounded != 0 {
			t.Fatalf("size=%d err=%v GET=%d bounded=%d", size, err, meter.gets, meter.bounded)
		}
	}
}

func TestLoadSourceRequiresBoundedStore(t *testing.T) {
	t.Parallel()
	metadata, _, memory := fixture(t)
	meter := &sourceReadMeter{ObjectStore: memory}
	hidden := struct{ storage.ObjectStore }{meter}
	_, err := LoadSource(t.Context(), hidden, metadata, Limits{})
	if err == nil || !strings.Contains(err.Error(), "require allocation-bounded object reads") || meter.gets != 0 || meter.bounded != 0 {
		t.Fatalf("err=%v GET=%d bounded=%d", err, meter.gets, meter.bounded)
	}
}

// sourceHTTP feeds the real S3 adapter without network or provider state.
type sourceHTTP struct {
	data        []byte
	reads, gets int
}
type sourceBody struct {
	io.Reader
	owner *sourceHTTP
}

func (b sourceBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.owner.reads += n
	return n, err
}
func (sourceBody) Close() error { return nil }
func (h *sourceHTTP) Do(*http.Request) (*http.Response, error) {
	h.gets++
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: sourceBody{strings.NewReader(string(h.data)), h}, ContentLength: int64(len(h.data))}, nil
}
func TestLoadSourceRealAdapterStopsAtDeclaredSize(t *testing.T) {
	t.Parallel()
	metadata, _, memory := fixture(t)
	raw, err := memory.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	for _, excess := range []bool{false, true} {
		t.Run(fmt.Sprint(excess), func(t *testing.T) {
			data := raw
			if excess {
				data = append(append([]byte(nil), raw...), []byte(strings.Repeat("x", 4096))...)
			}
			transport := &sourceHTTP{data: data}
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: transport}
			store, err := storage.NewS3Store(storage.S3StoreOptions{Client: storage.NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = LoadSource(t.Context(), store, metadata, Limits{})
			if excess {
				if !errors.Is(err, storage.ErrObjectTooLarge) || transport.reads != len(raw)+1 {
					t.Fatalf("err=%v bytes=%d", err, transport.reads)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if transport.gets != 1 {
				t.Fatalf("GETs=%d", transport.gets)
			}
		})
	}
}

func TestLoadSourcePreservesCancellationAndVerification(t *testing.T) {
	t.Parallel()
	metadata, _, memory := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := LoadSource(ctx, memory, metadata, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	raw, err := memory.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	if err := memory.Put(t.Context(), metadata.SourceBundle.Key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSource(t.Context(), memory, metadata, Limits{}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal(err)
	}
	metadata, _, memory = fixture(t)
	metadata.NativeSessionID = "different-native-session"
	if _, err := LoadSource(t.Context(), memory, metadata, Limits{}); err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatal(err)
	}
}
