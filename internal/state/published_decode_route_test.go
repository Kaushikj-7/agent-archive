package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
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
