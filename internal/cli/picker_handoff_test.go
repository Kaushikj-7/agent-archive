package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func scopedPickerCatalog(t *testing.T, mem *storagetest.MemoryStore) *catalog.Store {
	t.Helper()
	legacy := privateCatalogFromLegacy(t, mem)
	raw := &scopedMigrationCLIStore{legacy.ObjectStore.(*qualifiedCLIStore)}
	dest := destination.Config{Provider: "s3", Bucket: "private-picker-test", ArchiveFormat: destination.FormatCatalogV4}
	upgrade, err := catalog.OpenPickerUpgrade(t.Context(), raw, dest, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := upgrade.Step(t.Context(), 64)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = upgrade.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = upgrade.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func TestRealCLIBoundedHandoffStartupAndOlderReadNoBodies(t *testing.T) {
	a := newScopedArchive(t)
	for i := range 105 {
		a.add(t, fmt.Sprintf("picker-%03d", i), fmt.Sprintf("Title %03d", i), a.label, func(m *archive.Metadata) { m.Counts.Turns = nil })
	}
	remote := scopedPickerCatalog(t, a.mem)
	counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	wrapped, err := catalog.Wrap(counts)
	if err != nil {
		t.Fatal(err)
	}
	env := a.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	out, stderr, code := runPicker(t, env, "q\n", "--all-projects", "--source", "archive")
	if code != 0 {
		t.Fatal(code, stderr)
	}
	if !strings.Contains(out, "Older sessions") {
		t.Fatal("no Older action", out)
	}
	if counts.Metrics().Lists != 0 || counts.paths["body"] != 0 || counts.paths["other"] != 0 {
		t.Fatalf("startup enumerated archive/bodies: %+v paths=%v", counts.Metrics(), counts.paths)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	choices, model, _, used, err := boundedHandoffChoices(catalog.WithReadView(t.Context()), env, wrapped, home, handoffOptions{source: "archive"}, sessionScope{All: true}, nil, listFormatOptions{Now: env.now(), Numbered: true}, io.Discard)
	if err != nil || !used {
		t.Fatal(used, err)
	}
	if len(choices.shown().rows) != 50 {
		t.Fatal(len(choices.shown().rows))
	}
	if _, err = choices.loadOlder.Load(); err != nil {
		t.Fatal(err)
	}
	if len(choices.shown().rows) != 100 {
		t.Fatal(len(choices.shown().rows))
	}
	if model.searchError() != nil || counts.paths["body"] != 0 || counts.Metrics().Lists != 0 {
		t.Fatal("Older read complete archive")
	}
}

func TestRealCLIBoundedHandoffSelectionRejectsHeadRewriteBeforeSources(t *testing.T) {
	f := newPickerFixture(t)
	remote := scopedPickerCatalog(t, f.mem)
	env := f.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	input := &bareSelectionInput{Reader: strings.NewReader("1\n"), arm: func() {
		key, _ := archive.MetadataObjectKey("codex", f.archiveOnly)
		if err := remote.DeleteSession(context.Background(), key); err != nil {
			t.Error(err)
		}
	}}
	var out, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&out) }
	code := Run([]string{"handoff", "--all-projects", "--source", "archive"}, input, &out, &stderr, env)
	if code == 0 {
		t.Fatalf("selected a stale rewrite: %s %s", out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "refresh") {
		t.Fatalf("missing refresh diagnostic: %s", stderr.String())
	}
}

func TestBoundedHandoffFreshArchiveReplacesOfflineUnknownView(t *testing.T) {
	f := newPickerFixture(t)
	regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	scope := sessionScope{All: true}
	opts := handoffOptions{source: "auto"}
	format := listFormatOptions{Now: f.env.now()}
	offline, prior, err := boundedLocalHandoffChoices(t.Context(), f.env, f.home, opts, scope, regs, format, io.Discard)
	if err != nil || !prior.absenceUnknown || !offline.shown().incomplete {
		t.Fatalf("offline view lost uncertainty: err=%v", err)
	}
	remote := scopedPickerCatalog(t, f.mem)
	fresh, model, _, used, err := boundedHandoffChoices(catalog.WithReadView(t.Context()), f.env, remote, f.home, opts, scope, regs, format, io.Discard)
	if err != nil || !used {
		t.Fatal(used, err)
	}
	if model == prior || model.absenceUnknown || fresh.shown().incomplete || len(fresh.shown().rows) != 3 {
		t.Fatalf("fresh captured model retained offline state: view=%+v", fresh.shown())
	}
	if !prior.absenceUnknown || !offline.shown().incomplete {
		t.Fatal("fresh model changed the prior captured view")
	}
}

func TestRealCLIBoundedHandoffNativeFailureKeepsEligibilityUnknown(t *testing.T) {
	f := newHandoffFixture(t, false)
	remote := scopedPickerCatalog(t, f.mem)
	regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.project, "codex.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := f.env.now()
	env := f.env
	removed := false
	env.Now = func() time.Time {
		if !removed {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			removed = true
		}
		return now
	}
	scope := sessionScope{Label: "fixture", Dir: f.project, ProjectIDs: []string{regs[0].ProjectID}}
	choices, model, _, used, err := boundedHandoffChoices(catalog.WithReadView(t.Context()), env, remote, f.home, handoffOptions{source: "auto"}, scope, regs, listFormatOptions{Now: now}, io.Discard)
	if err != nil || !used {
		t.Fatal(used, err)
	}
	view := choices.shown()
	if choices.fellBack || choices.current != viewScope || view.holdsNothing() || !view.incomplete || view.total != -1 || len(view.rows) != 0 {
		t.Fatalf("native failure became proven empty or widened scope: fallback=%v scope=%d view=%+v", choices.fellBack, choices.current, view)
	}
	if len(model.labels) != 0 {
		t.Fatal("unknown native failure cached as no prompt")
	}
	active := model.activity[pickerID(regs[0].Harness.Name, regs[0].ArchiveSessionID)]
	compatibility := handoffPicker{ctx: t.Context(), env: env, home: f.home, source: "auto", localLabels: model.labels}
	if _, ok := compatibility.localMetadata(regs[0], active); ok || len(model.labels) != 0 {
		t.Fatal("shared search cache stored an unknown native failure")
	}
	must(t, os.WriteFile(path, raw, 0o600))
	retried, err := model.Older(t.Context(), scope)
	if err != nil || len(retried.Rows) != 1 || retried.Incomplete || retried.Empty || retried.LabelAttempts != 2 {
		t.Fatalf("native failure was not retryable: %+v %v", retried, err)
	}
}

type activityFailureSource struct {
	agentapi.SourceProvider
	err   error
	calls *int
}

func (s activityFailureSource) Activities(context.Context, agentapi.SourceEnvironment, []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	if s.calls != nil {
		*s.calls++
	}
	return nil, s.err
}

func TestRealCLIBoundedHandoffActivityFailureCannotProveEmpty(t *testing.T) {
	for _, source := range []string{"auto", "local", "archive"} {
		for _, offline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/offline=%t", source, offline), func(t *testing.T) {
				f := newHandoffFixture(t, false)
				remote := scopedPickerCatalog(t, f.mem)
				bindings := []builtin.Integration{}
				for _, descriptor := range productionAgents.Catalog().All() {
					binding, ok := productionAgents.Lookup(string(descriptor.ID))
					if !ok {
						t.Fatal(descriptor.ID)
					}
					binding.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
					if descriptor.ID == agentmeta.Codex {
						binding.Sources = activityFailureSource{SourceProvider: binding.Sources, err: errors.New("synthetic activity failure")}
					}
					bindings = append(bindings, binding)
				}
				registry, err := builtin.New(productionAgents.Catalog(), bindings)
				if err != nil {
					t.Fatal(err)
				}
				env := f.env
				env.Agents = registry
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
					if offline {
						return nil, errors.New("synthetic archive failure")
					}
					return remote, nil
				}
				out, stderr, code := runPicker(t, env, "q\n", "--source", source)
				if code == 0 || strings.Contains(out, "No sessions match.") || (!offline && strings.Contains(stderr, "archive could not be read")) {
					t.Fatalf("activity/archive failure became empty or false archive fallback: source=%s code=%d out=%s err=%s", source, code, out, stderr)
				}
				if (!offline || source != "archive") && !strings.Contains(stderr, "activity") {
					t.Fatalf("missing activity diagnostic: %s", stderr)
				}
			})
		}
	}
}

func TestBoundedHandoffSearchPreservesArchiveMatchesWhenLocalLabelsFail(t *testing.T) {
	f := newPickerFixture(t)
	archived, err := loadSessionsForBrowse(f.env, f.mem, listOptions{}, io.Discard, "handoff")
	if err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.project, "native-new.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	now := f.env.now()
	env := f.env
	removed := false
	env.Now = func() time.Time {
		if !removed {
			must(t, os.Remove(path))
			removed = true
		}
		return now
	}
	var readErr error
	p := handoffPicker{ctx: t.Context(), env: env, home: f.home, source: "auto", archiveRead: true, scope: sessionScope{All: true}, localLabels: map[localLabelKey]localLabelResult{}, readError: &readErr}
	rows, total, partial := p.rows(regs, archived, 1000)
	if readErr != nil || total != -1 || !partial || len(rows) != 2 {
		t.Fatalf("local label failure erased known archive matches or appeared complete: rows=%d total=%d partial=%v err=%v", len(rows), total, partial, readErr)
	}
	found := map[string]bool{}
	for _, row := range rows {
		found[row.metadata.SessionID] = true
	}
	if !found[f.archiveOnly] || !found[f.both] {
		t.Fatal(found)
	}
	for _, reg := range regs {
		if reg.ArchiveSessionID == f.notUploaded {
			if _, cached := p.localLabels[pickerLocalLabelKey(reg, info.ModTime(), "auto")]; cached {
				t.Fatal("shared search cache retained unknown label failure")
			}
		}
	}
	bindings := []builtin.Integration{}
	calls := 0
	for _, descriptor := range productionAgents.Catalog().All() {
		binding, ok := productionAgents.Lookup(string(descriptor.ID))
		if !ok {
			t.Fatal(descriptor.ID)
		}
		binding.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
		if descriptor.ID == agentmeta.Codex {
			binding.Sources = activityFailureSource{SourceProvider: binding.Sources, err: errors.New("synthetic activity failure"), calls: &calls}
		}
		bindings = append(bindings, binding)
	}
	registry, err := builtin.New(productionAgents.Catalog(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	env.Agents = registry
	p.env = env
	readErr = nil
	rows, total, partial = p.rows(regs, archived, 1000)
	if readErr == nil || len(rows) != 0 || total != -1 || !partial || calls != 1 {
		t.Fatalf("activity failure returned a complete search or duplicated inventory: rows=%d total=%d partial=%v calls=%d err=%v", len(rows), total, partial, calls, readErr)
	}
}
