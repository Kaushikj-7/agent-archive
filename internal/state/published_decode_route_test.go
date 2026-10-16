package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Both owned and ordinary full loads retain the public decoder's authority and
// syntax refusals, including malformed evidence that must not be rewritten.
func TestPublishedFullLoadRetainsDecoderRefusals(t *testing.T) {
	cases := map[string]string{
		"legacy":            `{"bundle":{},"status":"blocked","blocked_reason":"transcript_missing"}`,
		"foreign version":   `{"publication_version":3,"bundle":{}}`,
		"missing proof":     `{"publication_version":2,"bundle":{}}`,
		"foreign authority": `{"commit":null,"bundle":{}}`,
		"case duplicate":    `{"publication_version":2,"PUBLICATION_VERSION":2,"bundle":{}}`,
		"escaped duplicate": `{"last_published":null,"last_\u0070ublished":null,"bundle":{}}`,
		"malformed":         `{"bundle":`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			for _, owned := range []bool{false, true} {
				s := newTestStore(t)
				data := []byte(raw)
				if err := os.WriteFile(s.publishedPath("session-1"), data, 0600); err != nil {
					t.Fatal(err)
				}
				var direct publishedState
				directErr := json.Unmarshal(data, &direct)
				reader := s
				closeRead := func() {}
				if owned {
					reader, closeRead = s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(1<<20))
				}
				p, err := reader.LoadPublishedState("session-1")
				if (err == nil) != (directErr == nil) {
					t.Fatalf("owned=%v: load=%v direct=%v", owned, err, directErr)
				}
				if directErr == nil {
					if p.state.Status != direct.Status || p.state.BlockedReason != direct.BlockedReason {
						t.Fatal("legacy block changed")
					}
				} else {
					if errors.Is(err, ErrDurableStorageRecovery) != errors.Is(directErr, ErrDurableStorageRecovery) {
						t.Fatalf("recovery refusal changed: %v vs %v", err, directErr)
					}
					var syntax, directSyntax *json.SyntaxError
					if errors.As(err, &syntax) != errors.As(directErr, &directSyntax) {
						t.Fatalf("syntax refusal changed: %v vs %v", err, directErr)
					}
				}
				closeRead()
				after, readErr := os.ReadFile(s.publishedPath("session-1"))
				if readErr != nil || !bytes.Equal(after, data) {
					t.Fatalf("evidence changed: %v", readErr)
				}
			}
		})
	}
}

// Root containment applies to the unbudgeted public load too: neither a final
// file symlink nor a replaced published directory may import outside evidence.
func TestPublishedFullLoadRejectsEscapedFile(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			s := newTestStore(t)
			outside := t.TempDir()
			data := []byte(`{"bundle":{},"status":"blocked","blocked_reason":"transcript_missing"}`)
			target := filepath.Join(outside, "session-1.json")
			if err := os.WriteFile(target, data, 0600); err != nil {
				t.Fatal(err)
			}
			if directory {
				published := filepath.Dir(s.publishedPath("session-1"))
				if err := os.Remove(published); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, published); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(target, s.publishedPath("session-1")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.LoadPublishedState("session-1"); err == nil {
				t.Fatal("outside published state accepted")
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(after, data) {
				t.Fatalf("outside evidence changed: %v", err)
			}
		})
	}
}

func TestPublishedFullLoadStableRegularFileControls(t *testing.T) {
	s := newTestStore(t)
	p, err := s.LoadPublishedState("session-1")
	if err != nil || p.found {
		t.Fatalf("missing state changed: %v", err)
	}
	if err := os.Mkdir(s.publishedPath("session-1"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPublishedState("session-1"); !errors.Is(err, errStateFileUnstable) {
		t.Fatalf("nonregular state accepted: %v", err)
	}
	if err := os.Remove(s.publishedPath("session-1")); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"bundle":{},"status":"blocked","blocked_reason":"transcript_missing"}`)
	contained := filepath.Join(s.Home(), "contained.json")
	if err := os.WriteFile(contained, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "contained.json"), s.publishedPath("session-1")); err != nil {
		t.Fatal(err)
	}
	p, err = s.LoadPublishedState("session-1")
	if err != nil || !p.found || p.state.Status != CacheStatusBlocked {
		t.Fatalf("contained regular state refused: %v", err)
	}
}
