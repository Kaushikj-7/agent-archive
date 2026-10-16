package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/purge"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

type guidedCommandOutput struct {
	bytes.Buffer
	caps promptCapabilities
}

func TestGuidedRowsEmptyProviderObservations(t *testing.T) {
	t.Parallel()
	for _, width := range []int{0, 36, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Parallel()
			out := &guidedCommandOutput{caps: promptCapabilities{Width: width}}
			guidedRows(out, []string{"Machine ID", "Provider key", "Role", "State", "Binding"}, nil)
			if got := out.String(); got != "No records found.\n" {
				t.Fatalf("empty observations at width %d: %q", width, got)
			}
		})
	}
}

func (o *guidedCommandOutput) promptCapabilities() promptCapabilities { return o.caps }

func (o *guidedCommandOutput) colorTerminal() bool { return o.caps.Color }

// These transcripts exercise the production guided decisions in sequence with
// disposable state. They intentionally make no storage or credential effects.
func TestGuidedCommandScreens(t *testing.T) {
	t.Parallel()
	sizes := []struct {
		name   string
		width  int
		height int
		ascii  bool
	}{{"80x24", 80, 24, false}, {"100x30", 100, 30, false}, {"60x20", 60, 20, false}, {"36-static", 36, 20, true}}
	flows := []struct {
		name    string
		answers string
		run     func(*testing.T, *prompter, *guidedCommandOutput)
	}{
		{"pairing-source", "laptop\ncreate\n\n\ndone\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			_, err := p.guidedDefault("Name for the new machine", "")
			must(t, err)
			_, err = p.guidedChoice(promptModel{Question: "Create separate access for this machine?", Helpers: []string{"Separate access lets you revoke this machine later without affecting other machines."}, Default: "create", Primary: []option{{"create", "Create a Cloudflare key (recommended)"}, {"share", "Share existing access (revoking it affects all machines using it)"}}, Secondary: []actionOption{{"cancel", "q", "Cancel"}}})
			must(t, err)
			code := finishPairingDelivery(p, "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding", pairingLedger{Name: "laptop", ExpiresAt: screenNow.Add(time.Hour)}, t.TempDir(), false, out, out, Env{IsTerminal: func(any) bool { return true }, Interrupts: noInterrupts}, &issuance.Slot{}, nil)
			if code != 0 {
				t.Fatalf("pairing delivery exit %d", code)
			}
		}},
		{"pairing-receiver", "aa-pair1:synthetic-private-bundle\nsynthetic-private-code\nyes\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			_, err := readPairingBundle(p, setupOptions{}, p.source, Env{})
			must(t, err)
			_, err = readPairingCode(setupOptions{}, p, Env{LookupEnv: noEnv, IsTerminal: func(any) bool { return true }})
			must(t, err)
			payload := pairing.Payload{IssuerName: "studio", Name: "laptop", Storage: pairing.Storage{Provider: "s3", Bucket: "team-archive", Prefix: "agent-archive/", AWSProfile: "work", Region: "us-east-1"}}
			_, err = reviewPairingDestination(p, payload, config.Config{Storage: credentials.Config{Provider: "s3", Bucket: "prior-archive"}}, true, setupOptions{}, Env{AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{Name: "work", Region: "us-east-1"}}, nil }})
			must(t, err)
		}},
		{"handoff", "w\nq\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			choice, err := chooseDestination(p, []handoffDestination{handoffDestinationClaude, handoffDestinationCodex}, handoffDestinationCodex, true, productionAgents.Catalog(), nil)
			must(t, err)
			if choice.action != handoffWrite {
				t.Fatal("wrong handoff action")
			}
			must(t, writeHandoffChoice(p, []byte("synthetic handoff"), handoffTarget{}, "/Users/alex/src/项目-long-directory", out, Env{}))
		}},
		{"backfill-edit", "edit\n120\nyes\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			plan := backfill.Plan{Home: "/Users/alex", RetentionDays: 90, GeneratedAt: screenNow, Candidates: []backfill.Candidate{{Harness: "codex", ProjectRoot: "/Users/alex/src/项目-long-directory", StartedAt: screenNow}}}
			backfill.RenderText(out, plan)
			yes, err := confirmImport(p, out, &plan, 90)
			must(t, err)
			if !yes || plan.RetentionDays != 120 {
				t.Fatal("import edit lost plan")
			}
		}},
		{"backfill-undo", "\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			plan := backfill.UndoPlan{Batch: backfill.Batch{ID: "synthetic-import"}, Sessions: []backfill.UndoSession{{}}}
			backfill.RenderUndo(out, plan)
			yes, err := p.guidedYesNo(backfill.UndoQuestion(plan))
			must(t, err)
			if yes {
				t.Fatal("undo default accepted")
			}
			terminal.Println(out, "Cancelled. Nothing was changed.")
		}},
		{"uninstall-local-purge", "yes\n\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			_, confirmed, err := confirmUninstall(true, false, t.TempDir(), config.Config{}, false, p.in, out)
			must(t, err)
			if confirmed {
				t.Fatal("local purge default accepted")
			}
		}},
		{"purge-digest", "0123456789ab\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			code, confirmed := confirmPurge(purge.Plan{Bucket: "team-archive", Prefix: "agent-archive/", Digest: "0123456789abcdef", Candidates: []purge.Candidate{{}}}, p.in, out, out)
			if code != 0 || !confirmed {
				t.Fatal("exact digest rejected")
			}
		}},
		{"key-management", "\n\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			if err := confirmOwnKey(p, false, true); err == nil {
				t.Fatal("own key default accepted")
			}
			if err := confirmRevocation(p, false, true); err == nil {
				t.Fatal("revocation default accepted")
			}
		}},
		{"management-token", "synthetic-private-token\n", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			token, _, _, err := readManagementToken(t.Context(), p, Env{LookupEnv: noEnv}, nil, true)
			must(t, err)
			if token != "synthetic-private-token" {
				t.Fatal("token changed")
			}
		}},
		{"recovery", "", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			guidedExplanation(out, "Transcript recovery", "Session synthetic-session cannot prove that the current transcript extends its retained history.")
			terminal.Println(out)
			guidedExplanation(out, "Consequences", "Archived history, feedback and handoffs stay under the earlier ID. Each generation expires independently under normal retention.", "The new generation is queued locally for normal sync. Recovery permanently requires a generation-aware writer; older binaries will refuse this data directory.")
			terminal.Println(out)
			terminal.Println(out, "To start this generation, run agent-archive recover synthetic-session --confirm.")
		}},
		{"machines", "", func(t *testing.T, p *prompter, out *guidedCommandOutput) {
			t.Helper()
			terminal.Println(out, "Machines · bucket claims")
			rows := [][]string{}
			for i := range 14 {
				rows = append(rows, []string{fmt.Sprintf("machine-%02d", i), strings.Repeat("a", 32), "darwin/arm64", "Dedicated key claim (unverified)", "2026-10-06"})
			}
			guidedRows(out, []string{"Name", "Machine ID", "Platform", "Credential claim", "Heartbeat"}, rows)
			terminal.Println(out, "Not checked against the provider. Bucket records are untrusted claims, not proof of ownership or access removal.")
		}},
	}
	for _, flow := range flows {
		for _, size := range sizes {
			for _, color := range []bool{false, true} {
				name := flow.name + "-" + size.name
				if color {
					name += ".color"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					out := &guidedCommandOutput{caps: promptCapabilities{Color: color, InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: !size.ascii, ASCII: size.ascii, Width: size.width, Height: size.height}}
					p := newPrompter(&guidedCommandAnswers{input: flow.answers, out: out}, out)
					defer p.close()
					flow.run(t, p, out)
					text := out.String()
					// Paths created by uninstall are disposable, never developer installation.
					text = normalizeGuidedTempPaths(text)
					if strings.Contains(text, "synthetic-private") {
						t.Fatal("private input leaked into transcript")
					}
					text = strings.ReplaceAll(text, "\x1b", `\e`)
					golden.Check(t, filepath.Join("testdata", "guided-commands", name+".txt"), []byte(trimScreenLineEnds(text)))
				})
			}
		}
	}
}

func normalizeGuidedTempPaths(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "Also delete owned local state and credentials under ") {
			text = strings.ReplaceAll(text, line, "Also delete owned local state and credentials under /Users/alex/.local/share/agent-archive.")
		}
	}
	return text
}

func TestGuidedDestructiveDefaultsAndEOF(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"\n", "no\n", "n\n", ""} {
		t.Run(fmt.Sprintf("answer-%q", answer), func(t *testing.T) {
			t.Parallel()
			p := newPrompter(strings.NewReader(answer), io.Discard)
			defer p.close()
			yes, err := p.guidedYesNo("Delete local data?")
			if yes || answer == "" && err == nil {
				t.Fatal("unconfirmed destructive consent")
			}
		})
	}
}

func TestPairingBundleBoundAndSecretReceipts(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(strings.Repeat("x", pairing.MaxBundle+3)+"\n"), &out)
	defer p.close()
	if _, err := readPairingBundle(p, setupOptions{}, p.source, Env{}); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	if strings.Contains(out.String(), strings.Repeat("x", 20)) {
		t.Fatal("bundle retained")
	}
}

func TestGuidedPurgeRequiresExactDigest(t *testing.T) {
	t.Parallel()
	plan := purge.Plan{Digest: strings.Repeat("a", 64)}
	for _, answer := range []string{"", "\n", "yes\n", "aaaaaaaaaaa\n", "aaaaaaaaaaaaextra\n", "aaaaaaaaaaaa\n", "aaaaaaaaaaaa"} {
		t.Run(fmt.Sprintf("answer-%q", answer), func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code, confirmed := confirmPurge(plan, strings.NewReader(answer), &out, &errOut)
			want := strings.TrimSpace(answer) == plan.Digest[:12]
			if confirmed != want || (code == 0) != want {
				t.Fatalf("confirmation = %v, exit %d; want %v: %s", confirmed, code, want, &errOut)
			}
		})
	}
}

// A browser/pager hands answers back to the existing reader; creating a second
// reader between destination and file input would strand the typed-ahead q.
func TestHandoffGuidedTypedAheadKeepsFileAnswer(t *testing.T) {
	t.Parallel()
	in := bufio.NewReader(strings.NewReader("w\nq\n"))
	var out bytes.Buffer
	p := newPrompter(in, &out)
	defer p.close()
	choice, err := chooseDestination(p, nil, "", false, productionAgents.Catalog(), nil)
	must(t, err)
	if choice.action != handoffWrite {
		t.Fatal("write not selected")
	}
	must(t, writeHandoffChoice(p, []byte("private synthetic handoff"), handoffTarget{}, t.TempDir(), &out, Env{}))
	if !strings.Contains(out.String(), "Write to") {
		t.Fatal("file question missing")
	}
}

// guidedCommandAnswers supplies one echoed line at a time like a canonical terminal.
// Hidden fields suppress echo before any input is consumed.
type guidedCommandAnswers struct {
	input  string
	out    io.Writer
	hidden bool
}

func (a *guidedCommandAnswers) Read(dst []byte) (int, error) {
	if a.input == "" {
		return 0, io.EOF
	}
	end := strings.IndexByte(a.input, '\n') + 1
	if end == 0 {
		end = len(a.input)
	}
	end = min(end, len(dst))
	text := a.input[:end]
	a.input = a.input[end:]
	copy(dst, text)
	if !a.hidden {
		terminal.Print(a.out, text)
	}
	return len(text), nil
}

func (a *guidedCommandAnswers) hidePromptEcho() func() {
	old := a.hidden
	a.hidden = true
	return func() { a.hidden = old }
}
