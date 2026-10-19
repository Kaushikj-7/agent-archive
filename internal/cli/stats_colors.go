package cli

import "strings"

// The stats screen's color table: the only place a color is chosen. Every
// entry is a code from the terminal's own 16 ANSI colors (or bold or dim), so
// the screen follows the user's theme; no 256-color or truecolor code is ever
// written. A color is a role, never the only carrier of a meaning: each
// colored element also has a label, a glyph or a position that says the same
// (see statsColorsHaveNonColorCues in the tests).
//
//	agents        Claude Code yellow, Cursor blue, Codex green
//	model family  opus magenta, fable bright red, sonnet cyan, haiku yellow,
//	              gpt and codex models green
//	projects      cyan
//	daily spend   cyan
//	deltas        spend up yellow (amber), down green; never red
//	heads-up      yellow bullets
//	token types   cache read blue, cache write yellow, input green, output magenta
//	labels        dim; emphasis bold
//	key bar       keys bold, labels dim, the active view in reverse video
//
// Yellow carries three meanings (Claude Code, a heads-up bullet and a rise in
// spend, and the haiku and cache-write colors on other screens). They are far
// apart on a screen and each has a label or a glyph beside it. Only marks are
// yellow (a bullet, a bar, an arrow with its figure), never a sentence or an
// amount, because yellow text is hard to read on a light background.
//
// Judged on the screens rendered on a dark, a light, a Solarized dark and a
// Solarized light background: the yellow of Claude Code's bar, of a heads-up
// bullet and of a rise in spend does not read as one thing, because the bar is
// a large block under a legend that names it and the other two are a bullet
// and an arrow far below and above it. Two hues that do run together are
// Claude Code (Solarized's mustard) and Codex (its olive), which sit side by
// side on the agents bar; they are told apart by the blank cell between the
// segments and by the legend's names, as the color rule requires. The detail
// screen puts the agents' colors beside the token types' (cache write is also
// yellow, cache read blue, fresh input green): each bullet is labeled and the
// two lists are under different headings, so no color is asked to carry both.
//
// Dim (SGR 2) rather than bright black is the grey: bright black is the
// background itself in some popular themes (Solarized), and the rest of the
// command line already uses dim for secondary text.

// statsRole names what a piece of the screen means.
type statsRole int

// The roles of the stats screen.
const (
	// roleEmphasis is a title or a headline number.
	roleEmphasis statsRole = iota
	// roleLabel is a caption, a unit, a note: secondary text.
	roleLabel
	// roleProject is a project's bar.
	roleProject
	// roleSpendChart is the daily spend chart's bars.
	roleSpendChart
	// roleHeadsUp is a heads-up note's bullet.
	roleHeadsUp
	// roleDeltaUp is a rise in spend: amber, never red.
	roleDeltaUp
	// roleDeltaDown is a fall in spend.
	roleDeltaDown
	// roleCacheRead and the roles after it are the four kinds of token.
	roleCacheRead
	roleCacheWrite
	roleFreshInput
	roleOutput
	// roleKeyActive is the interactive screen's key bar item for the view
	// on show, in reverse video.
	roleKeyActive
)

// statsRoleCodes is the SGR code of each role.
var statsRoleCodes = map[statsRole]string{
	roleEmphasis:   "1",
	roleLabel:      "2",
	roleProject:    "36",
	roleSpendChart: "36",
	roleHeadsUp:    "33",
	roleDeltaUp:    "33",
	roleDeltaDown:  "32",
	roleCacheRead:  "34",
	roleCacheWrite: "33",
	roleFreshInput: "32",
	roleOutput:     "35",
	roleKeyActive:  "7",
}

// statsAgentCodes is the color of each agent, by the archive's harness name.
// An agent the table does not know is drawn without color.
var statsAgentCodes = map[string]string{
	"claude": "33",
	"cursor": "34",
	"codex":  "32",
}

// statsModelCodes is the color of each model family, matched as a prefix of
// the model's label ("opus-5.5", "gpt-6.1-sol", "claude-opus-5" when unpriced). The
// first prefix that matches wins. A family the table does not know is drawn
// without color.
var statsModelCodes = []struct {
	prefix string
	code   string
}{
	{"opus", "35"},
	{"fable", "91"},
	{"sonnet", "36"},
	{"haiku", "33"},
	{"gpt", "32"},
	{"codex", "32"},
}

// statsFamilyOf reduces a model label to the text a color's prefix is matched
// against: a full Claude model id ("claude-opus-5") to "opus-5".
func statsFamilyOf(label string) string {
	lower := strings.ToLower(label)
	lower = strings.TrimPrefix(lower, "claude-")
	return lower
}

// modelCode is the color of a model's label (by its family), or "" for none.
func modelCode(label string) string {
	family := statsFamilyOf(label)
	for _, m := range statsModelCodes {
		if strings.HasPrefix(family, m.prefix) {
			return m.code
		}
	}
	return ""
}

// agentCode is the color of an agent, or "" for none.
func agentCode(harness string) string { return statsAgentCodes[harness] }

// role paints text in a role's color.
func (p *statsPrinter) role(r statsRole, text string) string {
	return p.v.style.paint(statsRoleCodes[r], text)
}

// paint paints text in an SGR code; an empty code leaves it as it is.
func (p *statsPrinter) paint(code, text string) string {
	if code == "" {
		return text
	}
	return p.v.style.paint(code, text)
}
