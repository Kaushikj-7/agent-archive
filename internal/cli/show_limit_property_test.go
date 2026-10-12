package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// The property tests below run `show --transcript`'s trimming on random
// sessions (fixed seed, so a failure repeats) and check what the limit
// promises whatever the shape: output no larger than the limit whenever the
// limit leaves room for what cannot be dropped, valid UTF-8, valid JSON,
// the same output every time, and no panic or loop for any limit.

var randomPieces = []string{"word ", "é", "世界", "🙂", "\n", "x", "  ", "line of output\n", "ab", "\t"}

// randomText is text of about size bytes, of ASCII and multibyte pieces.
func randomText(r *rand.Rand, size int) string {
	var b strings.Builder
	for b.Len() < size {
		b.WriteString(randomPieces[r.Intn(len(randomPieces))])
	}
	return b.String()
}

// randomSize is a length that is often zero or tiny, sometimes long.
func randomSize(r *rand.Rand, long int) int {
	switch r.Intn(10) {
	case 0, 1:
		return 0
	case 2, 3, 4:
		return r.Intn(20)
	case 5, 6, 7:
		return r.Intn(400)
	case 8:
		return r.Intn(5000)
	}
	return r.Intn(long)
}

func randomTranscript(r *rand.Rand) archive.Transcript {
	t := archive.Transcript{ToolResultsUnavailable: r.Intn(4) == 0}
	exchanges := []int{0, 1, 2, 3, 8, 40, 150}[r.Intn(7)]
	huge := -1
	if exchanges > 0 && r.Intn(5) == 0 {
		huge = r.Intn(exchanges) // one exchange of thousands of steps
	}
	for i := range exchanges {
		e := archive.TranscriptExchange{Kind: archive.TranscriptExchangePrompt, Timestamp: fmt.Sprintf("2026-01-02T03:04:%02dZ", i%60)}
		switch {
		case i == 0 && r.Intn(4) == 0:
			e.Kind = archive.TranscriptExchangeLeading
		case r.Intn(8) == 0:
			e.Kind, e.Text = archive.TranscriptExchangeNotification, "Background task completed"
		default:
			e.Text = randomText(r, randomSize(r, 8000))
		}
		steps := r.Intn(12)
		if i == huge {
			steps = 300 + r.Intn(1500)
		}
		for range steps {
			var step archive.TranscriptStep
			switch r.Intn(7) {
			case 0, 1:
				step = archive.TranscriptStep{Kind: archive.TranscriptStepText, Text: randomText(r, randomSize(r, 6000))}
			case 2, 3:
				name, summary, isError := []string{"Bash", "Read", "exec_command", "Edit"}[r.Intn(4)], randomText(r, r.Intn(80)), r.Intn(9) == 0
				var result string
				if r.Intn(2) == 0 {
					result = randomText(r, randomSize(r, 2000))
				}
				tool := &archive.HandoffToolCall{Name: name, Summary: summary, IsError: isError, Result: result}
				step = archive.TranscriptStep{Kind: archive.TranscriptStepTool, Tool: tool}
			case 4:
				step = archive.TranscriptStep{Kind: archive.TranscriptStepShell, Text: randomText(r, randomSize(r, 8000)), Output: randomText(r, randomSize(r, 2000))}
			case 5:
				step = archive.TranscriptStep{Kind: archive.TranscriptStepCommand, Text: "/" + randomText(r, r.Intn(30)), Output: randomText(r, r.Intn(300))}
			default:
				if r.Intn(3) == 0 {
					step = archive.TranscriptStep{Kind: archive.TranscriptStepSummary, Text: randomText(r, randomSize(r, 6000))}
				} else {
					step = archive.TranscriptStep{Kind: archive.TranscriptStepOutput, Output: randomText(r, randomSize(r, 2000))}
				}
			}
			e.Steps = append(e.Steps, step)
		}
		t.Exchanges = append(t.Exchanges, e)
	}
	finals := []int{0, 0, 1, 3, 200, 3000}[r.Intn(6)]
	for range finals {
		t.HookFinals = append(t.HookFinals, randomText(r, randomSize(r, 6000)))
	}
	return t
}

// floorTranscript is the least `show --transcript` can print of t: the
// header, the newest exchange with its prompt cut and no steps, the newest
// hook final cut, and a footer of every kind of omission with large counts.
func floorTranscript(t archive.Transcript) archive.Transcript {
	var exchanges []archive.TranscriptExchange
	var finals []string
	if n := len(t.Exchanges); n > 0 {
		last := t.Exchanges[n-1]
		last.Steps = nil
		if last.Kind == archive.TranscriptExchangePrompt {
			last.Text = archive.TruncateUTF8(last.Text, 2000)
			last.TextTruncated = true
		}
		exchanges = []archive.TranscriptExchange{last}
	}
	if n := len(t.HookFinals); n > 0 {
		finals = []string{archive.TruncateUTF8(t.HookFinals[n-1], 2000) + " …(shortened)"}
	}
	out := archive.Transcript{Exchanges: exchanges, HookFinals: finals, ToolResultsUnavailable: t.ToolResultsUnavailable}
	for range 2 {
		for _, kind := range []archive.TranscriptElisionKind{
			archive.TranscriptElisionToolOutput, archive.TranscriptElisionToolCalls, archive.TranscriptElisionAssistantText,
			archive.TranscriptElisionPromptText, archive.TranscriptElisionHookFinals, archive.TranscriptElisionOldestHookFinals,
			archive.TranscriptElisionOldestExchanges, archive.TranscriptElisionOldestSteps,
		} {
			out.Elisions = append(out.Elisions, archive.TranscriptElision{Kind: kind, First: 99999, Last: 99999, Count: 99999})
		}
	}
	return out
}

func randomLimit(r *rand.Rand, untrimmed int) int {
	switch r.Intn(6) {
	case 0:
		return 1 + r.Intn(300)
	case 1:
		return 1 + r.Intn(5000)
	case 2:
		return untrimmed + r.Intn(10) // at or just past the untrimmed size
	case 3:
		return untrimmed*2 + 1
	}
	return 1 + int(math.Exp(r.Float64()*math.Log(float64(untrimmed)*1.5+2)))
}

func TestShowTranscriptTrimmingHoldsTheLimitForRandomSessions(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	bundle := archive.SourceBundle{ArchiveSessionID: view.SessionID}
	r := rand.New(rand.NewSource(20260929))
	styles := []textStyle{{}, {color: true, width: 80}}
	trimmedCases, floorCases := 0, 0
	for i := range 90 {
		transcript := randomTranscript(r)
		opts := transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC, Style: styles[r.Intn(2)]}, Full: r.Intn(2) == 0}
		plain := opts
		plain.Style = textStyle{}
		untrimmed := len(renderTranscriptBytes(view, transcript, opts))
		home := t.TempDir()
		floorOpts := opts
		floorOpts.FullRecord = showFullPath(home, bundle, false, opts.Full)
		floor := len(renderTranscriptBytes(view, floorTranscript(transcript), floorOpts))
		limit := randomLimit(r, untrimmed)
		if r.Intn(2) == 0 {
			// Around the least that can be printed, where trimming works hardest.
			limit = floor + r.Intn(max(untrimmed-floor, 1)*2)
		}
		var stderr bytes.Buffer
		fitted, shown := fitTranscriptToLimit(view, transcript, bundle, opts, limit, home, &stderr)
		printed := renderTranscriptBytes(view, fitted, shown)
		label := fmt.Sprintf("case %d: limit %d, untrimmed %d, full=%v, %d exchanges", i, limit, untrimmed, opts.Full, len(transcript.Exchanges))

		if !utf8.Valid(printed) {
			t.Fatalf("%s: output is not valid UTF-8", label)
		}
		// The same input gives the same output.
		fittedAgain, shownAgain := fitTranscriptToLimit(view, transcript, bundle, opts, limit, home, &bytes.Buffer{})
		if again := renderTranscriptBytes(view, fittedAgain, shownAgain); !bytes.Equal(again, printed) {
			t.Fatalf("%s: a second run printed something else", label)
		}
		if untrimmed <= limit {
			if len(fitted.Elisions) != 0 || !bytes.Equal(printed, renderTranscriptBytes(view, transcript, opts)) || stderr.Len() != 0 {
				t.Fatalf("%s: output that fits was changed", label)
			}
			continue
		}
		if len(fitted.Elisions) == 0 {
			// Nothing but the header, which is over the limit itself.
			if !strings.Contains(stderr.String(), "over the") {
				t.Fatalf("%s: over the limit, nothing to trim, and no warning", label)
			}
			continue
		}
		trimmedCases++
		// A record of exactly what was omitted is saved when anything was.
		saved, err := os.ReadFile(showFullPath(home, bundle, false, opts.Full))
		if err != nil || !bytes.Equal(saved, renderTranscriptBytes(view, transcript, plain)) {
			t.Fatalf("%s: saved record is not the untrimmed plain transcript (%v)", label, err)
		}
		if limit >= floor {
			floorCases++
			if len(printed) > limit || stderr.Len() != 0 {
				t.Fatalf("%s: printed %d bytes (floor %d), stderr=%q", label, len(printed), floor, stderr.String())
			}
			// Nothing about the newest exchange goes unsaid: it is still the
			// newest, and a changed prompt says it was cut.
			if len(transcript.Exchanges) > 0 {
				last := transcript.Exchanges[len(transcript.Exchanges)-1]
				got := fitted.Exchanges[len(fitted.Exchanges)-1]
				if got.Kind != last.Kind || got.Timestamp != last.Timestamp || (got.Text != last.Text && !got.TextTruncated) {
					t.Fatalf("%s: the newest exchange changed without saying so: %+v", label, got.Kind)
				}
			}
			// What the footer counts of dropped exchanges and final responses
			// is what is missing.
			for _, e := range fitted.Elisions {
				if e.Kind == archive.TranscriptElisionOldestExchanges && e.Count != len(transcript.Exchanges)-len(fitted.Exchanges) {
					t.Fatalf("%s: footer says %d exchanges dropped, %d were", label, e.Count, len(transcript.Exchanges)-len(fitted.Exchanges))
				}
				if e.Kind == archive.TranscriptElisionOldestHookFinals && e.Count != len(transcript.HookFinals)-len(fitted.HookFinals) {
					t.Fatalf("%s: footer says %d final responses dropped, %d were", label, e.Count, len(transcript.HookFinals)-len(fitted.HookFinals))
				}
			}
		} else if len(printed) > limit && !strings.Contains(stderr.String(), "over the") {
			t.Fatalf("%s: over the limit without a warning", label)
		}
	}
	if trimmedCases < 25 || floorCases < 8 {
		t.Fatalf("only %d trimmed and %d floor cases: the generator does not test enough", trimmedCases, floorCases)
	}
}

func randomJSONValue(r *rand.Rand, depth int) any {
	switch n := r.Intn(6); {
	case n == 0 && depth < 3:
		m := map[string]any{}
		for range r.Intn(4) {
			m[randomText(r, 1+r.Intn(8))] = randomJSONValue(r, depth+1)
		}
		return m
	case n == 1 && depth < 3:
		var list []any
		for range r.Intn(4) {
			list = append(list, randomJSONValue(r, depth+1))
		}
		return list
	case n == 2:
		return r.Intn(100000)
	default:
		return randomText(r, randomSize(r, 3000))
	}
}

func randomNormalized(r *rand.Rand) normalizedOutput {
	var n normalizedOutput
	index := 0
	for range []int{0, 1, 5, 40, 300, 800}[r.Intn(6)] {
		index += 1 + r.Intn(2)
		kind := []archive.TurnKind{archive.TurnKindHumanPrompt, archive.TurnKindAssistant, archive.TurnKindShellCommand, archive.TurnKindCompactSummary}[r.Intn(4)]
		turn := archive.NormalizedTurn{RecordIndex: index, Role: "user", Kind: kind, Text: randomText(r, randomSize(r, 5000)), Timestamp: "2026-01-02T03:04:05Z"}
		n.Turns = append(n.Turns, normalizedTurn{NormalizedTurn: turn})
		if r.Intn(2) == 0 {
			index++
			var input map[string]any
			if r.Intn(3) != 0 {
				input = map[string]any{"cmd": randomJSONValue(r, 0), "n": r.Intn(9)}
			}
			call := archive.NormalizedToolCall{RecordIndex: index, CallID: fmt.Sprintf("c%d", index), Name: "Bash", Input: input}
			n.ToolCalls = append(n.ToolCalls, call)
			if r.Intn(2) == 0 {
				index++
				n.ToolResults = append(n.ToolResults, archive.NormalizedToolResult{RecordIndex: index, CallID: call.CallID, OutputBytes: r.Intn(100000)})
			}
		}
	}
	for i := range []int{0, 0, 2, 100, 3000}[r.Intn(5)] {
		n.HookFinals = append(n.HookFinals, archive.HookFinalReconciliation{EvidenceIndex: i, Status: archive.HookFinalStatusUnreconciledIdentity})
	}
	return n
}

// fixedHomeLength is the length of every home the random-session property
// test uses, longer than any platform's temporary directory.
const fixedHomeLength = 200

func TestShowTranscriptJSONTrimmingHoldsTheLimitForRandomSessions(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	bundle := archive.SourceBundle{ArchiveSessionID: view.SessionID}
	r := rand.New(rand.NewSource(20260930))
	trimmedCases, floorCases := 0, 0
	for i := range 90 {
		n := randomNormalized(r)
		full, err := normalizedDocuments(view, n)
		if err != nil {
			t.Fatal(err)
		}
		// The saved record's path is in the floor, and the floor sizes the
		// next random draw, so a temporary directory whose random name
		// varies in length would make the cases vary from run to run and
		// between platforms. Every case gets a home of one fixed length.
		base := t.TempDir()
		home := base + string(os.PathSeparator) + strings.Repeat("h", max(1, fixedHomeLength-len(base)-1))
		floorView := normalizedOutput{Trimmed: &trimmedOutput{FullRecord: showFullPath(home, bundle, true, false)}}
		for _, kind := range []trimKind{trimToolResults, trimHookFinals, trimToolInput, trimAssistantText, trimPromptText, trimOldestRecords} {
			floorView.Trimmed.Omitted = append(floorView.Trimmed.Omitted, trimmedOmitted{Kind: kind, Count: 99999})
		}
		floorView.Trimmed.MaxBytes = 99999999
		floorDocs, _ := normalizedDocuments(view, floorView)
		limit := randomLimit(r, len(full))
		if r.Intn(2) == 0 {
			limit = len(floorDocs) + r.Intn(max(len(full)-len(floorDocs), 1)*2)
		}
		label := fmt.Sprintf("case %d: limit %d, untrimmed %d, %d turns", i, limit, len(full), len(n.Turns))
		var stderr bytes.Buffer
		fitted, err := fitNormalizedToLimit(view, n, bundle, limit, home, &stderr)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		printed, err := normalizedDocuments(view, fitted)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := fitNormalizedToLimit(view, n, bundle, limit, home, &bytes.Buffer{})
		if got, _ := normalizedDocuments(view, again); !bytes.Equal(got, printed) {
			t.Fatalf("%s: a second run printed something else", label)
		}
		if !utf8.Valid(printed) {
			t.Fatalf("%s: output is not valid UTF-8", label)
		}
		var documents []map[string]json.RawMessage
		for dec := json.NewDecoder(bytes.NewReader(printed)); dec.More(); {
			var doc map[string]json.RawMessage
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("%s: not valid JSON: %v", label, err)
			}
			documents = append(documents, doc)
		}
		if len(documents) != 2 {
			t.Fatalf("%s: %d documents, want 2", label, len(documents))
		}
		if len(full) <= limit {
			if !bytes.Equal(printed, full) || fitted.Trimmed != nil || stderr.Len() != 0 {
				t.Fatalf("%s: output that fits was changed", label)
			}
			continue
		}
		if _, ok := documents[1]["trimmed"]; !ok {
			// Nothing but the sidecar, which is over the limit itself.
			if len(n.Turns)+len(n.ToolCalls)+len(n.ToolResults)+len(n.HookFinals) != 0 || !strings.Contains(stderr.String(), "over the") {
				t.Fatalf("%s: over the limit and no trimmed object", label)
			}
			continue
		}
		trimmedCases++
		saved, err := os.ReadFile(showFullPath(home, bundle, true, false))
		if err != nil || !bytes.Equal(saved, full) {
			t.Fatalf("%s: saved record is not the untrimmed output (%v)", label, err)
		}
		if limit >= len(floorDocs) {
			floorCases++
			if len(printed) > limit || stderr.Len() != 0 {
				t.Fatalf("%s: printed %d bytes (floor %d), stderr=%q", label, len(printed), len(floorDocs), stderr.String())
			}
		} else if len(printed) > limit && !strings.Contains(stderr.String(), "over the") {
			t.Fatalf("%s: over the limit without a warning", label)
		}
		for _, turn := range fitted.Turns {
			if turn.TextTruncated && len(turn.Text) > 2000 {
				t.Fatalf("%s: a truncated turn holds %d bytes", label, len(turn.Text))
			}
		}
	}
	if trimmedCases < 25 || floorCases < 8 {
		t.Fatalf("only %d trimmed and %d floor cases: the generator does not test enough", trimmedCases, floorCases)
	}
}
