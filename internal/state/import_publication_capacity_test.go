package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Boundary files are sparse accounting fixtures; the integration regression
// separately exercises canonical production writers and verified publication.
func TestImportDataKeepsPublicationSpaceAtChargedBoundary(t *testing.T) {
	t.Parallel()
	for _, growing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "growing"}[growing], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			const oldSize int64 = 8
			const nextSize int64 = 16
			path := importStageRelative("selected")
			if growing {
				sparseDurable(t, filepath.Join(s.home, path), oldSize)
			}
			additional := 2*nextSize + durableControlBytes
			existing := int64(0)
			if growing {
				additional = 2 * (nextSize - oldSize)
				existing = 2*oldSize + durableControlBytes
			}
			ceiling := durableStorageQuota - importPublicationHeadroom
			sparseDurable(t, filepath.Join(s.home, "pending", "accounted.json"), (ceiling-existing-additional)/2+1)
			err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
				q, err := s.openDurableQuota(g)
				if err != nil {
					return err
				}
				defer func() { err = errors.Join(err, q.Close()) }()
				write := func(w io.Writer) error { _, err := w.Write(make([]byte, nextSize)); return err }
				if err := q.writeWithHeadroom(path, nextSize, write, nil, importPublicationHeadroom); !errors.Is(err, ErrDurableStorageCapacity) {
					t.Fatalf("consumed publication reserve: %v", err)
				}
				if growing {
					info, err := q.home.Root.Stat(path)
					if err != nil || info.Size() != oldSize {
						t.Fatal("refused growth changed evidence", err)
					}
				} else if _, err := q.home.Root.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused stage created data", err)
				}
				file, err := q.home.Root.OpenFile("pending/accounted.json", os.O_WRONLY, 0600)
				if err != nil {
					return err
				}
				err = file.Truncate((ceiling - existing - additional) / 2)
				err = errors.Join(err, file.Close())
				if err != nil {
					return err
				}
				return q.writeWithHeadroom(path, nextSize, write, nil, importPublicationHeadroom)
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImportShrinkAndControlRemainUsableAboveAdmissionCeiling(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	path := importStageRelative("selected")
	sparseDurable(t, filepath.Join(s.home, path), 32)
	sparseDurable(t, filepath.Join(s.home, "pending", "accounted.json"), (durableStorageQuota-durableControlBytes-64-128)/2)
	err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		for _, n := range []int64{32, 16} {
			if err := q.writeWithHeadroom(path, n, func(w io.Writer) error { _, err := w.Write(make([]byte, n)); return err }, nil, importPublicationHeadroom); err != nil {
				return err
			}
		}
		for _, name := range []string{"cancel.json", "release-progress.json"} {
			if err := q.write(filepath.Join(filepath.Dir(path), name), 16, func(w io.Writer) error { _, err := w.Write(make([]byte, 16)); return err }); err != nil {
				return err
			}
		}
		// Generic writers retain the old quota even for the same lexical path.
		return q.write(path, 24, func(w io.Writer) error { _, err := w.Write(make([]byte, 24)); return err })
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Binding facts alone must not classify conflicting registration relationship
// facts as ordinary. These exclusions leave the existing group quota unchanged.
func TestImportHeadroomLeavesNativeRelationshipFactsAtFullQuota(t *testing.T) {
	t.Parallel()
	const id = "00000000-0000-4000-8000-000000000001"
	stage := ImportStage{Version: 1, Registration: archive.SessionRegistration{
		NativeSessionID: id, Harness: archive.Harness{Name: archive.HarnessCodex},
		SourceKind: archive.SourceKindFile, Origin: archive.SessionOriginImport,
		CodexBinding: &archive.CodexSourceBinding{NativeThreadID: id, PhysicalRolloutID: id, RootID: id, ProducerSource: "cli"},
	}}
	if stage.publicationHeadroom() != importPublicationHeadroom {
		t.Fatal("otherwise ordinary profile did not exercise classification")
	}
	for _, related := range []archive.SessionRegistration{
		{SubagentID: "unlinked-child"}, {NativeRootSessionID: "00000000-0000-4000-8000-000000000002"},
	} {
		candidate := stage
		candidate.Registration.SubagentID = related.SubagentID
		candidate.Registration.NativeRootSessionID = related.NativeRootSessionID
		if candidate.publicationHeadroom() != 0 {
			t.Fatal("relationship facts acquired ordinary admission policy")
		}
	}
}
