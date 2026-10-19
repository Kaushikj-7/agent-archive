package collector

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

type nativeGraphNamespace string

const (
	nativeGraphAgent       nativeGraphNamespace = "agent"
	nativeGraphHome        nativeGraphNamespace = "home"
	nativeGraphProject     nativeGraphNamespace = "project"
	nativeGraphRoot        nativeGraphNamespace = "root"
	nativeGraphDestination nativeGraphNamespace = "destination"
)

func TestNativeChildGraphRejectsCyclesConflictsAndAmbiguousParents(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	makeReg := func(id, parent string) archive.SessionRegistration {
		return nativeLinkRegistration(id, id, parent, "/synthetic/native", at)
	}
	for _, tc := range []struct {
		name string
		regs []archive.SessionRegistration
		bad  []int
	}{
		{"self", []archive.SessionRegistration{makeReg("a", "a")}, []int{0}},
		{"cycle", []archive.SessionRegistration{makeReg("a", "b"), makeReg("b", "a"), makeReg("c", "b")}, []int{0, 1, 2}},
		{"ambiguous", []archive.SessionRegistration{makeReg("child", "parent"), makeReg("parent", ""), func() archive.SessionRegistration {
			r := makeReg("parent", "")
			r.ArchiveSessionID = "other"
			return r
		}()}, []int{0}},
		{"conflicting", []archive.SessionRegistration{func() archive.SessionRegistration {
			r := makeReg("child", "parent")
			r.ParentSessionID = "other"
			return r
		}(), makeReg("parent", "")}, []int{0}},
		{"conflicting roots", []archive.SessionRegistration{func() archive.SessionRegistration {
			r := makeReg("child", "parent")
			r.NativeRootSessionID = "root-a"
			return r
		}(), func() archive.SessionRegistration {
			r := makeReg("parent", "unseen-parent")
			r.NativeRootSessionID = "root-b"
			return r
		}(), makeReg("grandchild", "child")}, []int{0, 2}},
		{"conflicting distant roots", []archive.SessionRegistration{func() archive.SessionRegistration {
			r := makeReg("child", "parent")
			r.NativeRootSessionID = "root-a"
			return r
		}(), makeReg("parent", "ancestor"), func() archive.SessionRegistration {
			r := makeReg("ancestor", "")
			r.NativeRootSessionID = "root-b"
			return r
		}()}, []int{0}},
		{"conflicting terminal root", []archive.SessionRegistration{func() archive.SessionRegistration {
			r := makeReg("child", "parent")
			r.NativeRootSessionID = "different-root"
			return r
		}(), makeReg("parent", "")}, []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := newNativeChildGraph(tc.regs)
			for _, i := range tc.bad {
				if graph.accepts(tc.regs[i]) {
					t.Fatalf("accepted invalid relationship: %+v", tc.regs[i])
				}
			}
		})
	}
}

func TestNativeChildGraphNamespacesAndMissingParentsRemainIndependent(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	child := nativeLinkRegistration("child", "native-child", "native-parent", "/synthetic/native", at)
	parent := nativeLinkRegistration("parent", "native-parent", "native-child", "/synthetic/native", at)
	for _, dimension := range []nativeGraphNamespace{nativeGraphAgent, nativeGraphHome, nativeGraphProject, nativeGraphRoot, nativeGraphDestination} {
		t.Run(string(dimension), func(t *testing.T) {
			other := parent
			switch dimension {
			case nativeGraphAgent:
				other.Harness.Name = "muse"
			case nativeGraphHome:
				other.NativeSourceHome = "/synthetic/other"
			case nativeGraphProject:
				other.ProjectID = "other"
			case nativeGraphRoot:
				other.ProjectRoot = "/synthetic/other"
			case nativeGraphDestination:
				other.DestinationID = "other"
			}
			graph := newNativeChildGraph([]archive.SessionRegistration{child, other})
			if !graph.accepts(child) || !graph.accepts(other) {
				t.Fatal("different namespace fabricated a cycle")
			}
		})
	}
}

func TestNativeChildGraphLongChainAndRepeatedLogicalIDs(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	regs := make([]archive.SessionRegistration, 4096)
	for i := range regs {
		parent := ""
		if i > 0 {
			parent = strconv.Itoa(i - 1)
		}
		regs[i] = nativeLinkRegistration(strconv.Itoa(i), strconv.Itoa(i), parent, "/synthetic/native", at)
		regs[i].SubagentID = "reused-logical-agent"
	}
	graph := newNativeChildGraph(regs)
	for _, reg := range regs {
		if !graph.accepts(reg) {
			t.Fatal("independent native owner collapsed by logical agent ID")
		}
	}
}

func TestNativeChildGraphPreservesExplicitCodexForkRoot(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	parent := nativeLinkRegistration("fork", "fork-native", "", "/synthetic/native", at)
	parent.NativeRootSessionID = "original-root"
	child := nativeLinkRegistration("child", "child-native", parent.NativeSessionID, "/synthetic/native", at)
	child.NativeRootSessionID = parent.NativeRootSessionID
	graph := newNativeChildGraph([]archive.SessionRegistration{child, parent})
	if !graph.accepts(child) || !graph.accepts(parent) {
		t.Fatal("explicit fork provenance replaced by parent thread identity")
	}
}

func TestNativeChildGraphPreservesCodexImmediateParentHistoryScope(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	root := nativeLinkRegistration("root", "native-root", "", "/synthetic/native", at)
	child := nativeLinkRegistration("child", "native-child", root.NativeSessionID, "/synthetic/native", at)
	grandchild := nativeLinkRegistration("grandchild", "native-grandchild", child.NativeSessionID, "/synthetic/native", at)
	child.NativeRootSessionID, child.ParentSessionID = root.NativeSessionID, root.ArchiveSessionID
	grandchild.NativeRootSessionID, grandchild.ParentSessionID = child.NativeSessionID, child.ArchiveSessionID
	regs := []archive.SessionRegistration{root, child, grandchild}
	before := append([]archive.SessionRegistration(nil), regs...)
	graph := newNativeChildGraph(regs)
	for _, reg := range regs {
		if !graph.accepts(reg) {
			t.Fatal("immediate-parent history scope was treated as a conflicting terminal root", reg.NativeSessionID)
		}
	}
	if !reflect.DeepEqual(regs, before) {
		t.Fatal("graph interpretation changed recorded scope or parent evidence")
	}
	regs[2].NativeRootSessionID = "conflicting-explicit-root"
	if newNativeChildGraph(regs).accepts(regs[2]) {
		t.Fatal("history-scope handling relaxed a distinct explicit root claim")
	}
}
