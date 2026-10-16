package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func runRecoverCommand(args []string, stdout, stderr io.Writer, env Env) (code int) {
	fs := env.newCommandFlags("recover", stderr)
	confirm := fs.Bool("confirm", false, "start or resume the reviewed archive generation")
	id, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if id == "" {
		return fs.usageError("a full local SESSION_ID is required")
	}
	fail := func(err error) int { terminal.Printf(stderr, "agent-archive: recover: %v\n", err); return 1 }
	home, err := env.readHome()
	if err != nil {
		return fail(err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(errNotSetUp)
	}
	store := state.OpenReadOnly(home)
	receiptStore, closeReceipt := store.WithReadBudget(context.Background(), agentapi.NewNativeReadBudget(128<<20))
	defer closeReceipt()
	if next, recorded, err := receiptStore.GenerationSuccessor(id); err != nil {
		return fail(err)
	} else if recorded {
		closeReceipt() // only the bounded successor ID survives this read
		if *confirm {
			unlock, err := lockCollector(home, "recover", env.now())
			if err != nil {
				return fail(err)
			}
			defer unlock()
			store, err = state.Open(home)
			if err != nil {
				return fail(err)
			}
			store, closeResume := store.WithReadBudget(context.Background(), agentapi.NewNativeReadBudget(128<<20))
			defer closeResume()
			if err := store.ResumeGenerationRecoveries(context.Background()); err != nil {
				return fail(err)
			}
		}
		terminal.Printf(stdout, "Generation %s already links to %s. Repeating recovery preserves that identity and its original capture time.\n", id, next)
		return 0
	}
	closeReceipt()
	preview, err := loadGenerationPreview(store, cfg, id, env)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := preview.close(); err != nil {
			code = fail(err)
		}
	}()
	reg, at, builder := preview.reg, preview.at, preview.build
	guidedExplanation(stdout, "Transcript recovery", fmt.Sprintf("Session %s cannot prove that the current transcript extends its retained history.", id))
	terminal.Println(stdout)
	guidedExplanation(stdout, "Consequences",
		fmt.Sprintf("Recovery preserves its archived history, feedback and handoffs under %s, freezes further native capture there, and starts a new linked archive ID from the current filtered transcript. Missing earlier records remain a capture gap.", id),
		"Each generation expires independently under normal retention. Existing subagents keep their original identities and parent; new subagents use the active generation. Imported generations remain in their original undo batch.",
		"The new generation is queued locally for normal sync. Recovery permanently requires a generation-aware writer; older binaries will refuse this data directory.")
	terminal.Println(stdout)
	if !*confirm {
		terminal.Printf(stdout, "To start this generation, run agent-archive recover %s --confirm.\n", id)
		return 0
	}
	next, err := confirmGenerationRecovery(home, reg, cfg, at, builder, preview.budget)
	if err != nil {
		return fail(err)
	}
	terminal.Printf(stdout, "Queued new generation %s, linked to %s. Future hooks route to the new ID. Run agent-archive sync to publish it.\n", next, id)
	return 0
}

type generationPreview struct {
	budget *agentapi.NativeReadBudget
	close  func() error
	reg    archive.SessionRegistration
	at     time.Time
	build  func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error)
}

func loadGenerationPreview(store *state.Store, cfg config.Config, id string, env Env) (preview generationPreview, resultErr error) {
	reg, found, err := store.LoadRegistration(id)
	if err != nil {
		return generationPreview{}, err
	}
	if !found {
		return generationPreview{}, errors.New("session is not registered on this machine; use its full local archive ID")
	}
	if !cfg.AcceptSession(reg) {
		return generationPreview{}, errors.New("session is outside current capture permission or storage destination; review setup first")
	}
	lookup, homes, err := passCodexRollouts(context.Background(), store, cfg, env)
	if err != nil {
		return generationPreview{}, err
	}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, lookup.CloseReadOnly())
		}
	}()
	scoped, closeLocal := store.WithReadBudget(context.Background(), lookup.NativeReadBudget())
	defer closeLocal()
	if err := scoped.CheckHistoryRecovery(id); err != nil {
		return generationPreview{}, err
	}
	store = scoped
	summary, found, err := store.LoadPublishedSummary(id)
	if err != nil {
		return generationPreview{}, err
	}
	if !found || !summary.Published || summary.BlockedReason != state.BlockedReasonTranscriptRewritten {
		return generationPreview{}, errors.New("session has no published transcript_rewritten block")
	}
	if reg.CaptureFrozen {
		return generationPreview{}, errors.New("session is a frozen earlier generation; select its active successor")
	}
	if outstanding, err := store.Outstanding(reg, false); err != nil {
		return generationPreview{}, err
	} else if outstanding.Upload {
		return generationPreview{}, errors.New("settle pending publication with agent-archive sync before recovery")
	}
	closeLocal() // retained authority validation has no remaining data consumer
	opts := collector.Options{CodexRollouts: lookup, ConfiguredCodexHomes: homes, NativeRoots: cfg.NativeRoots, Sources: registryFor(env), Parsers: parsersFor(env), MachineID: cfg.MachineID, SkillEvidence: cfg.EffectiveSkillEvidence(), RequireSkillUse: cfg.RequireSkillUse, RepoKey: env.repoKey}
	at := env.now().UTC()
	builder, closeData, err := collector.PrepareGenerationRecovery(context.Background(), reg, at, opts)
	if err != nil {
		return generationPreview{}, err
	}
	keep = true
	return generationPreview{reg: reg, at: at, build: builder, budget: lookup.NativeReadBudget(), close: func() error { closeData(); return lookup.CloseReadOnly() }}, nil
}

func confirmGenerationRecovery(home string, reg archive.SessionRegistration, cfg config.Config, at time.Time, builder func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error), budget *agentapi.NativeReadBudget) (string, error) {
	if cfg.Paused {
		return "", errPaused
	}
	unlock, err := lockCollector(home, "recover", at)
	if err != nil {
		return "", err
	}
	defer unlock()
	store, err := state.Open(home)
	if err != nil {
		return "", err
	}
	store, closeLocal := store.WithReadBudget(context.Background(), budget)
	defer closeLocal()
	// Fence downgrade writers before committing any generation transition. Setup
	// writes use hooks.lock too; revalidation inside the builder closes the gap
	// between this fence and state.BeginGenerationRecovery's own short hold.
	hooks, err := local.NamedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return "", err
	}
	current, found, err := config.Load(home)
	if err == nil && (!found || current.Paused || !current.AcceptSession(reg)) {
		err = errors.New("capture permission changed; preview recovery again")
	}
	if err == nil {
		current.GenerationProtection = true
		err = config.Save(home, current)
	}
	hooks()
	if err != nil {
		return "", err
	}
	protectedBuilder := func(latest archive.SessionRegistration, id string) (archive.SessionRegistration, state.PendingPublication, error) {
		current, found, err := config.Load(home)
		if err != nil {
			return latest, state.PendingPublication{}, err
		}
		if !found || !current.GenerationProtection || current.Paused || !current.AcceptSession(latest) || current.EffectiveSkillEvidence() != cfg.EffectiveSkillEvidence() || current.RequireSkillUse != cfg.RequireSkillUse {
			return latest, state.PendingPublication{}, errors.New("capture policy changed; preview recovery again")
		}
		return builder(latest, id)
	}
	next, err := store.BeginGenerationRecovery(reg.ArchiveSessionID, at, protectedBuilder)
	if err != nil {
		return "", err
	}
	return next, nil
}
