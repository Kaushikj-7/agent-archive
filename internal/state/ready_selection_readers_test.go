package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These reader controls use APIs/fixtures already present on frozen f512. ROOT
// can run identical source bytes on f512 and this correction, without relaxing
// assertions or treating baseline compilation as causal evidence.
func TestReadyPublicationSummaryAndLabelReadersUseValidatedState(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	want := published.Summary()
	summary, found, err := s.LoadPublishedSummary(id)
	if err != nil || !found || !summary.Published || summary.LabelSourceChecksum != want.LabelSourceChecksum {
		t.Fatalf("successful selection unavailable to summary reader: %+v %v %v", summary, found, err)
	}
	checksum, stamp, err := s.LabelRevision(id)
	if err != nil || stamp == "" || checksum != want.LabelSourceChecksum {
		t.Fatalf("selected label revision unavailable: %q %q %v", checksum, stamp, err)
	}
	labels, n, err := s.LoadLabelPublication(id, 8<<20)
	if err != nil || labels == nil || n <= 0 || labels.state.LastPublished.Selection == nil || *labels.state.LastPublished.Selection != *p.Selection {
		t.Fatalf("selected label context unavailable: %d %v", n, err)
	}
	if labels, n, err = s.LoadLabelPublication(id, n-1); err == nil || labels != nil || n != 0 {
		t.Fatal("label aggregate cap relaxed")
	}
}

func TestReadyPublicationLabelDecodeRejectsCorruptRetainedAuthority(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*publishedState){
		"proof": func(next *publishedState) {
			proof := *next.LastPublished.Selection
			proof.SHA256 = strings.Repeat("0", 64)
			next.LastPublished.Selection = &proof
		},
		"source": func(next *publishedState) {
			source := *next.LastPublished.Source
			source.SHA256 = strings.Repeat("0", 64)
			next.LastPublished.Source = &source
		},
		"bundle": func(next *publishedState) { next.Bundle.NativeRecords = []map[string]any{{"content": "foreign"}} },
	} {
		t.Run(name, func(t *testing.T) {
			next := published.state
			snapshot := *next.LastPublished
			next.LastPublished = &snapshot
			change(&next)
			raw, e := json.Marshal(next)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(s.publishedPath(id), raw, 0600); e != nil {
				t.Fatal(e)
			}
			if label, _, e := s.LoadLabelPublication(id, 8<<20); e == nil || label != nil {
				t.Fatal("corrupt selecting label context accepted")
			}
			if _, _, e := s.LoadPublishedSummary(id); e == nil {
				t.Fatal("corrupt selecting summary accepted")
			}
			if _, _, e := s.LabelRevision(id); e == nil {
				t.Fatal("corrupt selecting label revision accepted")
			}
			got, e := os.ReadFile(s.publishedPath(id))
			if e != nil || !bytes.Equal(got, raw) {
				t.Fatal("rejected reader changed retained evidence")
			}
			if e = os.WriteFile(s.publishedPath(id), original, 0600); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestReadyPublicationReadersRequireExactHealthyConfiguration(t *testing.T) {
	for _, bad := range []string{"missing", "{", `{"schema_version":1,"retention_days":90,"machine_id":"synthetic"}`} {
		t.Run(bad, func(t *testing.T) {
			s, p := readySelectionFixture(t)
			id := p.Bundle.ArchiveSessionID
			published, err := s.LoadPublishedState(id)
			if err != nil {
				t.Fatal(err)
			}
			if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.publishedPath(id))
			if err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(s.Home(), "config.json")
			if bad == "missing" {
				err = os.Remove(cfgPath)
			} else {
				err = os.WriteFile(cfgPath, []byte(bad), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if label, _, e := s.LoadLabelPublication(id, 8<<20); e == nil || label != nil {
				t.Fatal("selecting labels accepted absent/damaged capability")
			}
			if _, _, e := s.LoadPublishedSummary(id); e == nil {
				t.Fatal("selecting summary accepted absent/damaged capability")
			}
			if _, _, e := s.LabelRevision(id); e == nil {
				t.Fatal("selecting revision accepted absent/damaged capability")
			}
			got, e := os.ReadFile(s.publishedPath(id))
			if e != nil || !bytes.Equal(got, raw) {
				t.Fatal("configuration rejection changed evidence")
			}
		})
	}
}

func TestReadyPublicationReadersRejectAuthorityAliases(t *testing.T) {
	s, p := readySelectionFixture(t)
	id := p.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	proof, err := json.Marshal(p.Selection)
	if err != nil {
		t.Fatal(err)
	}
	aliases := [][]byte{
		append([]byte(`{"PUBLICATION_VERSION":2,`), raw[1:]...),
		append([]byte(`{"publication_versi\u006fn":2,`), raw[1:]...),
		[]byte(strings.Replace(string(raw), `"last_published":{`, `"last_published":{"SELECTION_COMMIT":`+string(proof)+`,`, 1)),
	}
	for _, alias := range aliases {
		if err = os.WriteFile(s.publishedPath(id), alias, 0600); err != nil {
			t.Fatal(err)
		}
		if label, _, e := s.LoadLabelPublication(id, 8<<20); e == nil || label != nil {
			t.Fatal("duplicate aliased selecting authority accepted by label reader")
		}
		if _, _, e := s.LoadPublishedSummary(id); e == nil {
			t.Fatal("duplicate aliased selecting authority accepted by summary reader")
		}
		if _, _, e := s.LabelRevision(id); e == nil {
			t.Fatal("duplicate aliased selecting authority accepted by revision reader")
		}
	}
}
