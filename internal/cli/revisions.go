package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// loadArchiveSelection preserves the active read path. Explicit revision reads
// rederive their view from verified historical bytes, never active summaries.
func loadArchiveSelection(ctx context.Context, store storage.ObjectStore, key, revision string, parsers agentapi.ParsersLookup) (archive.Metadata, archive.SourceBundle, error) {
	if revision == "" {
		return reader.RefreshAndLoad(ctx, store, key, reader.Limits{})
	}
	selected, bundle, err := reader.ReadRevision(ctx, store, key, revision, reader.Limits{})
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	analysis, err := analyzeSource(ctx, parsers, bundle)
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, selected.MachineID, selected.StartedAt, selected.MetadataDerivedAt, selected.SourceBundle, currentRevisionParser(parsers, bundle))
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	metadata.ProjectName = selected.ProjectName
	// Parent-only selection intentionally does not resolve or include children.
	metadata.LinkedSessions = nil
	return metadata, bundle, nil
}

func printRevisionInventory(stdout, stderr io.Writer, metadata archive.Metadata, jsonOut bool) int {
	revisions, err := reader.Revisions(metadata)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	if jsonOut {
		return printJSON(stdout, stderr, struct {
			SessionID  string            `json:"session_id"`
			Harness    string            `json:"harness"`
			HasHistory bool              `json:"has_history"`
			Revisions  []reader.Revision `json:"revisions"`
		}{metadata.SessionID, metadata.Harness.Name, metadata.History != nil, revisions})
	}
	if len(revisions) == 0 {
		_, err = fmt.Fprintln(stdout, "No native revision history for this session.")
	} else {
		for _, r := range revisions {
			state := "preserved"
			if r.Active {
				state = "active"
			}
			filter := r.FilterVersion
			if filter == "" {
				filter = "unknown (legacy)"
			}
			if _, err = fmt.Fprintf(stdout, "%s  %s  %s  filter=%s\n", r.RevisionID, state, r.CapturedAt.Format("2006-01-02T15:04:05Z07:00"), archive.DisplayLine(filter)); err != nil {
				break
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: write revisions: %v\n", err)
		return 1
	}
	return 0
}

func currentRevisionParser(parsers agentapi.ParsersLookup, bundle archive.SourceBundle) archive.ParserInfo {
	parser, ok := parsers.LookupParser(bundle.Capture.Harness.Name)
	if !ok {
		return archive.ParserInfo{}
	}
	return archive.ParserInfo{Name: bundle.Capture.Harness.Name, Version: parser.Version()}
}
