package catalog

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

type nativeChildTransition string

const (
	nativeChildUnresolved nativeChildTransition = "unresolved"
	nativeChildLinked     nativeChildTransition = "linked"
	nativeChildUnlinked   nativeChildTransition = "unlinked"
	nativeChildReplay     nativeChildTransition = "replay"
	nativeChildDeleted    nativeChildTransition = "deleted"
)

func TestNativeChildOverflowCountsFollowResolvedParentOnly(t *testing.T) {
	writer, raw := fixture(t)
	parent := mutation(t, writer, "native-parent")
	if _, err := writer.Commit(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	child := largeLinkedMutation(t, writer, "native-child")
	child.Next.Summary.NativeChild = true
	child = replaceFixtureBody(t, writer, child)
	revision, err := writer.Commit(t.Context(), child)
	if err != nil {
		t.Fatal(err)
	}
	child.ExpectedRevision = revision
	activateFixture(t, writer)
	for _, stage := range []nativeChildTransition{nativeChildUnresolved, nativeChildLinked, nativeChildUnlinked, nativeChildReplay, nativeChildDeleted} {
		if stage != nativeChildUnresolved {
			child.ID = "native-transition/" + string(stage)
			switch stage {
			case nativeChildUnresolved:
				// The initial unresolved commit is handled before this branch.
			case nativeChildLinked:
				child.Next.Summary.ParentSessionID = parent.Next.Summary.SessionID
			case nativeChildUnlinked:
				child.Next.Summary.ParentSessionID = ""
			case nativeChildReplay:
				child.Next.Summary.Replay = &archive.Replay{}
			case nativeChildDeleted:
				child.Next = nil
			}
			if child.Next != nil {
				child = replaceFixtureBody(t, writer, child)
			}
			revision, err = writer.Commit(t.Context(), child)
			if err != nil {
				t.Fatal(stage, err)
			}
			child.ExpectedRevision = revision
		}
		snapshot, err := OpenSnapshot(t.Context(), raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		countRange := func(index Index, prefix string) uint64 {
			t.Helper()
			count, err := snapshot.Count(t.Context(), Query{Index: index, Lower: prefix + "0", Upper: prefix + ":"})
			if err != nil {
				t.Fatal(stage, err)
			}
			return count
		}
		for _, index := range []Index{CaptureIndex, ActivityIndex} {
			if countRange(index, OrderPrefix(false)) != 1 {
				t.Fatal(stage, "native child classified as root")
			}
			expected := uint64(1)
			kind := "ordinary/"
			if stage == nativeChildReplay {
				kind = "replay/"
			}
			if stage == nativeChildDeleted {
				expected = 0
			}
			if countRange(index, "!children/"+kind) != expected {
				t.Fatal(stage, "global child discriminator differs")
			}
		}
		if countRange(ProjectIndex, ChildPrefix("claude", "", false)) != 0 {
			t.Fatal(stage, "invented empty-parent aggregate")
		}
		entry, err := snapshot.Find(t.Context(), parent.SessionKey)
		if err != nil || entry == nil {
			t.Fatal(stage, err)
		}
		expected := uint64(0)
		if stage == nativeChildLinked {
			expected = 1
		}
		if entry.OrdinaryChildren != expected || entry.ReplayChildren != 0 {
			t.Fatal(stage, "parent counter differs", entry.OrdinaryChildren, entry.ReplayChildren)
		}
		if stage == nativeChildDeleted {
			continue
		}
		entry, err = snapshot.Find(t.Context(), child.SessionKey)
		if err != nil || entry == nil || entry.SummaryOverflow != overflowMetadata || !entry.Summary.NativeChild {
			t.Fatal(stage, "overflow dropped native discriminator", err)
		}
		resolved, _, err := snapshot.ResolveRow(t.Context(), Row{Key: child.SessionKey, Entry: *entry})
		if err != nil || !resolved.Entry.Summary.NativeChild || len(resolved.Entry.Summary.LinkedSessions) != 2000 {
			t.Fatal(stage, "full native body authority", err)
		}
		altered := resolved.Entry.Summary
		altered.NativeChild = false
		if err = entry.MatchMetadata(altered); err == nil {
			t.Fatal(stage, "overflow native discriminator mismatch accepted")
		}
	}
}
