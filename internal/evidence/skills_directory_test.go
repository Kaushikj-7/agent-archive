package evidence

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

type countedSkillDirectory struct {
	entries   []os.DirEntry
	readErr   error
	closeErr  error
	afterRead func()
	requests  []int
	closed    int
}

func (d *countedSkillDirectory) ReadDir(n int) ([]os.DirEntry, error) {
	d.requests = append(d.requests, n)
	if len(d.requests) != 1 {
		panic("directory drained after sentinel")
	}
	if d.afterRead != nil {
		d.afterRead()
	}
	return d.entries, d.readErr
}

func (d *countedSkillDirectory) Close() error {
	d.closed++
	return d.closeErr
}

type poisonedSkillEntry struct{}

func (poisonedSkillEntry) Name() string { panic("overflow Name inspected") }

func (poisonedSkillEntry) IsDir() bool { panic("overflow IsDir inspected") }

func (poisonedSkillEntry) Type() fs.FileMode { panic("overflow Type inspected") }

func (poisonedSkillEntry) Info() (fs.FileInfo, error) { panic("overflow Info inspected") }

func TestReadSkillEntriesBoundsAcquisitionBeforeInspection(t *testing.T) {
	for _, terminal := range []error{nil, io.EOF} {
		d := &countedSkillDirectory{entries: make([]os.DirEntry, maxSkillsPerRoot+1), readErr: terminal}
		for i := range d.entries {
			d.entries[i] = poisonedSkillEntry{}
		}
		got, err := readSkillEntries(d, t.Context())
		if got != nil || !errors.Is(err, errSkillDirectoryAcquired) || skillRootAbsent(err) {
			t.Fatalf("overflow=%v, %v", got, err)
		}
		if !reflect.DeepEqual(d.requests, []int{257}) || d.closed != 1 {
			t.Fatalf("acquisition=%v closes=%d", d.requests, d.closed)
		}
	}
}

type claimingSkillEOFError struct{}

func (claimingSkillEOFError) Error() string { return "claims EOF" }

func (claimingSkillEOFError) Is(target error) bool { return errors.Is(target, io.EOF) }

func TestReadSkillEntriesDistinguishesAcquiredFailureFromAbsence(t *testing.T) {
	readFailure, closeFailure := errors.New("read failed"), errors.New("close failed")
	for _, c := range []struct {
		name  string
		read  error
		close error
		want  []error
	}{
		{"literal_eof", io.EOF, nil, nil},
		{"read_not_exist", os.ErrNotExist, nil, []error{os.ErrNotExist}},
		{"close_not_exist", io.EOF, os.ErrNotExist, []error{os.ErrNotExist}},
		{"both_failures", readFailure, closeFailure, []error{readFailure, closeFailure}},
		{"composite_eof", errors.Join(io.EOF, readFailure), nil, []error{io.EOF, readFailure}},
		{"wrapped_eof", fmt.Errorf("read wrapper: %w", io.EOF), nil, []error{io.EOF}},
		{"claimed_eof", claimingSkillEOFError{}, nil, []error{io.EOF}},
		{"close_eof", nil, io.EOF, []error{io.EOF}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &countedSkillDirectory{readErr: c.read, closeErr: c.close}
			_, err := readSkillEntries(d, t.Context())
			if d.closed != 1 || !reflect.DeepEqual(d.requests, []int{257}) {
				t.Fatal("descriptor not retired exactly once")
			}
			if len(c.want) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, errSkillDirectoryAcquired) || skillRootAbsent(err) {
				t.Fatalf("acquired failure classified absent: %v", err)
			}
			for _, cause := range c.want {
				if !errors.Is(err, cause) {
					t.Fatalf("lost cause %v: %v", cause, err)
				}
			}
		})
	}
	if !skillRootAbsent(&os.PathError{Op: "open", Path: "missing", Err: os.ErrNotExist}) {
		t.Fatal("open absence no longer distinguishable")
	}
}

func TestReadSkillEntriesCancellationClosesBeforeInspection(t *testing.T) {
	for _, before := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		d := &countedSkillDirectory{entries: []os.DirEntry{poisonedSkillEntry{}}, afterRead: cancel}
		if before {
			cancel()
		}
		got, err := readSkillEntries(d, ctx)
		if got != nil || !errors.Is(err, context.Canceled) || d.closed != 1 {
			t.Fatalf("cancellation=%v %v closed=%d", got, err, d.closed)
		}
		wantReads := 1
		if before {
			wantReads = 0
		}
		if len(d.requests) != wantReads {
			t.Fatal("canceled directory acquired")
		}
	}
}

func TestObserveSkillsProviderDirectoryBounds(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "cursor", "muse"} {
		for _, count := range []int{0, 1, 256, 257, 513} {
			t.Run(fmt.Sprintf("%s/%d", harness, count), func(t *testing.T) {
				home := t.TempDir()
				provider, ok := builtin.NewBuiltins().LookupSkills(harness)
				if !ok {
					t.Fatal("actual skill provider absent")
				}
				roots := provider.EvidenceRoots(agentapi.SkillLocations{UserHome: home, ProjectRoot: t.TempDir()})
				if len(roots) == 0 {
					t.Fatal("provider has no roots")
				}
				selected := roots[0]
				if err := os.MkdirAll(selected.Path, 0700); err != nil {
					t.Fatal(err)
				}
				// Reverse creation order must still produce the same bounded order.
				for i := count - 1; i >= 0; i-- {
					name := fmt.Sprintf("skill-%03d", i)
					writeSkillFile(t, filepath.Join(selected.Path, name, "SKILL.md"), "---\nname: "+name+"\n---\nordinary content\n")
				}
				budget := agentapi.NewNativeReadBudget(8 << 20)
				got, release, err := ObserveSkillsWithContent(t.Context(), SkillOptions{Harness: harness, ObservedAt: time.Unix(2000000000, 0), Mode: config.SkillEvidenceBody, Locations: roots}, budget)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				var inventory map[string]any
				var names []string
				for _, item := range got.Evidence {
					if item.Payload["scope"] != selected.Scope {
						continue
					}
					switch item.Kind {
					case archive.EvidenceKindSkillInventory:
						inventory = item.Payload
					case archive.EvidenceKindSkillSnapshot:
						names = append(names, item.Payload["name"].(string))
					case archive.EvidenceKindSessionLabels, archive.EvidenceKindSkillDiscovered, archive.EvidenceKindSkillInvocation, archive.EvidenceKindSkillRead, archive.EvidenceKindLifecycleHook, archive.EvidenceKindFinalResponse, archive.EvidenceKindExplicitFeedback, archive.EvidenceKindLinkedSession, archive.EvidenceKindCaptureGap:
						t.Fatalf("unexpected evidence %s", item.Kind)
					default:
						t.Fatalf("unexpected evidence %s", item.Kind)
					}
				}
				if inventory == nil {
					t.Fatal("selected provider root disappeared")
				}
				if count > maxSkillsPerRoot {
					if inventory["root_status"] != "unreadable" || inventory["inventory_complete"] != false || len(inventory["skills"].([]any)) != 0 || len(names) != 0 || len(got.Digests) != 0 {
						t.Fatalf("overflow root retained prefix: %#v", inventory)
					}
					for _, key := range []string{"truncated", "omitted_count", "snapshot_omitted_count"} {
						if _, exists := inventory[key]; exists {
							t.Fatalf("unknown tail claimed %s", key)
						}
					}
				} else {
					if inventory["inventory_complete"] != true || len(names) != count {
						t.Fatalf("bounded inventory=%#v names=%d", inventory, len(names))
					}
					for i, name := range names {
						if name != fmt.Sprintf("skill-%03d", i) {
							t.Fatal("bounded order changed")
						}
					}
				}
				release()
				if used, _ := budget.Charged(); used != 0 {
					t.Fatal("directory companion loan survived release")
				}
			})
		}
	}
}

func TestObserveSkillsOverflowDoesNotConsumeOtherRoot(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	for i := range maxSkillsPerRoot + 1 {
		writeSkillFile(t, filepath.Join(root, fmt.Sprintf("skill-%03d", i), "SKILL.md"), "private overflow body")
	}
	writeSkillFile(t, filepath.Join(other, "valid", "SKILL.md"), "ordinary valid body")
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceBody, config.SkillEvidenceMetadata, config.SkillEvidenceNone} {
		got, err := ObserveSkills(SkillOptions{Harness: "muse", Mode: mode, ObservedAt: time.Unix(2000000000, 0), Locations: []agentapi.SkillRoot{{Path: root, Scope: "overflow", UserBoundary: root}, {Path: other, Scope: "valid", UserBoundary: other}}})
		if err != nil {
			t.Fatal(err)
		}
		if mode == config.SkillEvidenceNone {
			if len(got) != 0 {
				t.Fatal("none acquired inventory")
			}
			continue
		}
		want := 2
		if mode == config.SkillEvidenceBody {
			want = 3
		}
		if len(got) != want {
			t.Fatalf("mode %s evidence count: %#v", mode, got)
		}
		if got[0].Kind != archive.EvidenceKindSkillInventory || got[0].Payload["root_status"] != "unreadable" || got[0].Payload["inventory_complete"] != false || len(got[0].Payload["skills"].([]any)) != 0 {
			t.Fatalf("overflow root was not unknown/empty: %#v", got[0])
		}
		if got[1].Kind != archive.EvidenceKindSkillInventory || got[1].Payload["root_status"] != "present" || got[1].Payload["inventory_complete"] != true {
			t.Fatalf("other root lost: %#v", got[1])
		}
		skills := got[1].Payload["skills"].([]any)
		if len(skills) != 1 {
			t.Fatalf("other root inventory count: %#v", skills)
		}
		valid := skills[0].(map[string]any)
		if valid["name"] != "valid" || valid["sha256"] != fmt.Sprintf("%x", sha256.Sum256([]byte("ordinary valid body"))) {
			t.Fatalf("other root content changed: %#v", valid)
		}
		if mode == config.SkillEvidenceBody && (got[2].Kind != archive.EvidenceKindSkillSnapshot || got[2].Payload["name"] != "valid" || got[2].Payload["snapshot"] != "ordinary valid body") {
			t.Fatalf("other root snapshot lost: %#v", got[2])
		}
	}
}
