package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statshtml"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// statsHTMLFlags are the flags that turn `stats` into a web page: --html, and
// where the page goes. They follow handoff's --output and --force: the file
// is written with mode 0600 and an existing file is never replaced without
// --force. Like handoff, the page is written to a temporary file in the same
// folder and moved into place, so the file at the path is always either what
// was there or the whole page, and a symbolic link at the path is refused
// rather than followed.
type statsHTMLFlags struct {
	html         bool
	output       string
	force        bool
	includeNames bool
}

func addStatsHTMLFlags(fs *commandFlags) *statsHTMLFlags {
	f := &statsHTMLFlags{}
	fs.BoolVar(&f.html, "html", false, "write one self-contained HTML page (inline styles and SVG, no script, no external requests) to stdout, or to --output")
	fs.StringVar(&f.output, "output", "", "with --html, write the page to this file (mode 0600, replaced in one step) instead of stdout")
	fs.BoolVar(&f.force, "force", false, "with --output, replace FILE if it is an ordinary file that exists")
	fs.BoolVar(&f.includeNames, "include-names", false, "with --html, name the real projects, skills, MCP servers and models the built-in price table does not list; by default the page says project A, skill A, MCP server A, model A, ... so it can be shared")
	return f
}

// validate reports a usage error for flags that do not fit together, before
// any storage is touched: --html is not --json, its options need it, the page
// is not dumped onto a terminal, and a file that would be replaced needs
// --force.
func (f *statsHTMLFlags) validate(fs *commandFlags, jsonOut, stdoutIsTerminal bool) int {
	// --output "" (an unset shell variable, say) must not fall back to
	// standard output without a word.
	outputGiven := false
	fs.Visit(func(fl *flag.Flag) { outputGiven = outputGiven || fl.Name == "output" })
	switch {
	case outputGiven && f.output == "":
		return fs.usageError("--output needs a file name")
	case f.html && jsonOut:
		return fs.usageError("--html and --json cannot be combined; choose one")
	case !f.html && f.output != "":
		return fs.usageError("--output applies only to --html")
	case !f.html && f.includeNames:
		return fs.usageError("--include-names applies only to --html")
	case f.force && f.output == "":
		return fs.usageError("--force applies only to --output")
	case f.html && f.output == "" && stdoutIsTerminal:
		return fs.usageError("--html writes a web page: give --output FILE (for example --output stats.html), or redirect standard output")
	}
	if f.output == "" {
		return 0
	}
	if err := checkStatsHTMLTarget(f.output, f.force); err != nil {
		return fs.usageError("--output: %v", err)
	}
	return 0
}

// checkStatsHTMLTarget fails early, before the archive is read, when the page
// could not be written: the file exists and may not be replaced, is not an
// ordinary file (a folder, a symbolic link, a device or a pipe are never
// written), or its directory is missing.
func checkStatsHTMLTarget(path string, force bool) error {
	if strings.HasSuffix(path, string(filepath.Separator)) {
		return fmt.Errorf("%s ends with a slash, so it names a folder; give a file name", path)
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.IsDir():
		return fmt.Errorf("%s is a directory", path)
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link; name the file it points to, or remove it", path)
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("%s is not an ordinary file", path)
	case err == nil && !force:
		return fmt.Errorf("%s already exists; pass --force to replace it", path)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("the folder for %s does not exist or cannot be read: %w", path, err)
	}
	if !dir.IsDir() {
		return fmt.Errorf("%s is not a folder", filepath.Dir(path))
	}
	return nil
}

// toStdout is whether the page goes to standard output, which then carries
// nothing but the page.
func (f *statsHTMLFlags) toStdout() bool { return f.html && f.output == "" }

// write renders the page and delivers it: to stdout, or to the --output file
// (with a note on stderr, as handoff does). The page is rendered before
// anything is created, so a failure leaves no file behind.
func (f *statsHTMLFlags) write(stdout, stderr io.Writer, computed stats.Stats, filters statsFilters, now time.Time, emptyMessage string) int {
	page, err := renderStatsHTML(computed, filters, now, f.includeNames, emptyMessage)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	if f.output == "" {
		// A page cut short by a full disk or a closed pipe must not look
		// like a whole one.
		if err := writeOutput(stdout, page); err != nil {
			terminal.Printf(stderr, "agent-archive: stats: writing the page: %v\n", err)
			return 1
		}
		return 0
	}
	if err := writeStatsHTMLFile(f.output, page, f.force); err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	terminal.Printf(stderr, "stats: wrote %s (%d bytes); open it in a browser\n", f.output, len(page))
	return 0
}

// renderStatsHTML is the web page for computed: --html's, and the one the
// interactive screen's h saves.
func renderStatsHTML(computed stats.Stats, filters statsFilters, now time.Time, includeNames bool, emptyMessage string) ([]byte, error) {
	return statshtml.Render(computed, statshtml.Options{
		GeneratedAt:  now,
		IncludeNames: includeNames,
		Filters:      statshtml.Filters{Harness: filters.Harness, Model: filters.Model, Origin: filters.Origin, Replays: filters.Replays},
		EmptyMessage: emptyMessage,
	})
}

// writeStatsHTMLFile writes the page to path with mode 0600, whatever the
// umask. The page goes to a temporary file in the same folder first, and is
// then moved into place in one step, so an error, a full disk or a crash
// leaves the file at path as it was (or absent), never half a page. Without
// force, the move fails if anything is at path (a hard link, which never
// replaces one); with it, the file is replaced. A run killed between creating
// the temporary file and moving it (the write and one sync, so a moment) leaves
// a hidden file named .agent-archive-stats-*.tmp behind, at mode 0600.
func writeStatsHTMLFile(path string, page []byte, force bool) error {
	return writeStatsHTMLFileWithLink(path, page, force, os.Link)
}

// writeStatsHTMLFileWithLink permits testing a failed atomic create without
// relying on a particular filesystem or changing process-wide dependencies.
func writeStatsHTMLFileWithLink(path string, page []byte, force bool, link func(string, string) error) error {
	return writeAtomicOutput(path, page, force, outputFileOperations{
		createTemp: func(dir, pattern string) (atomicOutputFile, error) { return os.CreateTemp(dir, pattern) },
		link:       link, rename: os.Rename,
	})
}

// atomicOutputFile is the private temporary output, before publication.
type atomicOutputFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// outputFileOperations permits failure injection without process-wide hooks.
type outputFileOperations struct {
	createTemp func(string, string) (atomicOutputFile, error)
	link       func(string, string) error
	rename     func(string, string) error
}

func writeAtomicOutput(path string, page []byte, force bool, ops outputFileOperations) error {
	dir, _ := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := ops.createTemp(dir, ".agent-archive-stats-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := writeOutput(tmp, page); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The last look before the move: something may have appeared at path
	// since the flags were checked.
	if info, err := os.Lstat(path); err == nil {
		switch {
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not an ordinary file", path)
		case !force:
			return fmt.Errorf("%s already exists; pass --force to replace it", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if force {
		return ops.rename(tmpPath, path)
	}
	// os.Link fails if path exists, which makes "create only if absent" one
	// step. A replacing rename cannot safely substitute for a failed link:
	// another writer may have created path since the existence check.
	if err := ops.link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; pass --force to replace it", path)
		}
		return fmt.Errorf("create %s without replacing it: %w", path, err)
	}
	return nil
}
