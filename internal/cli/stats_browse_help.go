package cli

import (
	"fmt"
	"path/filepath"
	"strings"
)

// statsHelpWidth is the widest the help is laid out, so its lines stay
// readable on a wide terminal.
const statsHelpWidth = 100

// helpLines is the key list shown over the view, laid out for width columns.
// It is plain ASCII, so it reads the same in any locale.
func (b *statsBrowser) helpLines(width int) []string {
	width = min(max(width, statsMinWidth), statsHelpWidth)
	const keyWidth = 16
	style := b.view.style
	var lines []string
	heading := func(text string) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, style.bold(text))
	}
	entry := func(keys, text string) {
		prefix := padRight("  "+keys, keyWidth) + " "
		for i, line := range strings.Split(hangingIndent(prefix, text, width), "\n") {
			if i == 0 {
				line = style.bold(padRight("  "+keys, keyWidth)) + line[len(padRight("  "+keys, keyWidth)):]
			}
			lines = append(lines, line)
		}
	}
	windows := make([]string, len(b.windows))
	for i, days := range b.windows {
		windows[i] = fmt.Sprintf("%dd", days)
	}
	heading("agent-archive stats: keys")
	heading("Views")
	entry("o", "overview: the headline numbers, and where they went")
	entry("d", "detail: streaks, busiest day, token types, coverage and notes")
	entry("p", "projects: up to 500 projects, by spend, with each one's share")
	entry("m", "models: up to 500 model families, priced or not")
	entry("a", "agents: each agent side by side")
	heading("Window")
	entry("w", "cycle the window: "+strings.Join(windows, ", ")+" (the bar shows the next one). The numbers are counted again from what was already read, so it is instant.")
	heading("Scroll")
	entry("Up Down j k", "one line; the mouse wheel does the same")
	entry("PgUp PgDn", "one screen; space pages down too")
	entry("Home End", "the top, the bottom")
	entry("", "A screen taller than the terminal shows where you are on the last row: Top with an arrow when there is more below, a percentage, End.")
	heading("Other")
	entry("h", "save this window as a redacted web page: asks for a file name, and never replaces a file")
	entry("?", "this help; any key but a scroll key closes it")
	entry("q Ctrl-C Ctrl-D", "quit; Esc does not, so an arrow key split in transit cannot close the screen")
	lines = append(lines, "")
	lines = append(lines, style.dim(hangingIndent("", "The saved page says project A, model A, and so on instead of your names, so it can be shared. To keep the real names, run agent-archive stats --html --include-names --output FILE.", width)))
	// The dim paragraph is one string with line breaks: one line each.
	return splitLines(lines)
}

// splitLines is lines with any that hold line breaks split into one line
// each.
func splitLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		out = append(out, strings.Split(line, "\n")...)
	}
	return out
}

// savePrompt is the last row while h asks for a file name: the question, what
// is typed, and no more than one row of both. The question shortens as the
// terminal narrows, and the end of a long name is kept.
func (b *statsBrowser) savePrompt(width int) string {
	def := b.defaultHTMLName()
	question := "Save as: "
	for _, q := range []string{
		"Save redacted HTML as (Enter: " + def + ", Esc cancels): ",
		"Save HTML as [" + def + "]: ",
		"Save as (Enter: default name): ",
	} {
		// Room is left to type in.
		if visibleWidth(q)+statsMinTypedRoom <= width {
			question = q
			break
		}
	}
	room := width - visibleWidth(question)
	if room < 1 {
		return truncateVisible(question, width)
	}
	return b.view.style.bold(question) + tailVisible(b.typed, room, b.view.glyphs.ellipsis)
}

// statsMinTypedRoom is the columns the save prompt keeps for the name being
// typed before it takes a shorter question.
const statsMinTypedRoom = 12

// tailVisible is the end of text that fits limit columns, with the ellipsis
// where it is cut at the start.
func tailVisible(text string, limit int, ellipsis string) string {
	if visibleWidth(text) <= limit {
		return text
	}
	room := limit - visibleWidth(ellipsis)
	if room <= 0 {
		return truncateVisible(ellipsis, limit)
	}
	runes := []rune(text)
	used := 0
	i := len(runes)
	for i > 0 && used+runeWidth(runes[i-1]) <= room {
		i--
		used += runeWidth(runes[i])
	}
	return ellipsis + string(runes[i:])
}

// defaultHTMLName is the file name h saves to when none is typed: today's
// date, in the counting zone, in the current folder.
func (b *statsBrowser) defaultHTMLName() string {
	return "agent-archive-stats-" + b.inputs.now.In(b.inputs.location).Format("2006-01-02") + ".html"
}

// saveHTML writes the window on show as the redacted web page, as
// stats --html does (project A, model A, and so on; there is no way to turn
// the names on here). name is relative to the folder the command was run in,
// or absolute, or under "~/"; empty means the default name. It never replaces
// a file, refuses a folder, a link or a missing folder, and writes in one
// step (writeStatsHTMLFile). It returns the message to show: where the page
// went, or why it was not written.
func (b *statsBrowser) saveHTML(name string) string {
	// A space typed by accident is not part of the name.
	name = strings.TrimSpace(name)
	if name == "" {
		name = b.defaultHTMLName()
	}
	name = expandHome(b.env, name)
	path := name
	if !filepath.IsAbs(path) {
		dir, err := b.env.workingDir()
		if err != nil {
			return "Not saved: " + err.Error()
		}
		path = filepath.Join(dir, name)
	}
	if err := checkStatsHTMLTarget(path, false); err != nil {
		return "Not saved: " + noForce(err, path, name)
	}
	// The web page keeps the engine's default lists, like --html.
	computed := b.inputs.compute(b.windows[b.window], false)
	page, err := renderStatsHTML(computed, b.inputs.filters, b.inputs.now, false, statsEmptyMessage(computed, b.inputs.filters, b.inputs.hasSessions))
	if err != nil {
		return "Not saved: " + err.Error()
	}
	if err := writeStatsHTMLFile(path, page, false); err != nil {
		return "Not saved: " + noForce(err, path, name)
	}
	b.addSaved(path)
	return b.savedMessage(path)
}

// noForce is err's message for the screen, which has no --force: an existing
// file is never replaced here, and the person is told to pick another name.
// The path is the name as typed, so the reason is not cut off by a long one.
func noForce(err error, path, name string) string {
	message := strings.ReplaceAll(err.Error(), path, name)
	return strings.Replace(message, "; pass --force to replace it", "; it is never replaced here, so type another name", 1)
}

// savedMessage says where the page went: the whole path when it fits, else
// its end, which is the part that names the file.
func (b *statsBrowser) savedMessage(path string) string {
	const prefix, suffix = "Saved ", " (names replaced)"
	width := b.layout().width
	if room := width - len(prefix) - len(suffix); room >= statsMinTypedRoom {
		return prefix + tailVisible(path, room, b.view.glyphs.ellipsis) + suffix
	}
	return prefix + tailVisible(path, max(width-len(prefix), 1), b.view.glyphs.ellipsis)
}
