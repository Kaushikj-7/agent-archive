package cli

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// sourceConsumerHTTP serves only synthetic immutable fixture objects to S3.
type sourceConsumerHTTP struct {
	objects     map[string][]byte
	mu          sync.Mutex
	sourceBytes int
}
type sourceConsumerBody struct {
	io.Reader
	transport *sourceConsumerHTTP
	source    bool
}

func (b sourceConsumerBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if b.source {
		b.transport.mu.Lock()
		b.transport.sourceBytes += n
		b.transport.mu.Unlock()
	}
	return n, err
}
func (sourceConsumerBody) Close() error { return nil }
func (h *sourceConsumerHTTP) Do(req *http.Request) (*http.Response, error) {
	key := strings.TrimPrefix(req.URL.Path, "/synthetic/")
	data, ok := h.objects[key]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
		data = []byte("<Error><Code>NoSuchKey</Code></Error>")
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: sourceConsumerBody{bytes.NewReader(data), h, strings.Contains(key, "/source.")}, ContentLength: int64(len(data))}, nil
}

func TestArchiveSourceConsumersUseBoundedS3Reads(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, true)
	objects, err := f.mem.List(t.Context(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string][]byte{}
	sourceSize := 0
	for _, object := range objects {
		raw, err := f.mem.Get(t.Context(), object.Key)
		if err != nil {
			t.Fatal(err)
		}
		snapshot[object.Key] = raw
		if strings.Contains(object.Key, "/source.") {
			sourceSize = len(raw)
		}
	}
	if sourceSize == 0 {
		t.Fatal("fixture has no source")
	}
	for _, command := range [][]string{{"show", f.id, "--harness", "codex", "--normalized"}, {"handoff", f.id, "--source", "archive"}, {"eval", "export", f.id}} {
		t.Run(command[0], func(t *testing.T) {
			for _, excess := range []bool{false, true} {
				transport := &sourceConsumerHTTP{objects: map[string][]byte{}}
				for key, data := range snapshot {
					if excess && strings.Contains(key, "/source.") {
						data = append(append([]byte(nil), data...), bytes.Repeat([]byte("x"), 4096)...)
					}
					transport.objects[key] = data
				}
				cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: transport}
				store, err := storage.NewS3Store(storage.S3StoreOptions{Client: storage.NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "synthetic"})
				if err != nil {
					t.Fatal(err)
				}
				env := f.env
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
				var out, errOut bytes.Buffer
				code := Run(command, nil, &out, &errOut, env)
				if !excess && (code != 0 || !strings.Contains(out.String(), "widget")) {
					t.Fatalf("success: code=%d out=%s err=%s", code, out.String(), errOut.String())
				}
				if excess && code == 0 {
					t.Fatalf("oversized source accepted: %s", out.String())
				}
				wantRead := sourceSize
				if excess {
					wantRead++
				}
				if transport.sourceBytes != wantRead {
					t.Fatalf("excess=%v source bytes=%d want=%d, code=%d stderr=%s", excess, transport.sourceBytes, wantRead, code, errOut.String())
				}
			}
		})
	}
}
