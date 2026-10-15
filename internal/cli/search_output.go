package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/search"
)

type contentSearchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Restart bool   `json:"restart_required"`
}

type contentSearchKind string

const (
	contentSearchResults     contentSearchKind = "search"
	contentSearchContextRead contentSearchKind = "search_context"
	contentSearchRefreshOnly contentSearchKind = "search_refresh"
)

type contentSearchDocument struct {
	SchemaVersion     int                           `json:"schema_version"`
	Kind              contentSearchKind             `json:"kind"`
	OK                bool                          `json:"ok"`
	Error             *contentSearchError           `json:"error"`
	Filters           *contentSearchFiltersDocument `json:"filters,omitempty"`
	Query             string                        `json:"query,omitempty"`
	Scope             *contentSearchScope           `json:"scope,omitempty"`
	Limit             int                           `json:"limit,omitempty"`
	Returned          int                           `json:"returned"`
	Results           []contentSearchHit            `json:"results"`
	Generation        int64                         `json:"generation"`
	NextCursor        *string                       `json:"next_cursor"`
	Truncated         bool                          `json:"truncated"`
	TotalMatchedKnown bool                          `json:"total_matched_known"`
	TotalMatched      *int                          `json:"total_matched"`
	Coverage          *contentSearchCoverage        `json:"coverage"`
	Refresh           *contentSearchRefresh         `json:"refresh,omitempty"`
	Context           *contentSearchContext         `json:"context,omitempty"`
}

type contentSearchFiltersDocument struct {
	Harness  string     `json:"harness"`
	Since    *time.Time `json:"since"`
	Until    *time.Time `json:"until"`
	Replays  string     `json:"replays"`
	Children string     `json:"children"`
}

type contentSearchScope struct {
	Label    string   `json:"label"`
	All      bool     `json:"all_projects"`
	Projects []string `json:"project_ids"`
	Repo     string   `json:"repo_key"`
}

type contentSearchHit struct {
	Session   string             `json:"session_id"`
	Harness   string             `json:"harness"`
	Title     string             `json:"title"`
	Snippet   string             `json:"snippet"`
	Role      string             `json:"role"`
	Captured  time.Time          `json:"captured_at"`
	Matches   int                `json:"match_count"`
	Reference *string            `json:"reference"`
	Gaps      []contentSearchGap `json:"gaps"`
}

type contentSearchGap struct {
	Revision string `json:"revision_id"`
	Reason   string `json:"reason"`
}

type contentSearchCoverage struct {
	Scope       string     `json:"scope"`
	Present     bool       `json:"index_present"`
	Reconciled  *time.Time `json:"last_reconciled"`
	Observed    *time.Time `json:"last_observed"`
	Discovery   bool       `json:"discovery_complete"`
	Checks      bool       `json:"identity_checks_complete"`
	Complete    bool       `json:"complete_at_observation"`
	Known       int        `json:"known_sessions"`
	Indexed     int        `json:"indexed_sessions"`
	Pending     int        `json:"pending_sessions"`
	Failed      int        `json:"failed_sessions"`
	Unsupported int        `json:"unsupported_sessions"`
	Limitations []string   `json:"limitations"`
}

type contentSearchRefresh struct {
	Session   string `json:"session_id,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Scope     string `json:"scope"`
	Rebuild   bool   `json:"rebuild"`
	Passes    int    `json:"passes"`
	Elapsed   int64  `json:"elapsed_ms"`
	Completed bool   `json:"completed"`
	Stopped   string `json:"stopped_reason"`
	Loaded    int    `json:"loaded_sources"`
	Reused    int    `json:"reused_sources"`
	Bytes     int    `json:"compressed_bytes"`
	Committed int    `json:"committed"`
	Metadata  int    `json:"discovery_metadata_reads"`
	Nodes     int    `json:"discovery_nodes"`
}

type contentSearchSlice struct {
	Reference string `json:"reference"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Next      *int   `json:"next_offset"`
	Complete  bool   `json:"complete"`
}

type contentSearchContext struct {
	Selected contentSearchSlice   `json:"selected"`
	Before   []contentSearchSlice `json:"before"`
	After    []contentSearchSlice `json:"after"`
}

func contentSearchCoverageOf(o search.Observation, present bool) *contentSearchCoverage {
	var reconciled, observed *time.Time
	if !o.LastReconciled.IsZero() {
		reconciled = &o.LastReconciled
	}
	if !o.LastObserved.IsZero() {
		observed = &o.LastObserved
	}
	limitations := append([]string{}, o.Limitations...)
	if !present {
		limitations = append(limitations, "index_missing")
	}
	c := &contentSearchCoverage{Scope: "namespace", Present: present, Reconciled: reconciled, Observed: observed, Discovery: o.DiscoveryComplete, Checks: o.IdentityChecksComplete, Complete: o.Complete, Known: o.Known, Indexed: o.Indexed, Pending: o.Pending, Failed: o.Failed, Unsupported: o.Unsupported, Limitations: limitations}

	return c
}

func encodeSearchReference(ref search.Reference) string {
	b, _ := json.Marshal(ref)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSearchReference(token string) (search.Reference, error) {
	var ref search.Reference
	if len(token) > 16384 {
		return ref, errors.New("search reference is too large")
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return ref, errors.New("invalid search reference")
	}
	if err = json.Unmarshal(b, &ref); err != nil || ref.Start < 0 || ref.End <= ref.Start || ref.End-ref.Start > 8192 || ref.Record < 0 || ref.Source.SessionID == "" || ref.Source.Reference.Key == "" {
		return ref, errors.New("invalid search reference")
	}
	parts := strings.Split(ref.Source.Reference.Key, "/")
	hash := ref.Source.Reference.SHA256
	decoded, e := hex.DecodeString(hash)
	if len(parts) != 4 || parts[0] != "sessions" || e != nil || len(decoded) != 32 || hash != strings.ToLower(hash) || ref.Source.Reference.CompressedBytes <= 0 || ref.Source.Destination == "" || ref.Source.ParserVersion == "" || ref.Source.FilterVersion == "" || ref.Source.ExtractorVersion == "" || ref.Source.SchemaVersion <= 0 {
		return ref, errors.New("invalid search reference")
	}
	key, e := archive.MetadataObjectKey(parts[1], ref.Source.SessionID)
	if e != nil || strings.Join(parts[:3], "/")+"/metadata.json" != key || parts[3] != "source."+hash+".jsonl.gz" {
		return ref, errors.New("invalid search reference")
	}
	return ref, nil
}

func setSearchPage(doc *contentSearchDocument, page search.Page) {
	doc.Generation = page.Generation
	doc.Coverage = contentSearchCoverageOf(page.Observation, true)
	doc.Results = []contentSearchHit{}
	for _, hit := range page.Hits {
		var reference *string
		if hit.Reference != nil {
			token := encodeSearchReference(*hit.Reference)
			reference = &token
		}
		gaps := []contentSearchGap{}
		for _, g := range hit.Gaps {
			gaps = append(gaps, contentSearchGap{g.RevisionID, g.Reason})
		}
		v := contentSearchHit{Session: hit.SessionID, Harness: hit.Harness, Title: hit.Title, Snippet: hit.Snippet, Role: hit.Role, Captured: hit.CapturedAt, Matches: hit.MatchCount, Reference: reference, Gaps: gaps}
		doc.Results = append(doc.Results, v)
	}
	doc.Returned = len(doc.Results)
	if page.Next != "" {
		doc.NextCursor = &page.Next
		doc.Truncated = true
	}
}

func searchFailure(code string) error { return &searchCLIError{code} }

type searchCLIError struct{ code string }

func (e *searchCLIError) Error() string { return e.code }

func setSearchError(doc *contentSearchDocument, err error) {
	if err == nil {
		return
	}
	code := "index_unavailable"
	var classified *searchCLIError
	switch {
	case errors.As(err, &classified):
		code = classified.code
	case errors.Is(err, context.Canceled):
		code = "interrupted"
	case errors.Is(err, context.DeadlineExceeded):
		code = "query_timeout"
		if doc.Kind == contentSearchContextRead {
			code = "context_timeout"
		} else if doc.Kind == contentSearchRefreshOnly || doc.Refresh != nil {
			code = "refresh_timeout"
		}
	case errors.Is(err, search.ErrInvalidCursor):
		code = "invalid_cursor"
	case errors.Is(err, search.ErrCursorChanged):
		code = "cursor_changed"
	case errors.Is(err, search.ErrUpgradeRequired):
		code = "index_upgrade_required"
	case errors.Is(err, search.ErrChanged):
		code = "source_changed"
	case errors.Is(err, search.ErrDeleted):
		code = "source_deleted"
	case errors.Is(err, search.ErrUnsupported):
		code = "source_unsupported"
	case errors.Is(err, search.ErrUnavailable):
		code = "source_unavailable"
	case errors.Is(err, search.ErrFailedWork):
		code = "refresh_failed"
	case errors.Is(err, search.ErrWorkBusy):
		code = "work_busy"
	}
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	doc.OK = false
	doc.Error = &contentSearchError{code, message, code == "cursor_changed"}
}

func writeContentSearch(doc contentSearchDocument, opts contentSearchOptions, stdout, stderr io.Writer) int {
	var data []byte
	var err error
	if opts.json {
		data, err = jsonDocument(doc)
	} else {
		var out bytes.Buffer
		if doc.Context != nil {
			data, err = jsonDocument(doc.Context.Selected.Text)
			if opts.before > 0 || opts.after > 0 {
				data, err = jsonDocument(doc.Context)
			}
		} else {
			if doc.Refresh != nil {
				r := doc.Refresh
				terminalSearchSummary(&out, r, doc.Coverage)
			}
			if doc.Kind == contentSearchResults {
				for _, h := range doc.Results {
					fmt.Fprintf(&out, "%s · %s · %s\n  %s\n  %d matching passage(s)\n", archive.DisplayLine(h.Session), archive.DisplayLine(h.Harness), archive.DisplayLine(h.Title), archive.DisplayLine(h.Snippet), h.Matches)
					if h.Reference != nil {
						fmt.Fprintf(&out, "  ref: %s\n", *h.Reference)
					}
					for _, gap := range h.Gaps {
						fmt.Fprintf(&out, "  coverage gap %s: %s\n", archive.DisplayLine(gap.Revision), archive.DisplayLine(gap.Reason))
					}
				}
				if doc.Coverage != nil && !doc.Coverage.Present {
					out.WriteString("Search coverage is incomplete: no local content index. Run search --refresh.\n")
				} else if doc.Coverage != nil && doc.Coverage.Complete {
					fmt.Fprintf(&out, "%d result(s). Coverage complete at last observation; remote changes are unknown.\n", len(doc.Results))
				} else {
					fmt.Fprintf(&out, "%d result(s). Search coverage is incomplete at last observation.\n", len(doc.Results))
				}
				if doc.Coverage != nil {
					printContentSearchCoverage(&out, doc.Coverage)
				}
				if doc.NextCursor != nil {
					fmt.Fprintf(&out, "Next cursor: %s\n", *doc.NextCursor)
				}
			}

			data = out.Bytes()
		}
	}
	if err == nil && len(data) > opts.outputBytes {
		setSearchError(&doc, searchFailure("output_too_large"))
		doc.Error.Message = "escaped output exceeds --max-output-bytes; reduce --limit or increase output budget"
		doc.Results = []contentSearchHit{}
		doc.Context = nil
		doc.Returned = 0
		doc.NextCursor = nil
		doc.Truncated = false
		if opts.json {
			data, err = jsonDocument(doc)
		} else {
			data = nil
		}
	}
	if err == nil && len(data) > opts.outputBytes {
		doc = contentSearchDocument{SchemaVersion: 1, Kind: doc.Kind, OK: false, Error: &contentSearchError{Code: "output_too_large", Message: "escaped output exceeds --max-output-bytes; reduce --limit or increase output budget"}, Results: []contentSearchHit{}}
		if opts.json {
			data, err = jsonDocument(doc)
		} else {
			data = nil
		}
	}
	if err != nil {
		return searchCommandError(stderr, err)
	}
	if err = writeOutput(stdout, data); err != nil {
		return searchCommandError(stderr, err)
	}
	if doc.Error != nil {
		searchCommandError(stderr, errors.New(doc.Error.Message))
		return 1
	}
	return 0
}

func printContentSearchCoverage(out *bytes.Buffer, c *contentSearchCoverage) {
	fmt.Fprintf(out, "Index: %d indexed / %d locally known; %d pending, %d failed, %d unsupported.\n", c.Indexed, c.Known, c.Pending, c.Failed, c.Unsupported)
	if c.Observed != nil {
		fmt.Fprintf(out, "Last observed: %s. Remote changes after this time are unknown.\n", c.Observed.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintln(out, "Archive freshness unknown; no observation recorded.")
	}
	if c.Reconciled != nil {
		fmt.Fprintf(out, "Last reconciliation: %s.\n", c.Reconciled.UTC().Format(time.RFC3339))
	}
}
