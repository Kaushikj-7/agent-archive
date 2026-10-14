package cli

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type qualificationObjectKey string

const (
	qualificationHeadKey        qualificationObjectKey = catalog.HeadKey
	qualificationCoordinatorKey qualificationObjectKey = catalog.CoordinatorKey
)

func TestQualificationFixtureCopiesKeepMutationsPrivate(t *testing.T) {
	t.Parallel()
	metadata := qualificationMetadata(1400, 0)
	started := time.Now()
	constructed := buildQualificationFixture(t, metadata)
	t.Logf("uncached full constructor: %s", time.Since(started))
	started = time.Now()
	first := newQualificationFixture(t, metadata)
	t.Logf("first fixture (build or process seed plus copy): %s", time.Since(started))
	started = time.Now()
	second := newQualificationFixture(t, metadata)
	t.Logf("same-universe private copy: %s", time.Since(started))
	third := cloneQualificationFixture(t, second)
	if constructed.manifest != first.manifest || !reflect.DeepEqual(constructed.metadata, first.metadata) {
		t.Fatal("cached constructor changed the complete metadata universe")
	}
	if first.raw == second.raw || first.store == second.store || second.raw == third.raw {
		t.Fatal("fixture copies share writable stores or providers")
	}
	firstSeed, secondSeed := freezeQualificationFixture(t, first), freezeQualificationFixture(t, second)
	firstObjects, err := first.raw.List(t.Context(), "")
	must(t, err)
	seedBytes := len(firstSeed.head) + len(firstSeed.metadata)
	for _, object := range firstObjects {
		seedBytes += int(object.Size)
	}
	t.Logf("retained immutable seed payload: %d bytes", seedBytes)
	if !reflect.DeepEqual(firstSeed, secondSeed) {
		t.Fatal("reused seed changed bytes, head, metadata or manifest")
	}
	// Mutate caller-owned nested metadata and the first copy's nested metadata.
	metadata[0].Replay.RunID = "caller mutation"
	for i := range first.metadata {
		if first.metadata[i].Counts.Turns != nil {
			*first.metadata[i].Counts.Turns = 999
			break
		}
	}
	first.metadata[0].Title = "private title"
	must(t, first.raw.Put(t.Context(), catalog.HeadKey, []byte("corrupt head")))
	must(t, first.raw.Put(t.Context(), catalog.CoordinatorKey, []byte("corrupt coordinator")))
	for _, object := range firstObjects {
		key := qualificationObjectKey(object.Key)
		if key == qualificationHeadKey || key == qualificationCoordinatorKey {
			continue
		}
		original, err := first.raw.Get(t.Context(), object.Key)
		must(t, err)
		must(t, first.raw.Put(t.Context(), object.Key, []byte("corrupt object")))
		other, err := second.raw.Get(t.Context(), object.Key)
		must(t, err)
		if !bytes.Equal(original, other) {
			t.Fatal("object mutation escaped its private copy")
		}
		break
	}
	// A new invocation must still observe the original exact seed, including
	// pointer-backed metadata, coordinator/head state and all object bodies.
	fresh := newQualificationFixture(t, qualificationMetadata(1400, 0))
	if !reflect.DeepEqual(secondSeed, freezeQualificationFixture(t, fresh)) || !reflect.DeepEqual(secondSeed, freezeQualificationFixture(t, third)) {
		t.Fatal("mutation changed another command copy or the process seed")
	}
	query := catalog.PickerQuery{Scope: catalog.PickerScope{All: true}, Membership: catalog.PickerBrowse, Limit: 50}
	var pages []catalog.PickerPage
	for _, fixture := range []qualificationFixture{second, fresh} {
		snapshot, err := catalog.OpenSnapshot(t.Context(), fixture.store, nil)
		must(t, err)
		page, err := snapshot.PickerPage(t.Context(), query, "")
		must(t, err)
		if page.Next == "" {
			t.Fatal("isolated fixture lost its continuation")
		}
		// Continuations and references contain a private snapshot nonce. Compare
		// the public rows/costs, then exercise each token on its own snapshot.
		older, err := snapshot.PickerPage(t.Context(), query, page.Next)
		must(t, err)
		if len(older.Rows) != 50 {
			t.Fatal("isolated fixture lost its second page")
		}
		for _, candidate := range []*catalog.PickerPage{&page, &older} {
			if candidate.Next != "" {
				candidate.Next = "private snapshot continuation"
			}
			for i := range candidate.Rows {
				candidate.Rows[i].Reference = catalog.PickerReference{}
			}
		}
		pages = append(pages, page, older)
	}
	if !reflect.DeepEqual(pages[0], pages[2]) || !reflect.DeepEqual(pages[1], pages[3]) {
		t.Fatal("isolated fixtures changed page output")
	}
	key, err := archive.MetadataObjectKey("codex", second.metadata[0].SessionID)
	must(t, err)
	must(t, second.store.DeleteSession(t.Context(), key))
	entry, _, err := fresh.store.Writer.Find(t.Context(), key)
	must(t, err)
	if entry == nil {
		t.Fatal("delete escaped its private copy")
	}
	head, err := fresh.raw.Get(t.Context(), catalog.HeadKey)
	must(t, err)
	if storage.SHA256Hex(head) != storage.SHA256Hex(secondSeed.head) {
		t.Fatal("delete changed another head")
	}
	expectedHead := secondSeed.head
	for i := range 4 {
		t.Run(fmt.Sprintf("concurrent-copy-%d", i), func(t *testing.T) {
			t.Parallel()
			private := newQualificationFixture(t, qualificationMetadata(1400, 0))
			*private.metadata[0].Counts.Turns = i + 1
			must(t, private.raw.Put(t.Context(), catalog.HeadKey, []byte("private concurrent mutation")))
			untouched := newQualificationFixture(t, qualificationMetadata(1400, 0))
			if *untouched.metadata[0].Counts.Turns != 0 {
				t.Fatal("concurrent metadata mutation escaped its copy")
			}
			raw, err := untouched.raw.Get(t.Context(), catalog.HeadKey)
			must(t, err)
			if !bytes.Equal(raw, expectedHead) {
				t.Fatal("concurrent object mutation escaped its copy")
			}
		})
	}
}

func TestQualificationLargeFixturesDoNotRetainProcessSeeds(t *testing.T) {
	metadata := qualificationMetadata(1401, 0)
	key := storage.SHA256Hex(qualificationJSON(t, metadata))
	first := newQualificationFixture(t, metadata)
	second := newQualificationFixture(t, metadata)
	qualificationSeeds.Lock()
	_, retained := qualificationSeeds.fixtures[key]
	qualificationSeeds.Unlock()
	if retained {
		t.Fatal("one-use large universe was retained in the process cache")
	}
	if first.raw == second.raw || first.store == second.store {
		t.Fatal("large fixtures share writable state")
	}
	metadata[0].Replay.RunID = "caller mutation"
	*first.metadata[0].Counts.Turns = 999
	if second.metadata[0].Replay.RunID != "" || *second.metadata[0].Counts.Turns != 0 || first.metadata[0].Replay.RunID != "" {
		t.Fatal("large fixture metadata shares caller or sibling pointers")
	}
}
