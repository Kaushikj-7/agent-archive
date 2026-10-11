package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/pairing"
)

// Exercise the default interactive transfer with two distinct homes: the file
// and code displayed by the source must be enough for the receiver to decrypt.
func TestPairingGuidedFileTransferCanBeReceived(t *testing.T) {
	source, _, _ := pairingSourceFixture(t)
	source.IsTerminal = func(any) bool { return true }
	source.Interrupts = noInterrupts
	userHome, err := source.userHomeDir()
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(userHome, "Downloads"), 0700))
	source.Clipboard = func([]byte) error { t.Fatal("default transfer used clipboard"); return nil }
	var out bytes.Buffer
	if code := Run([]string{"machines", "add", "--name", "laptop"}, strings.NewReader("\n\n\n\ndone\n"), &out, &out, source); code != 0 {
		t.Fatalf("source: %d %s", code, &out)
	}
	for _, text := range []string{"Pairing · Transfer prepared", "File delivered locally", "Code available", "Close transfer instructions", "Receiver completion is unconfirmed"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing source progress %q: %s", text, &out)
		}
	}
	if strings.Contains(out.String(), "Paired successfully") {
		t.Fatal("source claimed receiver success")
	}
	path := filepath.Join(userHome, "Downloads", "agent-archive-pairing-laptop.txt")
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("pairing file is not private")
	}
	data, err := os.ReadFile(path)
	must(t, err)
	if strings.Contains(out.String(), string(bytes.TrimSpace(data))) {
		t.Fatal("file transfer also printed the encrypted bundle")
	}
	var code string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if len(strings.Fields(line)) == 6 {
			if normalized, err := pairing.NormalizeCode(line); err == nil {
				code = normalized
			}
		}
	}
	start, end := strings.Index(out.String(), "\x1b[?1049h"), strings.Index(out.String(), "\x1b[?1049l")
	if code == "" || start < 0 || end < start {
		t.Fatal("code was not shown automatically on a temporary screen")
	}
	receiverHome := t.TempDir()
	receiver := Env{UserHomeDir: func() (string, error) { return receiverHome, nil }}
	must(t, os.MkdirAll(filepath.Join(receiverHome, "Downloads"), 0700))
	must(t, os.WriteFile(filepath.Join(receiverHome, "Downloads", filepath.Base(path)), data, 0600))
	var receiverOut bytes.Buffer
	p := newPrompter(strings.NewReader("~/Downloads/"+filepath.Base(path)+"\n"), &receiverOut)
	bundle, err := readPairingBundle(p, setupOptions{}, p.in, receiver)
	must(t, err)
	payload, err := pairing.Open(bundle, code, source.now())
	must(t, err)
	if payload.Name != "laptop" || payload.IssuerName != "studio" {
		t.Fatal("receiver opened the wrong pairing")
	}
}

func TestPairingShowsCodeBeforeRefillAndCancellationDoesNotRefill(t *testing.T) {
	for _, choice := range []string{"done", "cancel"} {
		t.Run(choice, func(t *testing.T) {
			source, _, cf, _ := dedicatedFixture(t)
			source.LookupEnv = func(key string) (string, bool) {
				return bootstrapCanary, key == "CLOUDFLARE_API_TOKEN"
			}
			source.IsTerminal = func(any) bool { return true }
			source.Interrupts = func() (<-chan os.Signal, func()) {
				if len(cf.Live()) != 1 {
					t.Fatal("future keys were prepared before showing the code")
				}
				return nil, func() {}
			}
			var out bytes.Buffer
			path := filepath.Join(t.TempDir(), "pairing.txt")
			if status := Run([]string{"machines", "add", "--name", "laptop", "--file", path}, strings.NewReader("\n\n"+choice+"\n"), &out, &out, source); status != 0 {
				t.Fatalf("%s: %d %s", choice, status, &out)
			}
			if choice == "cancel" {
				if len(cf.Live()) != 0 || strings.Contains(out.String(), "Preparing access for future machines") {
					t.Fatal("cancellation kept live access or prepared spare keys")
				}
			} else if len(cf.Live()) != 3 || strings.Index(out.String(), "Preparing access for future machines") < strings.Index(out.String(), "\x1b[?1049l") {
				t.Fatal("future access was not prepared after delivery finished")
			}
		})
	}
}

// Run the printed command against a harmless argv recorder to check shell
// quoting and HOME expansion, without invoking the real CLI or cloud storage.
func TestPairingTransferCommandUsesReceivingHomeAndQuotesFilename(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pairing.txt", "pairing file.txt", "pair ' $(printf BAD) ; &.txt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bin, receiverHome := t.TempDir(), t.TempDir()
			must(t, os.WriteFile(filepath.Join(bin, "agent-archive"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700))
			var out bytes.Buffer
			now := time.Now()
			printPairingTransfer(&out, filepath.Join(t.TempDir(), name), false, pairingLedger{Name: "laptop", ExpiresAt: now.Add(15 * time.Minute)}, now)
			var command string
			for line := range strings.SplitSeq(out.String(), "\n") {
				if strings.HasPrefix(line, "  agent-archive setup") {
					command = strings.TrimSpace(line)
				}
			}
			if command == "" {
				t.Fatal("no receiver command provided")
			}
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
			cmd.Env = []string{"HOME=" + receiverHome, "PATH=" + bin}
			actual, err := cmd.CombinedOutput()
			must(t, err)
			want := "setup\n--pair-file\n" + filepath.Join(receiverHome, "Downloads", name) + "\n"
			if string(actual) != want {
				t.Fatalf("command argv: %q, want %q", actual, want)
			}
		})
	}
}

func TestPairingReceiverDistinguishesRelativeFilenamesFromPastedBundles(t *testing.T) {
	// Sequential because t.Chdir controls relative file resolution.
	home := t.TempDir()
	t.Chdir(home)
	env := Env{UserHomeDir: func() (string, error) { return home, nil }}
	bundle := pairing.Prefix + "SYNTHETIC"
	for _, name := range []string{"aa-pairing.txt", "aa-pair-backup.txt", "aa-pair1:backup.txt"} {
		must(t, os.WriteFile(filepath.Join(home, name), []byte(bundle+"\n"), 0600))
	}
	for _, input := range []string{"aa-pairing.txt", "aa-pair-backup.txt", "aa-pair1:backup.txt", bundle} {
		t.Run(input, func(t *testing.T) {
			p := newPrompter(strings.NewReader(input+"\n"), &bytes.Buffer{})
			got, err := readPairingBundle(p, setupOptions{}, p.in, env)
			must(t, err)
			if got != bundle {
				t.Fatalf("read %q, want file contents or pasted bundle %q", got, bundle)
			}
		})
	}
}

func TestPairingReceiverMalformedPastedBundleKeepsValidationError(t *testing.T) {
	// Sequential because t.Chdir isolates the pasted text from existing files.
	t.Chdir(t.TempDir())
	env := Env{
		LookupEnv:  noEnv,
		IsTerminal: func(any) bool { return true },
		UserHomeDir: func() (string, error) {
			t.Fatal("malformed pasted text was treated as a path")
			return "", nil
		},
		Home: func() (string, error) { t.Fatal("read archive home before validation"); return "", nil },
		PairingCode: func() (string, error) {
			t.Fatal("requested code before validation")
			return "", nil
		},
	}
	var out bytes.Buffer
	err := setupPairing(setupOptions{}, strings.NewReader(pairing.Prefix+"bad\n"), &out, &out, env)
	if err == nil || !strings.Contains(err.Error(), "pairing bundle is truncated or damaged") {
		t.Fatalf("expected damaged-bundle diagnostic, got %v", err)
	}
}

func TestPairingReceiverFileAndClipboardInputsStayBounded(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := Env{UserHomeDir: func() (string, error) { return home, nil }}
	path := filepath.Join(home, "oversized.txt")
	must(t, os.WriteFile(path, []byte(strings.Repeat("x", pairing.MaxBundle+2)), 0600))
	for _, input := range []string{"~/oversized.txt\n", pairing.Prefix + strings.Repeat("x", pairing.MaxBundle+2) + "\n", "\n", "-\n"} {
		p := newPrompter(strings.NewReader(input), &bytes.Buffer{})
		if _, err := readPairingBundle(p, setupOptions{}, p.in, env); err == nil {
			t.Fatal("accepted oversized or missing pairing input")
		}
	}
}

func TestPairingFileCollisionDoesNotOverwriteOrClaimDelivery(t *testing.T) {
	source, home, _ := pairingSourceFixture(t)
	source.IsTerminal = func(any) bool { return true }
	source.Interrupts = noInterrupts
	path := filepath.Join(t.TempDir(), "pairing.txt")
	must(t, os.WriteFile(path, []byte("existing contents"), 0600))
	var out bytes.Buffer
	if code := Run([]string{"machines", "add", "--name", "laptop", "--file", path}, strings.NewReader("\n"), &out, &out, source); code != 1 {
		t.Fatalf("collision: %d %s", code, &out)
	}
	data, err := os.ReadFile(path)
	must(t, err)
	if string(data) != "existing contents" || strings.Contains(out.String(), "Saved to:") || strings.Contains(out.String(), "\x1b[?1049h") {
		t.Fatal("existing file overwritten or failed transfer reported as delivered")
	}
	ledgers, err := readPairingLedgers(home)
	must(t, err)
	if len(ledgers) != 1 || ledgers[0].State != pairingDeliveryIntent {
		t.Fatal("uncertain delivery was forgotten")
	}
}

func TestPairingClipboardTransferClearsOnlyAfterFinishing(t *testing.T) {
	source, _, _ := pairingSourceFixture(t)
	source.IsTerminal = func(any) bool { return true }
	var clipboard []byte
	source.Clipboard = func(data []byte) error {
		clipboard = append([]byte(nil), data...)
		return nil
	}
	source.PairingClipboardRead = func() ([]byte, error) { return clipboard, nil }
	source.Interrupts = func() (<-chan os.Signal, func()) {
		if _, err := pairing.Inspect(string(clipboard)); err != nil {
			t.Fatal("clipboard cleared before code display")
		}
		return nil, func() {}
	}
	var out bytes.Buffer
	if status := Run([]string{"machines", "add", "--name", "laptop"}, strings.NewReader("clipboard\n\n\ndone\n"), &out, &out, source); status != 0 {
		t.Fatalf("clipboard transfer: %d %s", status, &out)
	}
	if len(clipboard) != 0 || !strings.Contains(out.String(), "Copied to this machine's clipboard") || !strings.Contains(out.String(), "agent-archive setup --pair") {
		t.Fatal("clipboard transfer missing instructions or not cleared on exit")
	}
}

func TestPairingFileCollisionCanRecoverToAnotherPath(t *testing.T) {
	source, _, _ := pairingSourceFixture(t)
	source.IsTerminal = func(any) bool { return true }
	source.Interrupts = noInterrupts
	dir := t.TempDir()
	existing, replacement := filepath.Join(dir, "existing.txt"), filepath.Join(dir, "replacement.txt")
	must(t, os.WriteFile(existing, []byte("keep me"), 0600))
	var out bytes.Buffer
	if status := Run([]string{"machines", "add", "--name", "laptop", "--file", existing}, strings.NewReader(replacement+"\n\n\ndone\n"), &out, &out, source); status != 0 {
		t.Fatalf("retry transfer: %d %s", status, &out)
	}
	data, err := os.ReadFile(existing)
	must(t, err)
	if string(data) != "keep me" {
		t.Fatal("original file overwritten")
	}
	data, err = os.ReadFile(replacement)
	must(t, err)
	_, err = pairing.Inspect(strings.TrimSpace(string(data)))
	must(t, err)
	if !strings.Contains(out.String(), "Saved to: "+replacement) || !strings.Contains(out.String(), "Downloads/replacement.txt") {
		t.Fatal("receiver command did not follow the replacement path")
	}
}

// Pairing text consumes exactly one bounded answer from the foundation buffer;
// the later hidden prompt must still receive typed-ahead code and consent.
func TestPairingTransferKeepsSharedPromptInput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	bundle := pairing.Prefix + "SYNTHETIC"
	p := newPrompter(strings.NewReader(bundle+"\nsynthetic-code\nno\n"), &out)
	defer p.close()
	got, err := readPairingBundle(p, setupOptions{}, p.in, Env{})
	must(t, err)
	code, err := p.guidedText(promptModel{Question: "Pairing code (hidden)", Secret: true, ReadAnswer: func(in *bufio.Reader) (string, error) { return boundedPairingAnswer(in, 512) }})
	must(t, err)
	consent, err := p.guidedYesNo("Replace destination?")
	must(t, err)
	if got != bundle || code != "synthetic-code" || consent || strings.Contains(out.String(), code) {
		t.Fatal("pairing buffer lost input or exposed the private code")
	}
	if !strings.Contains(out.String(), "Credential received") {
		t.Fatal("pairing code did not use a fixed secret receipt")
	}
}

func TestPairingTransferConsentDefaultsToNoAndKeepsBufferedAnswer(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	bundle := pairing.Prefix + "SYNTHETIC"
	p := newPrompter(strings.NewReader(bundle+"\nsynthetic-code\n\nnext-answer\n"), &out)
	defer p.close()
	got, err := readPairingBundle(p, setupOptions{}, p.in, Env{})
	must(t, err)
	code, err := p.guidedText(promptModel{Question: "Pairing code (hidden)", Secret: true, ReadAnswer: func(in *bufio.Reader) (string, error) { return boundedPairingAnswer(in, 512) }})
	must(t, err)
	consent, err := p.guidedYesNo("Replace destination?")
	must(t, err)
	next, err := p.in.ReadString('\n')
	must(t, err)
	if got != bundle || code != "synthetic-code" || consent || next != "next-answer\n" {
		t.Fatal("pairing consent changed its default or consumed the following answer")
	}
	if strings.Contains(out.String(), code) || !strings.Contains(out.String(), "Credential received") || !strings.Contains(out.String(), "? Replace destination?") || !strings.Contains(out.String(), "No (default)") {
		t.Fatal("pairing consent or private-code receipt changed")
	}
}

func TestPairingCodeDisplayRelinquishesPromptRegion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	defer p.close()
	r := p.renderer()
	region := r.begin(promptModel{Question: "Transfer"})
	env := Env{IsTerminal: func(any) bool { return true }, Interrupts: noInterrupts}
	must(t, showPairingCode(p, "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding", env))
	if r.suspended != 0 || r.epoch.Load() == region.epoch {
		t.Fatal("pairing screen retained the preceding prompt region or suspension")
	}
}

func TestPairingStatusGlyphsRespectTerminalCapabilities(t *testing.T) {
	t.Parallel()
	for _, ascii := range []bool{false, true} {
		out := &guidedCommandOutput{caps: promptCapabilities{ASCII: ascii, Width: 80}}
		printPairingAccessReady(out, config.Config{}, issuance.Slot{})
		printPairingAccessReady(out, config.Config{Storage: credentials.Config{Provider: credentials.ProviderR2}}, issuance.Slot{})
		printPairingAccessReady(out, config.Config{}, issuance.Slot{SlotID: "synthetic"})
		printPairingConnected(out, "laptop", pairing.Payload{IssuerName: "studio"}, false)
		mark := "✓ "
		if ascii {
			mark = "OK "
		}
		for _, fact := range []string{"Archive settings ready.", "Shared access ready. Revoking it affects every machine using this key.", "Separate access ready. You can revoke this machine independently.", "Paired successfully · laptop is connected. Paired with studio."} {
			if !strings.Contains(out.String(), mark+fact) {
				t.Fatalf("missing resolved status in ASCII=%t: %q", ascii, out.String())
			}
		}
		if ascii && strings.Contains(out.String(), "✓") {
			t.Fatalf("dumb status emitted Unicode: %q", out.String())
		}
	}
}

// The bounded guided adapter must retain EOF and raw line boundaries until
// guidedRead distinguishes a submitted blank from exhausted private input.
func TestPairingBundleWhitespaceEOFDoesNotCompleteSecretPrompt(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", " ", " \t"} {
		t.Run(fmt.Sprintf("input-%q", input), func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(input), &out)
			defer p.close()
			_, err := readPairingBundle(p, setupOptions{}, p.source, Env{})
			if !errors.Is(err, io.EOF) || strings.Contains(out.String(), "Credential skipped") || strings.Contains(out.String(), "Credential received") {
				t.Fatalf("exhausted private input completed a prompt: %v\n%s", err, &out)
			}
		})
	}
}
