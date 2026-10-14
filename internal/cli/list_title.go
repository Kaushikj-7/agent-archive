package cli

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// replayTitlePrefix is the mark formatSessionRows puts before a replay's
// title. listTitle keeps it and cleans the title after it.
const replayTitlePrefix = "[replay] "

// pastedFilesPrefix starts the title Claude Code gives a prompt that is only
// pasted files; listTitle shortens it to pastedFilesLabel.
const pastedFilesPrefix = "files pasted by the user:"

// pastedFilesLabel is what a pasted-files title becomes, followed by the first
// pasted name when one can be read.
const pastedFilesLabel = "Pasted files"

// minHashRun is the shortest run of hexadecimal digits a slash command's
// arguments shorten, and shortHashLength how much of it they keep.
const (
	minHashRun      = 12
	shortHashLength = 7
)

// maxListNumberDigits is the longest number stripTitleMarkup reads as an
// ordered-list marker ("12. "), so "2024. " is left alone.
const maxListNumberDigits = 2

// minSentenceWords is how many words a first sentence needs before listTitle
// drops the sentences after it, so "Hi. Fix the scroll bug" stays whole.
const minSentenceWords = 3

// sentenceAbbreviations are words ending in "." that do not end a sentence.
var sentenceAbbreviations = map[string]bool{
	"e.g.": true, "i.e.": true, "vs.": true, "etc.": true, "cf.": true,
	"approx.": true, "mr.": true, "mrs.": true, "ms.": true, "dr.": true,
	"no.": true, "st.": true,
}

// listTitle turns a session's display title (archive.DisplayTitle passed
// through archive.DisplayLine) into the short, readable title the session list
// shows. It keeps the first line and first sentence; strips a leading markdown
// heading, list bullet or blockquote and the quotes or backticks around the
// text; writes a prompt of pasted files as "Pasted files: <first name>";
// writes a slash command whose arguments hold long hashes as words with short
// hashes ("/code-review d3e5256…" for a full hash and range becomes "Code
// review d3e5256…"); and upper-cases the first letter of a plain first word.
// A leading "[replay] " mark is kept. It is deterministic and never cuts to a
// width; the layout does that. When cleaning leaves nothing, it returns
// display unchanged, so the result is empty only when display is.
func listTitle(display string) string {
	text := strings.TrimSpace(display)
	if rest, ok := strings.CutPrefix(text, replayTitlePrefix); ok {
		if cleaned := cleanTitle(rest); cleaned != "" {
			return replayTitlePrefix + cleaned
		}
		return display
	}
	if cleaned := cleanTitle(text); cleaned != "" {
		return cleaned
	}
	return display
}

// cleanTitle applies listTitle's rules until the text stops changing, however
// deeply markup nests ("> > > > fix it"), so listTitle is a fixed point. A
// pass that changes the text strips at least one byte, or only capitalizes a
// first letter (which can add a byte) that the next change must strip, so
// 2*len(text)+2 passes always reach the fixed point. Titles are at most 128
// runes (archive's sessionTitleLimit), so this stays small. It may return "".
func cleanTitle(text string) string {
	for range 2*len(text) + 2 {
		next := cleanTitleOnce(text)
		if next == text {
			break
		}
		text = next
	}
	return text
}

// cleanTitleOnce applies each of listTitle's rules once, in order.
func cleanTitleOnce(text string) string {
	if line, _, ok := strings.Cut(text, "\n"); ok {
		text = line
	}
	text = stripTitleMarkup(strings.TrimSpace(text))
	if label, ok := pastedFilesTitle(text); ok {
		return label
	}
	if short, ok := shortSlashCommand(text); ok {
		return short
	}
	text = firstSentence(text)
	return capitalizeTitle(text)
}

// stripTitleMarkup removes one leading markdown heading marker, list bullet
// or blockquote, then one ordered-list number ("1. ", "2) "), then the quotes or backticks around the text: a pair that
// wraps all of it, or an opening or closing mark with no partner.
func stripTitleMarkup(text string) string {
	if hashes := len(text) - len(strings.TrimLeft(text, "#")); hashes > 0 && hashes <= 6 && strings.HasPrefix(text[hashes:], " ") {
		text = strings.TrimSpace(text[hashes:])
	}
	for _, bullet := range []string{"- ", "* ", "+ ", "• ", "> "} {
		if rest, ok := strings.CutPrefix(text, bullet); ok {
			text = strings.TrimSpace(rest)
			break
		}
	}
	if digits := len(text) - len(strings.TrimLeft(text, "0123456789")); digits > 0 && digits <= maxListNumberDigits {
		if rest, ok := strings.CutPrefix(text[digits:], ". "); ok {
			text = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(text[digits:], ") "); ok {
			text = strings.TrimSpace(rest)
		}
	}
	for _, pair := range [][2]string{{`"`, `"`}, {"`", "`"}, {"“", "”"}, {"‘", "’"}, {"'", "'"}} {
		open, closing := pair[0], pair[1]
		if len(text) >= len(open)+len(closing) && strings.HasPrefix(text, open) && strings.HasSuffix(text, closing) {
			inner := text[len(open) : len(text)-len(closing)]
			if !strings.Contains(inner, open) && !strings.Contains(inner, closing) {
				return strings.TrimSpace(inner)
			}
		}
	}
	for _, mark := range []string{`"`, "`"} {
		count := strings.Count(text, mark)
		if lead := len(text) - len(strings.TrimLeft(text, mark)); lead > 0 && lead == count {
			text = strings.TrimSpace(text[lead:])
		} else if trail := len(text) - len(strings.TrimRight(text, mark)); trail > 0 && trail == count {
			text = strings.TrimSpace(text[:len(text)-trail])
		}
	}
	if rest, ok := strings.CutPrefix(text, "“"); ok && !strings.Contains(rest, "”") {
		text = strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutSuffix(text, "”"); ok && !strings.Contains(rest, "“") {
		text = strings.TrimSpace(rest)
	}
	return text
}

// pastedFilesTitle reports whether text is Claude Code's title for pasted
// files ("Files pasted by the user: ## "name": /path…") and, if so, returns
// "Pasted files: <name>": the first quoted name up to its first " · ", else
// the base name of the first path. A name it cannot read is left out.
func pastedFilesTitle(text string) (string, bool) {
	if len(text) < len(pastedFilesPrefix) || !strings.EqualFold(text[:len(pastedFilesPrefix)], pastedFilesPrefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text[len(pastedFilesPrefix):]), "#"))
	name := ""
	switch {
	case strings.HasPrefix(rest, `"`):
		name, _, _ = strings.Cut(rest[1:], `"`)
	case strings.HasPrefix(rest, "“"):
		name, _, _ = strings.Cut(rest[len("“"):], "”")
	default:
		name, _, _ = strings.Cut(rest, " ")
		name = strings.TrimRight(name, ":,;")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
	}
	name, _, _ = strings.Cut(name, " · ")
	name = strings.TrimSpace(name)
	if name == "" || name == "…" {
		return pastedFilesLabel, true
	}
	return pastedFilesLabel + ": " + name, true
}

// shortSlashCommand reports whether text is a slash command whose arguments
// hold a long hash and, if so, returns it as words with short hashes:
// "/code-review <hash>...<hash>" becomes "Code review <7 digits>…". Other
// slash commands are left as they are.
func shortSlashCommand(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	command, args, _ := strings.Cut(text[1:], " ")
	if command == "" || strings.ContainsFunc(command, func(r rune) bool {
		return !isASCIIAlnum(r) && r != '-' && r != '_' && r != ':'
	}) {
		return "", false
	}
	short, changed := shortenHashes(args)
	if !changed {
		return "", false
	}
	if i := strings.LastIndex(command, ":"); i >= 0 {
		command = command[i+1:]
	}
	words := strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(command))
	if words == "" {
		return "", false
	}
	words = strings.ToUpper(words[:1]) + words[1:]
	if short = strings.TrimSpace(short); short != "" {
		words += " " + short
	}
	return words, true
}

// shortenHashes cuts each run of at least minHashRun hexadecimal digits (with
// a decimal digit and a letter) in text to its first shortHashLength digits and
// "…", dropping the far end of a range ("<hash>..<hash>" or "<hash>...<hash>").
// It reports whether it cut any.
func shortenHashes(text string) (string, bool) {
	var b strings.Builder
	changed := false
	for i := 0; i < len(text); {
		if !isASCIIAlnum(rune(text[i])) {
			b.WriteByte(text[i])
			i++
			continue
		}
		end := alnumRunEnd(text, i)
		run := text[i:end]
		if !isHashRun(run) {
			b.WriteString(run)
			i = end
			continue
		}
		b.WriteString(run[:shortHashLength] + "…")
		changed = true
		i = end
		for _, dots := range []string{"...", ".."} {
			if strings.HasPrefix(text[i:], dots) {
				if far := alnumRunEnd(text, i+len(dots)); isHexRun(text[i+len(dots) : far]) {
					i = far
				}
				break
			}
		}
	}
	return b.String(), changed
}

// alnumRunEnd returns the index just past the run of ASCII letters and digits
// starting at text[start].
func alnumRunEnd(text string, start int) int {
	end := start
	for end < len(text) && isASCIIAlnum(rune(text[end])) {
		end++
	}
	return end
}

// isHashRun reports whether run looks like a commit hash: at least minHashRun
// hexadecimal digits, with both a decimal digit and a letter, so a plain number
// (a timestamp, an issue ID) is never cut.
func isHashRun(run string) bool {
	return len(run) >= minHashRun && isHexRun(run) && strings.ContainsAny(run, "0123456789") && strings.ContainsAny(run, "abcdefABCDEF")
}

// isHexRun reports whether run is one or more hexadecimal digits.
func isHexRun(run string) bool {
	if run == "" {
		return false
	}
	for _, r := range run {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
	}
	return true
}

// isASCIIAlnum reports whether r is an ASCII letter or digit.
func isASCIIAlnum(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// firstSentence returns text up to the end of its first sentence: a ".", "!"
// or "?" followed by a space and more text. A "." in an ellipsis, an
// abbreviation ("e.g.", "(e.g.", "U.S.") or an initial does not end one, nor
// does one inside a word ("v0.2.1", "list_table.go", a URL). A first sentence
// under minSentenceWords words keeps the text whole. The "." that ends the
// result is dropped (see trimFinalPeriod); "!" and "?" are kept.
func firstSentence(text string) string {
	for i := 0; i+2 < len(text); i++ {
		c := text[i]
		if (c != '.' && c != '!' && c != '?') || text[i+1] != ' ' || text[i+2] == ' ' {
			continue
		}
		head := text[:i+1]
		if c == '.' {
			if i > 0 && text[i-1] == '.' {
				continue
			}
			if isAbbreviation(head[strings.LastIndex(head, " ")+1:]) {
				continue
			}
			head = text[:i]
		}
		if len(strings.Fields(head)) < minSentenceWords {
			return trimFinalPeriod(text)
		}
		return strings.TrimSpace(head)
	}
	return trimFinalPeriod(text)
}

// trimFinalPeriod drops one "." that ends text, as firstSentence drops the one
// that ends a first sentence, so "Fix the bug." reads as "Fix the bug". It
// keeps an ellipsis, an abbreviation or initial, and a lone ".".
func trimFinalPeriod(text string) string {
	head, ok := strings.CutSuffix(text, ".")
	if !ok || head == "" || strings.HasSuffix(head, ".") || strings.HasSuffix(head, " ") || isAbbreviation(text[strings.LastIndex(text, " ")+1:]) {
		return text
	}
	return head
}

// isAbbreviation reports whether word, which ends in ".", is an abbreviation
// rather than the end of a sentence: one in sentenceAbbreviations, an initial
// ("J."), or letters joined by dots ("U.S.", "a.m."). Opening brackets and
// quotes before it ("(e.g.") do not count.
func isAbbreviation(word string) bool {
	word = strings.TrimLeft(word, "([{\"'`“‘")
	if sentenceAbbreviations[strings.ToLower(word)] || utf8.RuneCountInString(word) == 2 && unicode.IsUpper(firstRune(word)) {
		return true
	}
	parts := strings.Split(strings.TrimSuffix(word, "."), ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if utf8.RuneCountInString(part) != 1 || !unicode.IsLetter(firstRune(part)) {
			return false
		}
	}
	return true
}

// capitalizeTitle upper-cases the first letter of text when its first word is
// plain lower-case words ("help", "let’s", "when,"), leaving commands, paths,
// versions, identifiers ("iPhone", "go.mod", "v0.2.1") and a lone short ID
// (hex digits, perhaps with the dashes of a longer UUID prefix) as they are.
func capitalizeTitle(text string) string {
	word, _, _ := strings.Cut(text, " ")
	first := firstRune(word)
	if !unicode.IsLower(first) || len(text) >= minShortSessionID && isHexRun(strings.ReplaceAll(text, "-", "")) {
		return text
	}
	for _, r := range strings.TrimRight(word, ".,:;!?…") {
		if !unicode.IsLetter(r) && r != '\'' && r != '’' && r != '-' {
			return text
		}
		if unicode.IsUpper(r) || unicode.IsTitle(r) {
			return text
		}
	}
	title := unicode.ToTitle(first)
	if title == first {
		return text
	}
	return string(title) + text[utf8.RuneLen(first):]
}

// firstRune returns the first rune of s, or utf8.RuneError when s is empty.
func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}
