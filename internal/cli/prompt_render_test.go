package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

type promptScreen struct {
	bytes.Buffer
	caps promptCapabilities
}

func (s *promptScreen) promptCapabilities() promptCapabilities { return s.caps }

func promptExample() promptModel {
	return promptModel{Question: "Where should your archive live?", Helpers: []string{"Choose storage for sessions from ~/src/長い-project-path. Credentials stay private."}, Primary: []option{{"r2", "Cloudflare R2"}, {"s3", "Amazon S3"}}, Secondary: []actionOption{{"help", "h", "Setup instructions"}}, Default: "s3", Receipt: "Provider"}
}

func TestGuidedPromptSnapshots(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{80, 24}, {100, 30}, {60, 20}, {36, 20}} {
		for _, color := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-%d-color-%t", size[0], size[1], color), func(t *testing.T) {
				t.Parallel()
				out := &promptScreen{caps: promptCapabilities{Color: color, Width: size[0], Height: size[1]}}
				p := newPrompter(strings.NewReader("\nwork\nsynthetic-secret\n"), out)
				key, err := p.guidedChoice(promptExample())
				must(t, err)
				if key != "s3" {
					t.Fatal(key)
				}
				value, err := p.guidedText(promptModel{Question: "AWS profile", Default: "work", Receipt: "Profile", Label: "Profile"})
				must(t, err)
				if value != "work" {
					t.Fatal(value)
				}
				secret, err := p.guidedText(promptModel{Question: "Secret access key (hidden)", Secret: true, Label: "Credential"})
				must(t, err)
				if secret != "synthetic-secret" || strings.Contains(out.String(), secret) {
					t.Fatal("secret exposed or lost")
				}
				golden.Check(t, filepath.Join("testdata", "prompts", fmt.Sprintf("%dx%d-color-%t.txt", size[0], size[1], color)), []byte(strings.ReplaceAll(trimScreenLineEnds(out.String()), "\x1b", "\\e")))
			})
		}
	}
}

func TestGuidedPromptCollapseRequiresOwnedVisibleRows(t *testing.T) {
	t.Parallel()
	for _, mode := range []promptTestMode{promptModeLive, promptModeNoColor, promptModeResize, promptModeHeight, promptModeOverflow, promptModeExternal, promptModeSuspend, promptModeTypedAhead, promptModeContinued, promptModeRedirect, promptModeDumb, promptModeLongEcho} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			c := promptCapabilities{Color: mode != promptModeNoColor, InputTerminal: mode != promptModeRedirect, OutputTerminal: true, SharedTerminal: true, Redraw: mode != promptModeDumb, ASCII: mode == promptModeDumb, Width: 80, Height: 24}
			out := &promptScreen{caps: c}
			p := newPrompter(strings.NewReader(""), out)
			r := p.renderer()
			region := r.begin(promptExample())
			echo := "2\n"
			switch mode {
			case promptModeResize:
				out.caps.Width = 60
			case promptModeHeight:
				out.caps.Height = 20
			case promptModeOverflow:
				region.rows = 24
			case promptModeExternal:
				terminal.Println(p.out, "EXTERNAL SENTINEL")
			case promptModeSuspend:
				release := p.suspendPrompts()
				terminal.Println(out, "PAGER SENTINEL")
				release()
			case promptModeLongEcho:
				echo = strings.Repeat("a", 2000) + "\n"
			case promptModeEchoOff, promptModeNewlineOnly, promptModeNewlineLong, promptModeEchoOffLong, promptModeEchoOffStatic, promptModeEchoOffEOF, promptModeEchoOffEmpty, promptModeEchoOffAhead, promptModeNormal, promptModeNoColor, promptModeWide, promptModeControlEcho, promptModeComposed, promptModeLong, promptModeDefaultLong, promptModeRetry, promptModeScroll, promptModeTypedAhead, promptModeTypedAheadTwo, promptModePager, promptModeEof, promptModeFinalEof, promptModeSecretEof, promptModeInterrupt, promptModeTerm, promptModeHup, promptModeQuit, promptModePairingOutput, promptModeLive, promptModeContinued, promptModeRedirect, promptModeDumb:
				// These modes retain the original block; only capability/read flags differ.
			}
			r.finish(region, "Provider Amazon S3", echo, false, mode == promptModeTypedAhead, mode == promptModeContinued)
			got := out.String()
			collapsed := strings.Contains(got, "\x1b[2K")
			if want := mode == promptModeLive || mode == promptModeNoColor; collapsed != want {
				t.Fatalf("collapse=%t want %t: %q", collapsed, want, got)
			}
			if mode == promptModeDumb && (!strings.Contains(got, "> Choose [2]: ") || !strings.Contains(got, "OK Provider")) {
				t.Fatal(got)
			}
			if mode == promptModeNoColor && strings.Contains(got, "\x1b[2m") {
				t.Fatal("NO_COLOR added emphasis")
			}
		})
	}
}

func TestGuidedChoicesResolveDefaultsShortcutsAliasesAndStableNumbers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		key   string
		label string
	}{{"\n", "s3", "Amazon S3"}, {"2\n", "r2", "Cloudflare R2"}, {"13\n", "s3", "Amazon S3"}, {"h\n", "help", "Setup instructions"}, {"s3-existing\n", "s3-existing", "Amazon S3"}, {"wrong\n13\n", "s3", "Amazon S3"}} {
		t.Run(strings.TrimSpace(tc.input), func(t *testing.T) {
			t.Parallel()
			out := &promptScreen{}
			p := newPrompter(strings.NewReader(tc.input), out)
			m := promptExample()
			m.Numbers = []int{2, 13}
			m.Aliases = []option{{"s3-existing", ""}}
			m.ResolveReceipt = storageProviderLabel
			got, err := p.guidedChoice(m)
			must(t, err)
			if got != tc.key || !strings.Contains(out.String(), "✓ Provider "+tc.label) {
				t.Fatalf("key %s output %q", got, out.String())
			}
		})
	}
}

func TestGuidedConfirmAnswersYesNoAndEnter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		def     string
		input   string
		key     string
		choices string
	}{
		{"enter takes yes", "go", "\n", "go", "[Y/n]"},
		{"enter takes no", "stop", "\n", "stop", "[y/N]"},
		{"y", "stop", "Y\n", "go", "[y/N]"},
		{"yes", "stop", "yes\n", "go", "[y/N]"},
		{"n", "go", "n\n", "stop", "[Y/n]"},
		{"no", "go", "no\n", "stop", "[Y/n]"},
		{"no default asks again", "", "\ny\n", "go", "[y/n]"},
		{"a shortcut", "go", "h\n", "help", "[Y/n]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := &promptScreen{}
			p := newPrompter(strings.NewReader(tc.input), out)
			got, err := p.guidedChoice(promptModel{Question: "Go?", Helpers: []string{"Going is final."}, Confirm: true, No: "stop", Default: tc.def,
				Primary: []option{{"go", "Go"}}, Secondary: []actionOption{{"help", "h", "help"}, {"stop", "s", "stop"}}})
			must(t, err)
			if got != tc.key {
				t.Fatalf("key %q, want %q\n%s", got, tc.key, out.String())
			}
			for _, want := range []string{"? Go? " + tc.choices + "\n", "  Going is final.\n", "  or [h] help\n"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestGuidedTextValidatesBeforeReceiptAndSharesBufferedInput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("bad\nwork\nsynthetic-secret\nnext\n"), &out)
	value, err := p.guidedText(promptModel{Question: "Profile", Validate: func(s string) error {
		if s != "work" {
			return errors.New("Choose a configured profile.")
		}
		return nil
	}})
	must(t, err)
	if value != "work" || strings.Contains(out.String(), "✓ Profile bad") {
		t.Fatal(out.String())
	}
	secret, err := p.guidedText(promptModel{Question: "Credential", Secret: true})
	must(t, err)
	next, err := p.line("Legacy: ")
	must(t, err)
	if secret != "synthetic-secret" || next != "next" || strings.Contains(out.String(), secret) {
		t.Fatal("reader lost input or exposed secret")
	}
}

func TestGuidedEOFNeverResolvesDefault(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", " \t"} {
		for _, secret := range []bool{false, true} {
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(input), &out)
			_, err := p.guidedText(promptModel{Question: "Confirm", Default: "yes", Secret: secret})
			if !errors.Is(err, io.EOF) || strings.Contains(out.String(), symbolOK) {
				t.Fatalf("input=%q secret=%t error %v output %q", input, secret, err, out.String())
			}
		}
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(input), &out)
		_, err := p.guidedChoice(promptExample())
		if !errors.Is(err, io.EOF) || strings.Contains(out.String(), symbolOK) {
			t.Fatalf("input=%q %v %q", input, err, out.String())
		}
	}
}

func TestGuidedFinalAnswerAtEOFRemainsAnAnswer(t *testing.T) {
	t.Parallel()
	for _, secret := range []bool{false, true} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(" work "), &out)
		value, err := p.guidedText(promptModel{Question: "Profile", Default: "default", Secret: secret})
		must(t, err)
		if value != "work" {
			t.Fatalf("final answer=%q", value)
		}
	}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("r2"), &out)
	key, err := p.guidedChoice(promptExample())
	must(t, err)
	if key != "r2" {
		t.Fatalf("final key=%q", key)
	}
}

func TestGuidedSecretKeepsBoundedReadersAndSanitizesValidation(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"123456789\n", "bad\nvalid\nnext\n"} {
		out := &promptScreen{}
		p := newPrompter(strings.NewReader(input), out)
		read := func(r *bufio.Reader) (string, error) { return boundedPairingAnswer(r, 8) }
		value, err := p.guidedText(promptModel{Question: "Pairing input", Default: "forbidden-secret-default", Receipt: "forbidden-secret-receipt", Secret: true, ReadAnswer: read, Validate: func(value string) error {
			if value == "bad" {
				return fmt.Errorf("never show %s", value)
			}
			return nil
		}})
		if strings.Contains(out.String(), "forbidden-secret") || strings.Contains(out.String(), "never show bad") {
			t.Fatal("secret state exposed")
		}
		if strings.HasPrefix(input, "123") {
			if err == nil || !strings.Contains(err.Error(), "size limit") || strings.Contains(out.String(), symbolOK) {
				t.Fatalf("%s %v", out.String(), err)
			}
		} else {
			must(t, err)
			if value != "valid" {
				t.Fatal(value)
			}
			next, err := p.line("")
			must(t, err)
			if next != "next" {
				t.Fatal("bounded reader lost typed ahead")
			}
		}
	}
}

func TestGuidedChoiceCannotDefaultToAnUnavailableAction(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	_, err := p.guidedChoice(promptModel{Question: "Blocked review", Default: "start", Primary: []option{{"check", "Check again"}}})
	if !errors.Is(err, io.EOF) || strings.Contains(out.String(), "✓") || strings.Contains(out.String(), "[start]") {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestGuidedWrappedCursorCountsEchoOnlyOnce(t *testing.T) {
	t.Parallel()
	out := &promptScreen{caps: promptCapabilities{InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 36, Height: 24}}
	p := newPrompter(strings.NewReader(""), out)
	r := p.renderer()
	region := r.begin(promptModel{Question: "Profile", Label: "Profile", Default: strings.Repeat("d", 60)})
	r.finish(region, "Profile resolved", "\n", false, false, false)
	if want := fmt.Sprintf("\x1b[%dA", region.rows); !strings.Contains(out.String(), want) {
		t.Fatalf("wrapped input label counted twice: %q, want %q", out.String(), want)
	}
}

func TestGuidedTerminalOwnerCanDelegateJobControl(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	p.lineGuard = &promptLineGuard{}
	releaseOuter := p.suspendPrompts(true)
	releaseInner := p.suspendPrompts()
	if !p.lineGuard.delegated.Load() {
		t.Fatal("terminal owner lost job-control authority")
	}
	releaseInner()
	if !p.lineGuard.delegated.Load() {
		t.Fatal("nested release restored prompt authority too early")
	}
	releaseOuter()
	if p.lineGuard.delegated.Load() {
		t.Fatal("prompt authority was not restored")
	}
	p.lineGuard = nil
}

func TestPromptWriterPreservesTerminalStyle(t *testing.T) {
	t.Parallel()
	out := &promptScreen{caps: promptCapabilities{Color: true, Redraw: true, Width: 60, Height: 20}}
	p := newPrompter(strings.NewReader(""), out)
	for _, wrapped := range []io.Writer{p.out, &lockedWriter{w: p.out}, &promptWriter{w: &lockedWriter{w: out}}} {
		got := styleFor(wrapped)
		if want := styleFor(out); got != want {
			t.Fatalf("wrapped terminal style: %+v", got)
		}
		if got := capabilitiesFor(nil, wrapped); got != out.caps {
			t.Fatalf("wrapped capabilities: %+v", got)
		}
	}
}

func TestPromptRowsCountWideGlyphsThatWrapBeforeTheLastCell(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"xx漢xx漢xx漢", "x́x́漢x́x́漢x́x́漢", "xx❤️xx❤️xx❤️", "\x1b[32mxx漢xx漢xx漢\x1b[0m"} {
		if got := displayLines(text, 3); got != 5 {
			t.Fatalf("wide glyph rows for %q: %d, want 5", text, got)
		}
	}
}

func TestPromptAmbiguousEmojiWidthsPreserveHistory(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"❤️", "👩‍💻", "👍🏽", "1\u20e3", "가", "🇺🇸", "☀\ufe0e", "का़", "bad\rhelper", "bad\thelper"} {
		out := &promptScreen{caps: promptCapabilities{InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 36, Height: 20}}
		p := newPrompter(strings.NewReader(""), out)
		r := p.renderer()
		region := r.begin(promptModel{Question: "Profile", Helpers: []string{value}})
		r.finish(region, "Profile work", "work\n", false, false, false)
		region = r.begin(promptModel{Question: "Profile"})
		r.finish(region, "Profile work", value+"\n", false, false, false)
		if strings.Contains(out.String(), "\x1b[2K") {
			t.Fatalf("ambiguous emoji width erased history: %q", out.String())
		}
	}
}

// Choices validate the resolved key, including defaults, before success receipts.
func TestGuidedChoiceValidatesResolvedKeysBeforeReceipt(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"1\n2\n", "start\ncheck\n", "\n2\n"} {
		out := &promptScreen{}
		p := newPrompter(strings.NewReader(input), out)
		var checked []string
		key, err := p.guidedChoice(promptModel{Question: "Start archiving?", Default: "start", Primary: []option{{"start", "Start archiving"}, {"check", "Check again"}}, Validate: func(key string) error {
			checked = append(checked, key)
			if key == "start" {
				return errors.New("Project directory disappeared. Check again.")
			}
			return nil
		}})
		must(t, err)
		if key != "check" || strings.Join(checked, ",") != "start,check" || strings.Contains(out.String(), "✓ Start archiving") || !strings.Contains(out.String(), "Project directory disappeared") || !strings.Contains(out.String(), "✓ Check again") {
			t.Fatalf("validation key=%q checked=%v output=%q", key, checked, out.String())
		}
	}
}

func TestPromptSyntheticCapabilitiesSetWrappingWidth(t *testing.T) {
	t.Parallel()
	out := &promptScreen{caps: promptCapabilities{Width: 36, Height: 20}}
	style := styleFor(out)
	text := style.hang("  ", "Choose storage for the selected projects and keep credentials private.")
	if style.width != 36 || !strings.Contains(text, "\n") {
		t.Fatalf("explicit width lost: style=%+v text=%q", style, text)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if visibleWidth(line) > 36 {
			t.Fatalf("synthetic wrapped line too wide: %q", line)
		}
	}
}

func TestPromptEchoStartsAtTheActualWrappedCursorColumn(t *testing.T) {
	t.Parallel()
	out := &promptScreen{caps: promptCapabilities{InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 3, Height: 100}}
	p := newPrompter(strings.NewReader(""), out)
	r := p.renderer()
	region := r.begin(promptModel{Question: "Profile", Label: "漢"})
	if region.rows != 7 || region.cursorColumns != 1 {
		t.Fatalf("wrapped region rows=%d column=%d, want 7 and 1", region.rows, region.cursorColumns)
	}
	r.finish(region, "Profile resolved", "xx\n", false, false, false)
	// The wide label wraps early and leaves the cursor in column one. Two
	// echoed characters fit that row; counting total label width erases a
	// preceding row instead.
	if want := fmt.Sprintf("\x1b[%dA\r", region.rows); !strings.Contains(out.String(), want) {
		t.Fatalf("wrapped cursor over-erased: %q, want %q", out.String(), want)
	}
}

// Canonical ECHOCTL expands controls to caret notation; terminal modes can also
// give them cursor effects. Their raw rune widths cannot prove ownership.
func TestGuidedControlEchoPreservesHistoryAndSanitizesReceipt(t *testing.T) {
	t.Parallel()
	for _, control := range []string{"\x01", "\b", "\f", "\x7f"} {
		out := &promptScreen{caps: promptCapabilities{InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 80, Height: 24}}
		p := newPrompter(strings.NewReader(""), out)
		r := p.renderer()
		region := r.begin(promptModel{Question: "Profile"})
		r.finish(region, "Profile work"+control, strings.Repeat(control, 170)+"\n", false, false, false)
		if strings.Contains(out.String(), "\x1b[2K") || strings.Contains(out.String(), control) || !strings.Contains(out.String(), "✓ Profile work") {
			t.Fatalf("unsafe control echo or receipt: %q", out.String())
		}
	}
}

func TestGuidedFinalTerminalAnswerStartsReceiptOnNextLine(t *testing.T) {
	t.Parallel()
	out := &promptScreen{caps: promptCapabilities{InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: true, Width: 80, Height: 24}}
	p := newPrompter(strings.NewReader(""), out)
	r := p.renderer()
	region := r.begin(promptModel{Question: "Profile", Label: "Profile"})
	r.finish(region, "Profile work", "work", false, false, false)
	if !strings.Contains(out.String(), "Profile: \n✓ Profile work\n\n") || strings.Contains(out.String(), "\x1b[2K") {
		t.Fatalf("final answer receipt lacks its own static row: %q", out.String())
	}
}
