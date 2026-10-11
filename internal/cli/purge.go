package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/purge"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func runPurgeCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		fs := env.newCommandFlags("purge", stderr)
		if !fs.parseFlagsOnly(args) {
			return 2
		}
	}
	if len(args) == 0 {
		terminal.Println(stderr, "agent-archive: purge: choose plan or apply; run agent-archive purge --help")
		return 2
	}
	switch args[0] {
	case "plan":
		return runPurgePlan(args[1:], stdout, stderr, env)
	case "apply":
		return runPurgeApply(args[1:], stdin, stdout, stderr, env)
	default:
		terminal.Printf(stderr, "agent-archive: purge: unknown action %q; run agent-archive purge --help\n", args[0])
		return 2
	}
}

func runPurgePlan(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("purge plan", stderr)
	mode := fs.String("mode", "unreferenced", "unreferenced or old-filter")
	before := fs.String("before-filter", "", "for old-filter, include source versions below this number")
	noPager := fs.Bool("no-pager", false, "print directly to the terminal; do not page through $PAGER")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	selectedMode := purge.Mode(*mode)
	if selectedMode != purge.ModeUnreferenced && selectedMode != purge.ModeOldFilter {
		return fs.usageError("--mode must be unreferenced or old-filter")
	}
	if selectedMode == purge.ModeOldFilter && *before == "" {
		return fs.usageError("--before-filter is required for old-filter")
	}
	if selectedMode == purge.ModeUnreferenced && *before != "" {
		return fs.usageError("--before-filter requires --mode old-filter")
	}
	if selectedMode == purge.ModeOldFilter {
		n, err := strconv.Atoi(*before)
		if err != nil || n <= 0 {
			return fs.usageError("--before-filter must be a positive numeric version")
		}
	}
	store, cfg, found, err := openReadOnlyStore(env)
	if err != nil {
		return purgeError(stderr, err)
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	plan, err := purge.Inventory(context.Background(), store, cfg.DestinationID(), cfg.Storage.Bucket, cfg.Storage.Prefix, selectedMode, *before, env.now())
	if err != nil {
		return purgeError(stderr, err)
	}
	home, err := env.readHome()
	if err != nil {
		return purgeError(stderr, err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return purgeError(stderr, err)
	}
	name := hex.EncodeToString(id) + ".json"
	file := filepath.Join(home, "purge-plans", name)
	if err := local.Write(file, plan); err != nil {
		return purgeError(stderr, err)
	}
	if err := withPager(context.Background(), stdout, stderr, env, *noPager, func(w io.Writer) error {
		printPurgePlan(w, file, plan)
		return nil
	}); err != nil {
		return purgeError(stderr, err)
	}
	return 0
}

// printPurgePlan prints a saved plan, saved at file, for review: every
// candidate key, then the live source references it leaves alone.
func printPurgePlan(w io.Writer, file string, plan purge.Plan) {
	terminal.Printf(w, "Plan: %s\nBucket: %s\nPrefix: %s\nExpires: %s\nDigest: %s\n", file, plan.Bucket, plan.Prefix, plan.ExpiresAt.Format("2006-01-02T15:04:05Z"), plan.Digest)
	terminal.Printf(w, "Unreferenced source deletion candidates (%d):\n", len(plan.Candidates))
	for _, c := range plan.Candidates {
		terminal.Printf(w, "  %s (%d bytes, filter %s)\n", c.Key, c.Size, c.FilterVersion)
	}
	terminal.Printf(w, "Live older-filter source references (%d; never planned for deletion):\n", len(plan.CurrentOldSessions))
	for _, s := range plan.CurrentOldSessions {
		terminal.Printf(w, "  %s -> %s (filter %s)\n", s.MetadataKey, s.SourceKey, s.FilterVersion)
	}
	terminal.Println(w, "Pause every uploading machine before apply. A versioned bucket also retains noncurrent versions and delete markers until an administrator removes them.")
}

func runPurgeApply(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("purge apply", stderr)
	yes := fs.Bool("yes", false, "apply without an interactive confirmation")
	name, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if name == "" {
		return fs.usageError("a PLAN path is required")
	}
	// The digest is typed at a prompt that reads standard input whether or
	// not it is a terminal, so an agent's shell would wait there for it.
	if mode, _ := env.nonInteractive(); mode.on && !*yes {
		return purgeError(stderr, fmt.Errorf("confirming a purge needs a person to type the digest. Nothing was changed. Run again with --yes to delete without asking, only when the person asked for the deletion. Prompts are off because %s; to be asked here anyway, run with %s=0", mode.reason, envNonInteractive))
	}
	home, err := env.readHome()
	if err != nil {
		return purgeError(stderr, err)
	}
	dir := filepath.Join(home, "purge-plans")
	file := filepath.Clean(name)
	if filepath.Dir(file) != dir || filepath.Ext(file) != ".json" {
		return fs.usageError("PLAN must be a local file in %s", dir)
	}
	info, err := os.Lstat(file)
	if err != nil {
		return purgeError(stderr, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return purgeError(stderr, errors.New("plan must be a private regular file (0600)"))
	}
	var plan purge.Plan
	if err := local.Read(file, &plan); err != nil {
		return purgeError(stderr, err)
	}
	// Hold the same local lock as collection and resume through the entire
	// remote deletion pass. The pause check below must be made under it.
	unlock, err := lockCollector(home, "purge apply", env.now())
	if err != nil {
		return purgeError(stderr, fmt.Errorf("collector or settings change is running: %w", err))
	}
	defer unlock()
	cfg, found, err := config.Load(home)
	if err != nil {
		return purgeError(stderr, err)
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	if err := plan.Validate(cfg.DestinationID(), env.now()); err != nil {
		return purgeError(stderr, err)
	}
	if plan.Bucket != cfg.Storage.Bucket || plan.Prefix != cfg.Storage.Prefix {
		return purgeError(stderr, errors.New("purge plan bucket or prefix differs from current setup"))
	}
	if !cfg.Paused {
		return purgeError(stderr, errors.New("pause this machine and every other uploading machine before purge apply"))
	}
	if !*yes {
		if code, confirmed := confirmPurge(plan, stdin, stdout, stderr); !confirmed {
			return code
		}
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return purgeError(stderr, err)
	}
	reportPath := file + ".report"
	report := purge.Report{PlanDigest: plan.Digest}
	for _, c := range plan.Candidates {
		report.Remaining = append(report.Remaining, c.Key)
	}
	if _, err := os.Stat(reportPath); err == nil {
		if err := local.Read(reportPath, &report); err != nil {
			return purgeError(stderr, err)
		}
		if report.PlanDigest != plan.Digest {
			return purgeError(stderr, errors.New("existing report belongs to another plan"))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return purgeError(stderr, err)
	}
	save := func(value purge.Report) error { return local.Write(reportPath, value) }
	if err := save(report); err != nil {
		return purgeError(stderr, err)
	}
	if err := purge.Apply(context.Background(), store, plan, &report, save); err != nil {
		report.Error = err.Error()
		if saveErr := save(report); saveErr != nil {
			return purgeError(stderr, errors.Join(err, fmt.Errorf("report write: %w", saveErr)))
		}
		return purgeError(stderr, fmt.Errorf("%w; deleted %d, remaining %d; resume before expiry with the same plan", err, len(report.Deleted), len(report.Remaining)))
	}
	terminal.Printf(stdout, "Deleted %d unreferenced sources. Report: %s\n", len(report.Deleted), reportPath)
	return 0
}

// confirmPurge asks for the first twelve characters of the plan's digest.
// When it is not confirmed, code is the exit status to return.
func confirmPurge(plan purge.Plan, stdin io.Reader, stdout, stderr io.Writer) (code int, confirmed bool) {
	p := newPrompter(stdin, stdout)
	defer p.close()
	answer, err := p.guidedText(promptModel{Question: fmt.Sprintf("Delete %d unreferenced sources from %s/%s?", len(plan.Candidates), plan.Bucket, plan.Prefix), Helpers: []string{"Type " + plan.Digest[:12] + " to continue. Any other answer cancels."}, Label: "Digest", ResolveReceipt: func(value string) string {
		if value == plan.Digest[:12] {
			return "Purge confirmed"
		}
		return "Purge cancelled"
	}})
	if err != nil {
		return purgeError(stderr, err), false
	}
	if strings.TrimSpace(answer) != plan.Digest[:12] {
		terminal.Println(stderr, "Purge cancelled.")
		return 1, false
	}
	return 0, true
}

func purgeError(stderr io.Writer, err error) int {
	terminal.Printf(stderr, "agent-archive: purge: %v\n", err)
	return 1
}
