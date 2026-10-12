package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/purge"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

type guidedTranscriptFlow string

const (
	guidedFlowPairingSource          guidedTranscriptFlow = "pairing-source"
	guidedFlowPairingReceiver        guidedTranscriptFlow = "pairing-receiver"
	guidedFlowHandoffFile            guidedTranscriptFlow = "handoff-file"
	guidedFlowBackfillEditImport     guidedTranscriptFlow = "backfill-edit-import"
	guidedFlowBackfillUndo           guidedTranscriptFlow = "backfill-undo"
	guidedFlowRecoveryPreviewConfirm guidedTranscriptFlow = "recovery-preview-confirm"
	guidedFlowUninstallLocalPurge    guidedTranscriptFlow = "uninstall-local-purge"
	guidedFlowPurgePlanApply         guidedTranscriptFlow = "purge-plan-apply"
)

// Full command transcripts complement the decision matrix: every transcript
// includes command planning, prompts and committed result or cancellation.
// Sequential KDF work bounds pairing test memory.
func TestGuidedFullCommandTranscripts(t *testing.T) {
	payload := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: screenNow, ExpiresAt: screenNow.Add(time.Hour), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(payload, code)
	must(t, err)
	for _, flow := range []guidedTranscriptFlow{guidedFlowPairingSource, guidedFlowPairingReceiver, guidedFlowHandoffFile, guidedFlowBackfillEditImport, guidedFlowBackfillUndo, guidedFlowRecoveryPreviewConfirm, guidedFlowUninstallLocalPurge, guidedFlowPurgePlanApply} {
		for _, size := range []struct {
			width  int
			height int
			ascii  bool
		}{{80, 24, false}, {100, 30, false}, {60, 20, false}, {36, 20, true}} {
			for _, color := range []bool{false, true} {
				if testing.Short() && color != (size.width == 100 || size.ascii) {
					// Keep every flow and size, with plain and colored Unicode
					// plus the narrow ASCII screen. Full mode keeps both colors
					// at every size, including their presentation interactions.
					continue
				}
				name := fmt.Sprintf("%s-%dx%d", flow, size.width, size.height)
				if color {
					name += ".color"
				}
				t.Run(name, func(t *testing.T) {
					out := &guidedCommandOutput{caps: promptCapabilities{Color: color, InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Redraw: !size.ascii, ASCII: size.ascii, Width: size.width, Height: size.height}}
					normalize := func(s string) string { return s }
					run := func(args []string, answers string, env Env) {
						in := &guidedCommandAnswers{input: answers, out: out}
						env.IsTerminal = func(any) bool { return true }
						terminalCommand := fmt.Sprintf("$ agent-archive %s\n", strings.Join(args, " "))
						out.WriteString(terminalCommand)
						exit := Run(args, in, out, out, env)
						fmt.Fprintf(&out.Buffer, "[exit %d]\n", exit)
						if exit != 0 {
							t.Fatalf("command failed:\n%s", out.String())
						}
					}
					switch flow {
					case guidedFlowPairingSource:
						env, home, _ := pairingSourceFixtureAt(t, t.TempDir(), pairingDisplayFixtureHome(t))
						env.Now = func() time.Time { return screenNow }
						env.Clipboard = func([]byte) error { return nil }
						env.Interrupts = noInterrupts
						run([]string{"machines", "add", "--name", "laptop"}, "\n\n\n\ndone\n", env)
						userHome, _ := env.UserHomeDir()
						normalize = func(s string) string {
							s = normalizeGuidedPairingCode(s)
							s = strings.ReplaceAll(s, userHome, "/Users/alex")
							return strings.ReplaceAll(s, home, "/Users/alex/.agent-archive")
						}
					case guidedFlowPairingReceiver:
						f := newScreenFixture(t)
						f.withApps(t, "codex")
						f.env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
						project := f.project(t, "src/项目-long-project")
						run([]string{"setup", "--pair", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}, bundle+"\n"+code+"\nsave\n", f.env)
						normalize = f.normalize
						if strings.Contains(out.String(), bundle) || strings.Contains(out.String(), code) {
							t.Fatal("pairing input leaked")
						}
						beforeCheck, afterCheck, checked := strings.Cut(out.String(), "Checking your storage connection")
						if !checked || !strings.Contains(beforeCheck, "Storage not checked yet") || strings.Contains(beforeCheck, "Storage connected") {
							t.Fatal("pairing review claimed an unchecked connection")
						}
						if !strings.Contains(afterCheck, "Connected to your storage") || !strings.Contains(afterCheck, "Setup complete") {
							t.Fatal("pairing did not check storage before completed capture")
						}
					case guidedFlowHandoffFile:
						home, project, path := handoffDisplayFixturePaths(t)
						f := newHandoffFixtureForAt(t, false, "codex", handoffTranscript, home, project)
						run([]string{"handoff", f.id}, "w\n"+path+"\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.id, "SESSION")
							s = strings.ReplaceAll(s, path, "/Users/alex/handoff.md")
							s = strings.ReplaceAll(s, f.project, "/Users/alex/src/项目-long-project")
							return strings.ReplaceAll(s, f.home, "/Users/alex/.agent-archive")
						}
						if _, err := os.Stat(path); err != nil {
							t.Fatal(err)
						}
					case guidedFlowBackfillEditImport:
						f, _ := newImportFixture(t)
						run([]string{"backfill"}, "edit\n120\nyes\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.userHome, "/Users/alex")
							return strings.ReplaceAll(s, f.root, "")
						}
					case guidedFlowBackfillUndo:
						f, _ := newUndoFixture(t)
						run([]string{"backfill", "undo"}, "yes\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, f.userHome, "/Users/alex")
							return strings.ReplaceAll(s, f.root, "")
						}
					case guidedFlowRecoveryPreviewConfirm:
						env, home, path, id := recoverFixture(t)
						run([]string{"recover", id}, "", env)
						run([]string{"recover", id, "--confirm"}, "", env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(s, id, "PREVIOUS_ID")
							s = strings.ReplaceAll(s, home, "/Users/alex/.agent-archive")
							return strings.ReplaceAll(s, path, "/Users/alex/src/project/session.jsonl")
						}
					case guidedFlowUninstallLocalPurge:
						f := newScreenFixture(t)
						f.installed(t)
						run([]string{"uninstall", "--delete-local-data"}, "yes\nyes\n", f.env)
						normalize = f.normalize
					case guidedFlowPurgePlanApply:
						f := newScreenFixture(t)
						f.installed(t)
						// Synthetic orphan content is never an archived developer transcript.
						must(t, f.bucket.Put(context.Background(), "sessions/codex/"+strings.Repeat("a", 32)+"/source."+strings.Repeat("b", 64)+".jsonl.gz", []byte("synthetic orphan")))
						run([]string{"purge", "plan"}, "", f.env)
						var path string
						for line := range strings.SplitSeq(out.String(), "\n") {
							if value, ok := strings.CutPrefix(line, "Plan: "); ok {
								path = value
							}
						}
						var plan purge.Plan
						must(t, jsonFile(path, &plan))
						_, err := config.SetPaused(f.home, true)
						must(t, err)
						run([]string{"purge", "apply", path}, plan.Digest[:12]+"\n", f.env)
						normalize = func(s string) string {
							s = strings.ReplaceAll(f.normalize(s), plan.Digest, "HASH")
							return strings.ReplaceAll(s, plan.Digest[:12], "DIGEST")
						}
					}
					text := normalize(out.String())
					text = normalizeGuidedHandoffTranscript(text)
					text = normalizeGuidedActivityTranscript(text)
					// Random issued IDs and content hashes are presentation placeholders.
					text = regexp.MustCompile(`[0-9a-f]{64}`).ReplaceAllString(text, "HASH")
					text = regexp.MustCompile(`[0-9a-f]{32}`).ReplaceAllString(text, "MACHINE_ID")
					text = strings.ReplaceAll(text, "\x1b", `\e`)
					golden.Check(t, filepath.Join("testdata", "guided-full", name+".txt"), []byte(trimScreenLineEnds(text)))
				})
			}
		}
	}
}

// A redirected structured result remains a JSON document despite human guided
// presentation changes elsewhere. Provider checks and secrets are not invoked.
func TestGuidedMachinesJSONHasNoPromptDecoration(t *testing.T) {
	t.Parallel()
	env, _, _ := pairingSourceFixture(t)
	out := &guidedCommandOutput{}
	var errOut bytes.Buffer
	if Run([]string{"machines", "--json"}, nil, out, &errOut, env) != 0 {
		t.Fatalf("JSON failed: %s", out.String())
	}
	var document any
	must(t, json.Unmarshal(out.Bytes(), &document))
	if errOut.Len() != 0 {
		t.Fatalf("JSON stderr: %s", &errOut)
	}
	if !strings.HasPrefix(out.String(), "{") || strings.Contains(out.String(), "? ") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("decorated JSON %q", out.String())
	}
}

func TestGuidedEmptyMachinesListing(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"wide", "narrow", "json"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			env, _, _ := pairingSourceFixture(t)
			width := 80
			if mode == "narrow" {
				width = 36
			}
			out := &guidedCommandOutput{caps: promptCapabilities{Width: width}}
			var errOut bytes.Buffer
			args := []string{"machines"}
			if mode == "json" {
				args = append(args, "--json")
			}
			if exit := Run(args, nil, out, &errOut, env); exit != 0 || errOut.Len() != 0 {
				t.Fatalf("empty listing failed: exit %d, stderr %s", exit, &errOut)
			}
			if mode == "json" {
				var document map[string]json.RawMessage
				must(t, json.Unmarshal(out.Bytes(), &document))
				var records []json.RawMessage
				must(t, json.Unmarshal(document["records"], &records))
				if len(records) != 0 || strings.Contains(out.String(), "No records found.") {
					t.Fatalf("empty JSON listing changed: %s", out.String())
				}
				return
			}
			if strings.Count(out.String(), "No records found.") != 1 || !strings.Contains(out.String(), "Bucket records are untrusted claims") {
				t.Fatalf("empty listing lacks explicit state or trust caveat: %s", out.String())
			}
		})
	}
}

// Actual handoff flows retain static, readable output when cursor ownership is
// unavailable. Stdout and stderr are captured independently in the split case.
func TestGuidedFullHandoffOutputSurfaces(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"dumb", "redirected", "stderr-split"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newHandoffFixture(t, false)
			out := &guidedCommandOutput{caps: promptCapabilities{InputTerminal: mode != "redirected", OutputTerminal: mode != "redirected", SharedTerminal: mode == "dumb", ASCII: mode == "dumb", Width: 80, Height: 24}}
			var errOut bytes.Buffer
			f.env.IsTerminal = func(any) bool { return mode != "redirected" }
			path := filepath.Join(t.TempDir(), "handoff.md")
			args := []string{"handoff", f.id}
			answers := "w\n" + path + "\n"
			if mode == "redirected" {
				args = append(args, "--output", path)
				answers = ""
			}
			in := &guidedCommandAnswers{input: answers, out: out}
			out.WriteString("$ agent-archive handoff SESSION\n")
			exit := Run(args, in, out, &errOut, f.env)
			if exit != 0 {
				t.Fatalf("handoff failed: %s\n%s", out.String(), &errOut)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			text := out.String() + "[stderr]\n" + errOut.String() + "[exit 0]\n"
			text = strings.ReplaceAll(text, f.id, "SESSION")
			text = strings.ReplaceAll(text, path, "/Users/alex/handoff.md")
			text = strings.ReplaceAll(text, f.project, "/Users/alex/src/project")
			text = strings.ReplaceAll(text, f.home, "/Users/alex/.agent-archive")
			text = normalizeGuidedHandoffTranscript(text)
			if strings.Contains(text, "\x1b") {
				t.Fatalf("static surface emitted cursor controls: %q", text)
			}
			golden.Check(t, filepath.Join("testdata", "guided-full", "handoff-"+mode+".txt"), []byte(trimScreenLineEnds(text)))
		})
	}
}

func normalizeGuidedHandoffTranscript(text string) string {
	text = regexp.MustCompile(`handoff-[0-9a-f]{8}\.md`).ReplaceAllString(text, "handoff-SESSION.md")
	return regexp.MustCompile(`(handoff: wrote .*?) \([0-9]+ bytes\)`).ReplaceAllString(text, "$1 (BYTES bytes)")
}

// Animation frames and upload counters are transient terminal rows. Their
// timing varies with the host; retain the command's completed result lines and
// the exact guided prompt cursor operations instead.
func normalizeGuidedActivityTranscript(text string) string {
	text = regexp.MustCompile(`(?:\r(?:\x1b\[[0-9;]*m)?[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏](?:\x1b\[[0-9;]*m)?[^\r\n]*)+\r\x1b\[K`).ReplaceAllString(text, "")
	return regexp.MustCompile(`\rUploading: [0-9]+ of [0-9]+ sessions, [^\r\n]*(?:\r\x1b\[K)?`).ReplaceAllString(text, "")
}

func TestGuidedTranscriptNormalizationPreservesPromptOperations(t *testing.T) {
	t.Parallel()
	kept := "\x1b[4A\r\x1b[2K\x1b[1B\r\x1b[2K✓ Saved\n\rWarning: storage failed\r\x1b[K\nUploaded 12 sessions.\n"
	transient := "\r⠋ Registering sessions…\r⠙ Registering sessions…\r\x1b[K\rUploading: 1 of 12 sessions, 1 KB of 8 KB\r\x1b[K"
	if got := normalizeGuidedActivityTranscript(transient + kept); got != kept {
		t.Fatalf("normalization changed prompt operations or result: %q", got)
	}
}

func TestPairingReviewEditorOffersCurrentProjectWithoutSelectingIt(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.installed(t)
	current := f.project(t, "src/current-project")
	f.env.WorkingDir = func() (string, error) { return current, nil }
	f.env.BackfillTempDirs = []string{}
	cfg := mustLoadConfig(t, f.home)
	before, err := json.Marshal(cfg.Archive.Projects)
	must(t, err)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("edit\nprojects\n"), &out)
	defer p.close()
	reviewed, err := reviewPairingSettings(p, pairing.Payload{}, cfg, cfg, true, f.userHome, setupOptions{}, f.env)
	if err == nil || !strings.Contains(out.String(), "~/src/current-project · this folder") {
		t.Fatalf("current project missing from pairing editor: %v\n%s", err, &out)
	}
	after, err := json.Marshal(reviewed.Archive.Projects)
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("opening the pairing project editor changed capture rules without selection")
	}
}

// Paired consent must expose the exact roots hidden by the compact review,
// then return to the same captured facts without consuming the next answer.
func TestPairingReviewDetailsKeepsCapturedFactsAndBufferedConsent(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "codex", "claude")
	cfg := config.Config{Harnesses: []string{"codex", "claude"}, RetentionDays: 90}
	for _, name := range []string{"one", "two", "项目-three"} {
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: f.project(t, "src/"+name), Included: true})
	}
	out := &guidedCommandOutput{caps: promptCapabilities{Width: 36, Height: 20}}
	p := newPrompter(strings.NewReader("d\nsave\nnext-answer\n"), out)
	defer p.close()
	var details string
	f.env.IsTerminal = func(any) bool { return true }
	f.env.RunPager = func(_ context.Context, _ string, _ []string, input io.Reader, _, _ io.Writer) error {
		data, err := io.ReadAll(input)
		details = string(data)
		// A post-pager clock change must not rebuild consent observations.
		p.now = func() time.Time { t.Fatal("Details refreshed the captured review"); return time.Time{} }
		return err
	}
	before, err := json.Marshal(cfg)
	must(t, err)
	reviewed, err := reviewPairingSettings(p, pairing.Payload{}, cfg, config.Config{}, false, f.userHome, setupOptions{}, f.env)
	must(t, err)
	after, err := json.Marshal(reviewed)
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("Details changed paired capture settings")
	}
	for _, fact := range []string{"Full settings and privacy", "  Apps\n    Codex", "~/src/one", "~/src/two", "~/src/项目-three", "Codex: ~/.codex", "Claude Code: ~/.claude", "Storage not checked yet", "docs/security/privacy.md"} {
		if !strings.Contains(details, fact) {
			t.Fatalf("paired narrow Details omitted %q:\n%s", fact, details)
		}
	}
	if strings.Contains(details, "Storage connected") || strings.Count(strings.Join(strings.Fields(out.String()), " "), "3 included projects (paths in Details)") != 2 {
		t.Fatalf("Details did not return to the same unchecked compact review:\n%s\n%s", details, out.String())
	}
	next, err := p.in.ReadString('\n')
	must(t, err)
	if next != "next-answer\n" || p.renderer().suspended != 0 || p.renderer().delegated != 0 {
		t.Fatal("pager lost buffered input or retained terminal ownership")
	}
}

// Replace only a complete displayed six-word code, retaining surrounding instructions.
func normalizeGuidedPairingCode(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if len(strings.Fields(line)) == 6 {
			if _, err := pairing.NormalizeCode(line); err == nil {
				lines[i] = "PAIRING_CODE"
			}
		}
	}
	return strings.Join(lines, "\n")
}

func TestGuidedPairingCodeNormalizationPreservesInstructionsAndControls(t *testing.T) {
	t.Parallel()
	code := "aardvark abandoned abbreviate abdomen abhorrence abiding"
	kept := "\x1b[?1049h\x1b[2J\x1b[H2. Enter the pairing code on the other machine\n\n" + code + "\n\nKeep this screen open while entering the code there.\nPress Enter to hide.\n\x1b[?1049l\n? Pairing\n\n[Enter] Close transfer instructions\n"
	want := strings.Replace(kept, "\n"+code+"\n", "\nPAIRING_CODE\n", 1)
	if got := normalizeGuidedPairingCode(kept); got != want {
		t.Fatalf("normalization changed prompt structure: %q", got)
	}
}
