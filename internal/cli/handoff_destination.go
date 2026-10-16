package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// handoffAction is what the destination prompt was answered with.
type handoffAction int

const (
	handoffLaunch handoffAction = iota
	handoffPrint
	handoffCopy
	handoffWrite
	handoffQuit
)

// handoffConfirmation names the yes/no aliases of a single-agent prompt.
type handoffConfirmation string

const (
	handoffConfirmYesKey  handoffConfirmation = "y"
	handoffConfirmYesWord handoffConfirmation = "yes"
	handoffConfirmNoKey   handoffConfirmation = "n"
	handoffConfirmNoWord  handoffConfirmation = "no"
	handoffConfirmQuitKey handoffConfirmation = "q"
)

// handoffChoice is an answer to the destination prompt: an agent to launch,
// or what to do with the handoff instead.
type handoffChoice struct {
	action handoffAction
	// dest is the agent to launch, for handoffLaunch.
	dest handoffDestination
}

// handoffLetters are the prompt's choices other than an agent, in the order
// it lists them. Each is answered by its key or its word.
var handoffLetters = []struct {
	key    string
	word   string
	label  string
	action handoffAction
}{
	{"p", "print", "print", handoffPrint},
	{"c", "copy", "copy to the clipboard", handoffCopy},
	{"w", "write", "write to a file", handoffWrite},
	{"q", "quit", "quit", handoffQuit},
}

// handoffDefaultDestination is where a session goes by default when the
// configuration names nothing: to another agent than the one it came from.
var handoffDefaultDestination = map[string]handoffDestination{
	archive.HarnessClaude: handoffDestinationCodex,
	archive.HarnessCodex:  handoffDestinationClaude,
	archive.HarnessCursor: handoffDestinationClaude,
}

// offersDestinations reports whether the command asks where the handoff
// goes: on a terminal, when nothing already says. A pipe, --output, and
// JSON print as they always have, so `codex "$(agent-archive handoff
// --latest)"` still works. So does --no-preamble: a launched agent needs the
// preamble (--to refuses it), so the only use left for it is printing.
func offersDestinations(opts handoffOptions, interactive bool) bool {
	return interactive && opts.to == "" && opts.output == "" && opts.format == "markdown" && !opts.noPreamble
}

// installedDestinations returns the agents whose CLI is on PATH, in
// catalog presentation order.
func installedDestinations(env launchSpecDependencies) []handoffDestination {
	var installed []handoffDestination
	for _, binding := range env.launcherLookup().Launchers() {
		dest := handoffDestination(binding.Agent)
		for _, name := range binding.Launcher.Executables().Names {
			if _, err := env.lookPath(name); err == nil {
				installed = append(installed, dest)
				break
			}
		}
	}
	return installed
}

// defaultDestination is the installed agent Enter chooses for a session
// from source: the configured one, else another agent than source, else
// the first installed. It is empty when none is installed.
func defaultDestination(installed []handoffDestination, source string, cfg config.HandoffConfig) handoffDestination {
	for _, dest := range []handoffDestination{handoffDestination(cfg.DefaultTo[source]), handoffDefaultDestination[source]} {
		if dest != "" && slices.Contains(installed, dest) {
			return dest
		}
	}
	if len(installed) == 0 {
		return ""
	}
	return installed[0]
}

// chooseDestination asks where the handoff goes: one of the installed
// agents, numbered with def first, or print, copy, write to a file, or quit.
// With one agent installed it asks yes or no to that agent instead, and no
// quits. Enter takes def, or print when no agent is installed. Input ending
// is quitting, as in the session picker. header, when given, names the
// session above the question.
func chooseDestination(p *prompter, installed []handoffDestination, def handoffDestination, canCopy bool, c agentmeta.Catalog, header []string) (handoffChoice, error) {
	agents := installed
	if i := slices.Index(installed, def); i > 0 {
		agents = slices.Concat([]handoffDestination{def}, installed[:i], installed[i+1:])
	}
	confirm := len(agents) == 1
	primary := make([]option, 0, len(agents))
	for _, dest := range agents {
		label := string(dest)
		if d, ok := c.Lookup(string(dest)); ok {
			label = d.DisplayName
		}
		primary = append(primary, option{string(dest), label})
	}
	secondary := []actionOption{}
	for _, l := range handoffLetters {
		if l.action == handoffCopy && !canCopy || l.action == handoffQuit && confirm {
			continue
		}
		// Beside an agent the actions share one line, so each is its word.
		label := l.label
		if len(agents) > 0 {
			label = l.word
		}
		secondary = append(secondary, actionOption{l.word, l.key, label})
	}
	defaultKey := string(def)
	if len(agents) == 0 {
		defaultKey = "print"
	}
	aliases := []option{}
	for _, dest := range agents {
		if d, ok := c.Lookup(string(dest)); ok {
			for _, alias := range d.Aliases {
				aliases = append(aliases, option{alias, string(dest)})
			}
		}
	}
	question := "Continue in"
	if confirm {
		question = "Continue in " + primary[0].Label + "?"
		aliases = append(aliases, option{"y", ""}, option{"yes", ""}, option{"n", ""}, option{"no", ""}, option{"q", ""}, option{"quit", ""})
	}
	// resolve turns an answer into an agent or a handoffLetters word.
	resolve := func(key string) string {
		if confirm {
			switch handoffConfirmation(key) {
			case handoffConfirmYesKey, handoffConfirmYesWord:
				return string(agents[0])
			case handoffConfirmNoKey, handoffConfirmNoWord, handoffConfirmQuitKey:
				return "quit"
			}
		}
		return agentmeta.Canonical(c, key)
	}
	answer, err := p.guidedChoice(promptModel{Context: header, Question: question, Primary: primary, Secondary: secondary, Default: defaultKey, Aliases: aliases, Confirm: confirm, InlineSecondary: len(agents) > 0, Receipt: "Handoff", ResolveReceipt: func(key string) string {
		key = resolve(key)
		if d, ok := c.Lookup(key); ok {
			return d.DisplayName
		}
		for _, l := range handoffLetters {
			if l.word == key {
				return l.label
			}
		}
		return key
	}})
	answer = resolve(answer)
	if errors.Is(err, io.EOF) {
		return handoffChoice{action: handoffQuit}, nil
	}
	if err != nil {
		return handoffChoice{}, err
	}
	if slices.Contains(agents, handoffDestination(answer)) {
		return handoffChoice{action: handoffLaunch, dest: handoffDestination(answer)}, nil
	}
	for _, l := range handoffLetters {
		if answer == l.word {
			return handoffChoice{action: l.action}, nil
		}
	}
	return handoffChoice{action: handoffQuit}, nil
}

// handoffPromptHeader names the session a handoff continues above the
// destination prompt: its title, then its agent, when it was last active,
// and the handoff's size.
func handoffPromptHeader(target handoffTarget, h archive.Handoff, size int, now time.Time, c agentmeta.Catalog) []string {
	var header []string
	var title string
	if target.metadata != nil {
		title = archive.DisplayTitle(*target.metadata)
	}
	if strings.TrimSpace(title) == "" {
		labels, _ := archive.LabelsFromAnalysis(target.analysis)
		title = archive.DisplayTitle(archive.Metadata{Name: labels.Name, Title: labels.Title})
	}
	if title = listTitle(archive.DisplayLine(title)); title != "" {
		header = append(header, title)
	}
	harness := archive.CanonicalHarness(h.Session.Harness)
	facts := []string{}
	if d, ok := c.Lookup(harness); ok {
		facts = append(facts, d.DisplayName)
	} else if harness != "" {
		facts = append(facts, archive.DisplayLine(harness))
	}
	if t := h.Session.LastActivityAt; t != nil && !t.IsZero() {
		facts = append(facts, longWhen(now, *t))
	}
	facts = append(facts, fmt.Sprintf("%d KB", (size+1023)/1024))
	return append(header, strings.Join(facts, " · "))
}

// askHandoffDestination offers the agents installed here, defaulting by the
// source session's harness and config.json's handoff.default_to.
func askHandoffDestination(p *prompter, h archive.Handoff, header []string, home string, env handoffDestinationDependencies) (handoffChoice, error) {
	// `--file` works before setup, when there is no configuration.
	cfg, _, err := config.Load(home)
	if err != nil {
		return handoffChoice{}, fmt.Errorf("load config: %w", err)
	}
	return askHandoffDestinationConfig(p, h, header, cfg, env)
}

func askHandoffDestinationConfig(p *prompter, h archive.Handoff, header []string, cfg config.Config, env handoffDestinationDependencies) (handoffChoice, error) {
	installed := installedDestinations(env)
	return chooseDestination(p, installed, defaultDestination(installed, archive.CanonicalHarness(h.Session.Harness), cfg.Handoff), env.clipboardAvailable(), catalogFor(env), header)
}

// deliverHandoff carries out a choice other than launching: print it (paged
// when longer than the screen, as show is), copy it, or write it to a file.
func deliverHandoff(choice handoffChoice, p *prompter, rendered []byte, target handoffTarget, opts handoffOptions, stdout, stderr io.Writer, env handoffDestinationDependencies) error {
	switch choice.action {
	case handoffPrint:
		release := p.suspendPrompts(true)
		defer release()
		_, _, err := pageText(context.Background(), stdout, stderr, env, false, false, rendered)
		return err
	case handoffCopy:
		if err := env.clipboard(rendered); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		terminal.Printf(stderr, "handoff: copied %d bytes\n", len(rendered))
		return nil
	case handoffWrite:
		// The default file goes where an agent would have started.
		dir, err := launchDir(opts, env)
		if err != nil {
			return err
		}
		return writeHandoffChoice(p, rendered, target, dir, stderr, env)
	case handoffLaunch, handoffQuit:
		// Launching is the caller's; quitting does nothing.
	}
	return nil
}

// writeHandoffChoice asks for a path and writes the handoff there with mode
// 0600, replacing an existing file only when told to. The default is in dir.
// A leading ~/ is the home directory, as in a shell; ~user is left as
// typed. q cancels (./q is a file named q). A write that fails is reported
// and asked again, with no default: Enter then cancels too.
func writeHandoffChoice(p *prompter, rendered []byte, target handoffTarget, dir string, stderr io.Writer, env handoffDestinationDependencies) error {
	def := filepath.Join(dir, "handoff-"+shortSessionID(handoffFileName(target.bundle))+".md")
	label := "Write to"
	for {
		path, err := p.guidedText(promptModel{Question: label, Default: def, ResolveReceipt: func(value string) string {
			if value == "q" || value == "" {
				return "Handoff file cancelled"
			}
			return "File " + value
		}})
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if path == "q" || (def == "" && path == "") {
			return nil
		}
		if path == "~" || strings.HasPrefix(path, "~/") {
			home, err := env.userHomeDir()
			if err != nil {
				return fmt.Errorf("home directory: %w", err)
			}
			path = filepath.Join(home, path[1:])
		}
		force := false
		info, statErr := os.Lstat(path)
		if statErr == nil && info.IsDir() {
			// Replacing it could only fail after asking.
			terminal.Printf(stderr, "agent-archive: handoff: %s is a directory\n", path)
			label, def = "Write to (Enter to cancel)", ""
			continue
		}
		if statErr == nil {
			replace, err := p.guidedYesNo(path + " already exists. Replace it?")
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if !replace {
				continue
			}
			force = true
		}
		if err := writeHandoffOutput(path, rendered, force); err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			label, def = "Write to (Enter to cancel)", ""
			continue
		}
		terminal.Printf(stderr, "handoff: wrote %s (%d bytes)\n", path, len(rendered))
		return nil
	}
}

func (e Env) openTerminal(spec termlaunch.Spec) (string, error) {
	if e.OpenTerminal != nil {
		return e.OpenTerminal(spec)
	}
	return termlaunch.Open(context.Background(), spec, termlaunch.DefaultEnvironment())
}
