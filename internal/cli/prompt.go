package cli

import (
	"bufio"
	"errors"
	"fmt"
	"golang.org/x/term"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// prompter handles terminal and redirected input without echoing secrets.
type prompter struct {
	reviewModel *setupReviewModel

	tokenHelpers      []string
	projectScan       *setupProjectSearch
	projectConfig     config.Config
	projectCurrent    string
	projectBackfilled map[string]bool

	tokenCommand []string
	in           *bufio.Reader
	out          io.Writer
	source       io.Reader
	// handBack, when set, takes the input that a key-reading browser read
	// but did not use (what was typed ahead of the prompts after it), so
	// that in reads it first.
	handBack func([]byte)
	now      func() time.Time
	style    textStyle
	// guidedSpacing separates legacy guided answers from what follows.
	// Other commands use this prompter too and retain their existing output.
	guidedSpacing bool
	render        *promptRenderer
	lineGuard     *promptLineGuard
	// singleArea is set while setup changes one area of an installed
	// setup, where step headings do not count steps.
	singleArea bool
	// reviewHint, when set, is a line the setup review repeats: where to
	// change what an answer chose for the person.
	reviewHint string
	// createdBuckets are the S3 buckets setup created in this run (see
	// setup_s3_create.go); in memory only.
	createdBuckets []createdS3Bucket
	// resourceRequestsThisRun separates explicit in-process retries from restart recovery.
	resourceRequestsThisRun map[string]bool
	// guided is a guided R2 bucket creation whose key setup has yet to
	// stage; created is every bucket and key such creations left in the
	// person's Cloudflare account in this run.
	guided  *r2Handoff
	created []*r2Created
}

// warn and note print one review item. Continuation lines, such as a link,
// are indented under the item's text.
func (p *prompter) warn(text string, continuation ...string) {
	p.item(p.style.warnMark(), text, continuation)
}

func (p *prompter) note(text string, continuation ...string) {
	p.item(p.style.dim("·"), text, continuation)
}

func (p *prompter) item(mark, text string, continuation []string) {
	terminal.Printf(p.out, "  %s %s\n", mark, text)
	for _, l := range continuation {
		terminal.Println(p.out, "    "+l)
	}
}

// clock returns the prompter's injected clock, or the wall clock when none
// was provided.
func (p *prompter) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: &promptWriter{w: out, trailing: 1}, source: in, style: styleFor(out)}
}

func (p *prompter) line(label string) (string, error) {
	terminal.Print(p.out, label)
	text, err := p.in.ReadString('\n')
	p.noteLineEcho(text)
	if err != nil {
		if errors.Is(err, io.EOF) && text != "" {
			// A final answer with no trailing newline is still a real one.
			if p.guidedSpacing {
				terminal.Println(p.out)
			}
			return strings.TrimSpace(text), nil
		}
		// No more input at all: treated as an error, never as a silent
		// blank. Otherwise a truncated scripted input, or a real Ctrl-D,
		// would make every remaining prompt take its default silently —
		// including "Enable automatic capture?", which defaults to yes —
		// so setup could commit real changes the user never confirmed.
		return "", fmt.Errorf("no more input: %w", err)
	}
	if p.guidedSpacing {
		terminal.Println(p.out)
	}
	return strings.TrimSpace(text), nil
}

func (p *prompter) promptText(question string, bold bool, choices []string, def int, plainEnd string) string {
	if question == "" && len(choices) == 0 {
		return ""
	}
	if !p.style.color {
		text := question
		if len(choices) > 0 {
			text += " [" + strings.Join(choices, "/") + "]"
		}
		return text + plainEnd
	}
	text := question
	if bold {
		text = p.style.bold(question)
	}
	if len(choices) > 0 {
		styled := make([]string, len(choices))
		for i, c := range choices {
			styled[i] = c
			if i == def {
				styled[i] = p.style.bold(c)
			}
		}
		text += " [" + strings.Join(styled, "/") + "]"
	}
	if text == "" {
		return promptCursor + " "
	}
	return text + " " + promptCursor + " "
}

// promptCursor ends every prompt on a color terminal.
const promptCursor = "›"

// option is one numbered entry in a menu. Key is what the caller receives;
// Label is what the user reads.
type option struct {
	Key   string
	Label string
}

// actionOption is a visible, unnumbered action at a storage prompt.
type actionOption struct {
	Key      string
	Shortcut string
	Label    string
}

// matchOption finds the option whose key equals answer, or failing that the
// only option whose key starts with it.
func matchOption(answer string, options []option) (string, bool) {
	answer = strings.ToLower(answer)
	if answer == "" {
		return "", false
	}
	for _, o := range options {
		if answer == o.Key {
			return o.Key, true
		}
	}
	match := ""
	for _, o := range options {
		if strings.HasPrefix(o.Key, answer) {
			if match != "" {
				return "", false
			}
			match = o.Key
		}
	}
	return match, match != ""
}

func (p *prompter) secret(label string) (string, error) {
	if file, ok := p.source.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fd := int(file.Fd())
		state, err := term.GetState(fd)
		if err != nil {
			return "", fmt.Errorf("read terminal state: %w", err)
		}
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
		done := make(chan struct{})
		defer func() { signal.Stop(interrupts); close(done); _ = term.Restore(fd, state) }()
		go func() {
			select {
			case sig := <-interrupts:
				_ = term.Restore(fd, state)
				// Exit only after restoring the caller's terminal. A signal
				// must not be mistaken for consent or resume the wizard.
				os.Exit(128 + int(sig.(syscall.Signal)))
			case <-done:
			}
		}()
		terminal.Print(p.out, p.labelText(label))
		value, err := readSecret(fd)
		terminal.Println(p.out)
		if errors.Is(err, io.EOF) {
			// As for a line: no more input is an error, never a blank.
			return "", fmt.Errorf("no more input: %w", err)
		}
		if err != nil {
			return "", fmt.Errorf("cannot hide credential input: %w", err)
		}
		if p.guidedSpacing {
			terminal.Println(p.out)
		}
		return strings.TrimSpace(string(value)), nil
	}
	// Redirected input is read without reproducing its contents. Callers must
	// supply it via a private stream, never a command argument.
	return p.line(p.labelText(label))
}

// labelText styles a prompt written whole, such as "Projects: " or
// "Import 3 sessions? [y/N/edit] ", with the same cursor: its trailing ": " or " "
// is the plain end, and a bracketed list of choices at its end marks its
// default with a capital letter, as in [y/N].
func (p *prompter) labelText(label string) string {
	if !p.style.color {
		return label
	}
	question := strings.TrimRight(label, " ")
	question = strings.TrimSuffix(question, ":")
	var choices []string
	def := -1
	if strings.HasSuffix(question, "]") {
		if open := strings.LastIndex(question, " ["); open >= 0 {
			choices = strings.Split(question[open+2:len(question)-1], "/")
			question = question[:open]
			for i, c := range choices {
				if c != strings.ToLower(c) {
					def = i
				}
			}
		}
	}
	return p.promptText(question, true, choices, def, "")
}

// typedInput is a command's standard input for the prompts that read answers:
// one buffered reader, so an answer typed (or scripted) ahead for a later
// prompt is not lost to an earlier one's, and the input behind it, which the
// key browser reads a key at a time and hands back what it read but did not
// use.
type typedInput struct {
	answers *bufio.Reader
	file    io.Reader
	back    *handedBack
}

// handedBack is the input beneath a command's answers: stdin, preceded by
// what a browser that read keys handed back.
type handedBack struct {
	r    io.Reader
	text []byte
}

func (h *handedBack) Read(p []byte) (int, error) {
	if len(h.text) > 0 {
		n := copy(p, h.text)
		h.text = h.text[n:]
		return n, nil
	}
	return h.r.Read(p)
}

// give puts text before the input not read yet.
func (h *handedBack) give(text []byte) {
	h.text = append(slices.Clone(text), h.text...)
}

func newTypedInput(stdin io.Reader) *typedInput {
	back := &handedBack{r: stdin}
	return &typedInput{answers: bufio.NewReader(back), file: stdin, back: back}
}

// prompter is a prompter that reads the answers, and whose browser reads keys
// from the input itself and hands back what it did not use.
func (t *typedInput) prompter(out io.Writer) *prompter {
	p := newPrompter(t.answers, out)
	p.source, p.handBack = t.file, t.back.give
	return p
}
