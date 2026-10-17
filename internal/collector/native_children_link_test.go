package collector

import (
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestNativeChildLinksRefuseCyclicInventoryBeforeDurableEffects(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	const a = "00000000-0000-0000-0000-000000000001"
	const b = "00000000-0000-0000-0000-000000000002"
	regs := []archive.SessionRegistration{nativeLinkRegistration("a", a, b, home, at), nativeLinkRegistration("b", b, a, home, at)}
	for _, reg := range regs {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotMtimes(t, local.Home())
	p := &pass{ctx: t.Context(), local: local, registrations: regs, now: at}
	p.reconcileNativeLinks()
	if p.result.NativeLinkPendingReasons["parent_identity_conflict"] != 2 || !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatalf("cycle reached durable link state: %+v", p.result)
	}
	for _, reg := range p.registrations {
		if reg.ParentSessionID != "" || reg.NativeLinkVersion != 0 {
			t.Fatalf("cycle linked: %+v", reg)
		}
	}
}

func TestNativeChildLinkKeysSeparateAgentAndSourceRevision(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	codex := nativeLinkRegistration("child", "native", "parent", "/synthetic/native", at)
	muse := codex
	muse.Harness.Name = "muse"
	if nativeParentKey(codex, "parent") == nativeParentKey(muse, "parent") {
		t.Fatal("native IDs collided across agents")
	}
	first := muse
	first.NativeBinding = &archive.NativeSourceBinding{RootRevision: "first"}
	second := muse
	second.NativeBinding = &archive.NativeSourceBinding{RootRevision: "second"}
	if nativeParentKey(first, "parent") == nativeParentKey(second, "parent") {
		t.Fatal("source root revision was discarded")
	}
}

func TestNativeChildLinksKeepUnqualifiedMuseOwnershipUnavailable(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	child := nativeLinkRegistration("child", "native", "parent", t.TempDir(), at)
	child.Harness.Name = "muse"
	// An injected inventory row is not an admitted Muse child. Do not persist
	// it using a fake proof or loosen archive validation just for this control.
	before := snapshotMtimes(t, local.Home())
	p := &pass{ctx: t.Context(), local: local, registrations: []archive.SessionRegistration{child}, now: at}
	p.reconcileNativeLinks()
	if p.result.NativeLinkPendingReasons["native_child_authority_unavailable"] != 1 || !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatalf("unqualified Muse row gained link authority: %+v", p.result)
	}
	if child.Validate() == nil {
		t.Fatal("Muse native child validation unexpectedly enabled")
	}
	// Composite migration belongs solely to Codex; this must return before
	// accessing Store, even with identical logical IDs.
	if !(&pass{}).markLegacyNativeComposite(child, child) {
		t.Fatal("non-Codex child acquired legacy migration work")
	}
}

func TestNativeChildLinksReserveSharedLedgerBeforeGraphAllocation(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg := nativeLinkRegistration("child", "native", "parent", t.TempDir(), at)
	budget := agentapi.NewNativeReadBudget(1)
	before := snapshotMtimes(t, local.Home())
	p := &pass{ctx: t.Context(), local: local, registrations: []archive.SessionRegistration{reg}, now: at,
		opts: Options{sourcePasses: &sourcePassSet{env: agentapi.SourceEnvironment{ReadBudget: budget}}}}
	p.reconcileNativeLinks()
	if p.result.NativeLinkPendingReasons["relationship_budget_pending"] != 1 || !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatalf("budget pressure did not defer graph safely: %+v", p.result)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("refused graph leaked %d bytes", used)
	}
	p.opts.sourcePasses.env.ReadBudget = agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	p.reconcileNativeLinks()
	if used, _ := p.opts.sourcePasses.env.ReadBudget.Charged(); used != 0 {
		t.Fatalf("completed graph leaked %d bytes", used)
	}
}

func TestNativeChildLinksIgnoreOtherAgentsRemovalNamespace(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	const parentNative = "00000000-0000-0000-0000-000000000001"
	parent := nativeLinkRegistration("parent", parentNative, "", home, at)
	child := nativeLinkRegistration("child", "00000000-0000-0000-0000-000000000002", parentNative, home, at)
	for _, reg := range []archive.SessionRegistration{parent, child} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.RecordRemoval("muse", parentNative, state.RemovalReasonUndo, at); err != nil {
		t.Fatal(err)
	}
	p := &pass{ctx: t.Context(), local: local, registrations: []archive.SessionRegistration{child, parent}, now: at}
	p.reconcileNativeLinks()
	if p.registrations[0].ParentSessionID != parent.ArchiveSessionID {
		t.Fatalf("other agent's removal suppressed Codex relationship: %+v", p.result)
	}
}

func TestNativeChildLinksDoNotMutateExcludedChild(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	const parentNative = "00000000-0000-0000-0000-000000000001"
	parent := nativeLinkRegistration("parent", parentNative, "", home, at)
	child := nativeLinkRegistration("child", "00000000-0000-0000-0000-000000000002", parentNative, home, at)
	for _, reg := range []archive.SessionRegistration{parent, child} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotMtimes(t, local.Home())
	p := &pass{ctx: t.Context(), local: local, registrations: []archive.SessionRegistration{child, parent}, now: at,
		opts: Options{AcceptSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID != "child" }}}
	p.reconcileNativeLinks()
	if p.registrations[0].ParentSessionID != "" || !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatal("excluded child acquired durable relationship effects")
	}
}
