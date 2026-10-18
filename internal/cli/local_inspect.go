package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type localReadOptions struct {
	harness     string
	project     string
	query       string
	allProjects bool
	json        bool
	transcript  bool
	full        bool
	limit       int
	maxBytes    int
}

const localReadSchemaVersion = 1

type localReadCoverage struct {
	Enumerated       int    `json:"enumerated"`
	Inspected        int    `json:"inspected"`
	InScope          int    `json:"in_scope"`
	Skipped          int    `json:"skipped"`
	IdentityComplete bool   `json:"identity_complete"`
	Reason           string `json:"reason,omitempty"`
	ReservedBytes    int64  `json:"reserved_bytes"`
	ReadBytes        int64  `json:"read_bytes"`
}

func localCoverage(c nativesessions.Coverage) localReadCoverage {
	return localReadCoverage{c.Enumerated, c.Inspected, c.InScope, c.Skipped, c.IdentityComplete, c.Reason, c.ReservedBytes, c.ReadBytes}
}

func localReadFlags(fs *commandFlags, allowed ...string) (err error) {
	fs.Visit(func(f *flag.Flag) {
		if err == nil && !slices.Contains(allowed, f.Name) {
			err = fmt.Errorf("--%s is unavailable with --source local", f.Name)
		}
	})
	return err
}

// Local reads use native discovery and source contracts only. In particular,
// there is no archive home, configuration loader, store or capture operation.
func discoverLocalRead(ctx context.Context, env nativeHandoffDependencies, o localReadOptions) (nativesessions.Result, error) {
	if o.project != "" && o.allProjects {
		return nativesessions.Result{}, errors.New("--project and --all-projects cannot be used together")
	}
	dir := o.project
	if dir == "" && !o.allProjects {
		var err error
		dir, err = env.workingDir()
		if err != nil {
			return nativesessions.Result{}, err
		}
	}
	if !o.allProjects {
		if !filepath.IsAbs(dir) {
			cwd, err := env.workingDir()
			if err != nil {
				return nativesessions.Result{}, err
			}
			if !filepath.IsAbs(cwd) {
				return nativesessions.Result{}, errors.New("local checkout location unavailable")
			}
			dir = filepath.Join(cwd, dir)
		}
		canonical, err := env.nativeFiles().EvalSymlinks(dir)
		if err != nil {
			return nativesessions.Result{}, errors.New("local --project needs an existing checkout directory")
		}
		dir = canonical
		info, err := env.nativeFiles().Lstat(dir)
		if err != nil || !info.IsDir() {
			return nativesessions.Result{}, errors.New("local --project needs an existing checkout directory")
		}
	}
	roots, err := env.nativeRoots(o.harness)
	if err != nil {
		return nativesessions.Result{}, err
	}
	return nativesessions.Discover(ctx, registryFor(env), env.nativeFiles(), roots, nativesessions.Scope{Directories: []string{dir}, All: o.allProjects}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
}

type localReadRow struct {
	NativeID  string               `json:"native_session_id"`
	Harness   string               `json:"harness"`
	Project   string               `json:"project"`
	StartedAt time.Time            `json:"started_at"`
	Name      string               `json:"name,omitempty"`
	Title     string               `json:"title,omitempty"`
	Preview   string               `json:"preview,omitempty"`
	Gaps      []archive.CaptureGap `json:"gaps,omitempty"`
}

func localRow(c nativesessions.Candidate) localReadRow {
	var gaps []archive.CaptureGap
	if c.Ref.Harness == "muse" {
		gaps = []archive.CaptureGap{{Code: "child_coverage_unavailable"}}
	}
	return localReadRow{NativeID: c.NativeID, Harness: c.Ref.Harness, Project: c.Directory, StartedAt: c.StartedAt, Gaps: gaps}
}

func localReadFailure(stderr io.Writer, command string, err error) int {
	terminal.Printf(stderr, "agent-archive: %s: %v\n", command, err)
	return 1
}

func runLocalList(deps any, stdout, stderr io.Writer, o localReadOptions) int {
	env, ok := deps.(nativeHandoffDependencies)
	if !ok {
		return localReadFailure(stderr, "list", errors.New("local native reads unavailable"))
	}
	if o.limit < 0 {
		return localReadFailure(stderr, "list", errors.New("--limit cannot be negative"))
	}
	canonical, ok := harnessFlagWithCatalog(catalogFor(env), o.harness)
	if !ok {
		return localReadFailure(stderr, "list", errors.New(harnessFlagError(o.harness)))
	}
	o.harness = canonical
	ctx, _, stop := nativeCommandContext(env)
	defer stop()
	result, err := discoverLocalRead(ctx, env, o)
	if err != nil {
		return localReadFailure(stderr, "list", err)
	}
	// Preview loads exactly the existing first bounded batch. It never performs
	// full filtering or silently broadens the search when names are unavailable.
	catalog := nativePreviewCatalog{ctx: ctx, files: env.nativeFiles(), sources: registryFor(env), previews: registryFor(env), candidates: result.Candidates, reserved: result.Coverage.ReadBytes, stderr: stderr, now: env.now()}
	more, err := catalog.load()
	if err != nil {
		return localReadFailure(stderr, "list", err)
	}
	rows := make([]localReadRow, 0, len(catalog.rows))
	query := parseSessionQuery(o.query)
	for _, row := range catalog.rows {
		if !query.empty() && !query.matches(row.fields) {
			continue
		}
		index := nativeRowIndex(row, result.Candidates)
		if index < 0 {
			return localReadFailure(stderr, "list", errors.New("invalid native preview selection"))
		}
		item := localRow(result.Candidates[index])
		item.Name, item.Title, item.Preview = row.fields.Name, row.fields.Title, row.Hint
		rows = append(rows, item)
		if o.limit > 0 && len(rows) == o.limit {
			break
		}
	}
	if o.json {
		out := struct {
			SchemaVersion  int               `json:"schema_version"`
			Domain         string            `json:"domain"`
			Source         string            `json:"source"`
			Coverage       localReadCoverage `json:"coverage"`
			PreviewLimited bool              `json:"preview_limited"`
			Sessions       []localReadRow    `json:"sessions"`
		}{localReadSchemaVersion, "local-read-list", "local", localCoverage(result.Coverage), more || catalog.exhausted, rows}
		if err := writeCompactJSON(stdout, out); err != nil {
			return localReadFailure(stderr, "list", err)
		}
	} else {
		for _, row := range rows {
			terminal.Printf(stdout, "%s  %s  %s  %s  %s\n", archive.DisplayLine(row.NativeID), archive.DisplayLine(row.Harness), archive.DisplayLine(row.Project), archive.DisplayLine(row.Name+" "+row.Title), archive.DisplayLine(row.Preview))
		}
		terminal.Printf(stderr, "list: local read-only source; searched %d bounded previews of %d verified identities; discovery complete: %t\n", catalog.next, len(result.Candidates), result.Coverage.IdentityComplete)
	}
	return 0
}

func runLocalShow(deps any, stdout, stderr io.Writer, o localReadOptions) int {
	env, ok := deps.(nativeHandoffDependencies)
	if !ok {
		return localReadFailure(stderr, "show", errors.New("local native reads unavailable"))
	}
	if o.full && o.json {
		return localReadFailure(stderr, "show", errors.New("--full is for the readable transcript"))
	}
	canonical, ok := harnessFlagWithCatalog(catalogFor(env), o.harness)
	if !ok {
		return localReadFailure(stderr, "show", errors.New(harnessFlagError(o.harness)))
	}
	o.harness = canonical
	ctx, _, stop := nativeCommandContext(env)
	defer stop()
	result, err := discoverLocalRead(ctx, env, o)
	if err != nil {
		return localReadFailure(stderr, "show", err)
	}
	selected, err := nativeIdentity(result.Candidates, o.query, o.harness)
	if err != nil {
		return localReadFailure(stderr, "show", err)
	}
	if selected == nil {
		return localReadFailure(stderr, "show", errors.New("native session not found in bounded local discovery; use list --source local or broaden --all-projects"))
	}
	target, err := handoffFromNative(ctx, *selected, env.nativeFiles(), env)
	if err != nil {
		return localReadFailure(stderr, "show", err)
	}
	labels, _ := archive.LabelsFromAnalysis(target.analysis)
	row := localReadRow{NativeID: selected.NativeID, Harness: selected.Ref.Harness, Project: selected.Directory, StartedAt: selected.StartedAt, Name: labels.Name, Title: labels.Title, Gaps: target.bundle.Capture.Gaps}
	if o.json {
		var normalizedView *normalizedOutput

		if o.transcript {
			view := target.analysis.View
			turns := make([]normalizedTurn, len(view.Turns))
			for i, turn := range view.Turns {
				turns[i] = normalizedTurn{NormalizedTurn: turn}
			}
			normalizedView = &normalizedOutput{Turns: turns, ToolCalls: view.ToolCalls, ToolResults: view.ToolResults, HookFinals: view.HookFinals}
		}
		out := struct {
			SchemaVersion     int                              `json:"schema_version"`
			Domain            string                           `json:"domain"`
			Source            string                           `json:"source"`
			Session           localReadRow                     `json:"session"`
			Coverage          localReadCoverage                `json:"coverage"`
			View              *normalizedOutput                `json:"view,omitempty"`
			TokenObservations *archive.TokenObservationSummary `json:"token_observations,omitempty"`
			Observability     map[string]archive.Availability  `json:"observability"`
		}{SchemaVersion: localReadSchemaVersion, Domain: "local-read-show", Source: "local", Session: row, View: normalizedView, Coverage: localCoverage(result.Coverage), TokenObservations: target.analysis.TokenObservations, Observability: localReadObservability(target.analysis.Observability)}

		raw, err := json.Marshal(out)
		if err == nil {
			raw = append(archive.DisplayJSON(raw), '\n')
		}
		if err == nil && o.transcript && o.maxBytes > 0 && len(raw) > o.maxBytes {
			err = errors.New("local transcript exceeds --max-bytes; raise the explicit output limit")
		}
		if err != nil {
			return localReadFailure(stderr, "show", err)
		}
		returnCode := 0
		if err := writeOutput(stdout, raw); err != nil {
			returnCode = localReadFailure(stderr, "show", err)
		}
		return returnCode
	}
	if !o.transcript {
		terminal.Printf(stdout, "%s (%s)\n%s\nStarted: %s\n", archive.DisplayLine(row.NativeID), archive.DisplayLine(row.Harness), archive.DisplayLine(archive.DisplayTitle(archive.Metadata{Name: row.Name, Title: row.Title})), row.StartedAt.Format(time.RFC3339))
	}
	gapOutput := stdout
	if o.transcript {
		gapOutput = stderr
	}
	for _, gap := range row.Gaps {
		terminal.Printf(gapOutput, "Coverage gap: %s\n", archive.DisplayLine(gap.Code))
	}
	if !result.Coverage.IdentityComplete {
		terminal.Printf(stderr, "show: local discovery incomplete (%s); this explicit identity was verified\n", archive.DisplayLine(result.Coverage.Reason))
	}
	if o.transcript {
		t, err := archive.BuildTranscriptWithAnalysis(target.bundle, target.analysis, archive.HandoffOptions{ToolResultLines: transcriptResultLines, ToolResultBytes: transcriptResultBytes})
		if err != nil {
			return localReadFailure(stderr, "show", err)
		}
		view := sessionView{Metadata: archive.Metadata{SessionID: selected.NativeID, NativeSessionID: selected.NativeID, Name: row.Name, Title: row.Title, Harness: target.bundle.Capture.Harness, StartedAt: selected.StartedAt}}
		render := transcriptOptions{summaryOptions: summaryOptions{Now: env.now(), Style: styleFor(stdout)}, Full: o.full}
		if o.maxBytes > 0 {
			var fits bool
			t, fits = archive.FitTranscript(t, o.maxBytes, o.full, func(t archive.Transcript) int { return len(renderTranscriptBytes(view, t, render)) })
			if !fits {
				return localReadFailure(stderr, "show", errors.New("local transcript exceeds --max-bytes after trimming; raise the explicit output limit"))
			}
		}
		if err := writeOutput(stdout, renderTranscriptBytes(view, t, render)); err != nil {
			return localReadFailure(stderr, "show", err)
		}
	}
	return 0
}

func runLocalListCommand(fs *commandFlags, env listCommandDependencies, stdout, stderr io.Writer, o localReadOptions) int {
	if err := localReadFlags(fs, "source", "harness", "limit", "json", "all-projects", "project", "no-pager"); err != nil {
		return fs.usageError("%v", err)
	}
	canonical, ok := harnessFlagWithCatalog(catalogFor(env), o.harness)
	if !ok {
		return fs.usageError("%s", harnessFlagError(o.harness))
	}
	if o.limit < 0 {
		return fs.usageError("--limit cannot be negative")
	}
	o.harness = canonical
	return runLocalList(env, stdout, stderr, o)
}

func dispatchShowSource(fs *commandFlags, env showCommandDependencies, stdout, stderr io.Writer, source string, o localReadOptions) (bool, int) {
	if source == "local" {
		if err := localReadFlags(fs, "source", "harness", "project", "all-projects", "transcript", "full", "json", "no-pager", "max-bytes"); err != nil {
			return true, fs.usageError("%v", err)
		}
		if o.query == "" {
			return true, fs.usageError("--source local needs a native session ID; use list --source local to find one")
		}
		if o.project != "" && o.allProjects {
			return true, fs.usageError("--project and --all-projects cannot be used together")
		}
		return true, runLocalShow(env, stdout, stderr, o)
	}
	if source != "archive" || o.project != "" || o.allProjects {
		return true, fs.usageError("--source must be archive or local; --project and --all-projects need --source local")
	}
	return false, 0
}

func localReadObservability(observed archive.Observability) map[string]archive.Availability {
	out := map[string]archive.Availability{"structured_counts": observed.StructuredCounts, "tool_calls": observed.ToolCalls, "files_touched": observed.FilesTouched, "git_activity": observed.GitActivity, "compactions": observed.Compactions, "tool_errors": observed.ToolErrors, "tool_results": observed.ToolResults}
	for key, value := range out {
		if value.State == "" {
			delete(out, key)
		}
	}
	return out
}
