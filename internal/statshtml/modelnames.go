package statshtml

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// publicModelFamily snapshots the built-in price table, the only authority
// on which model names are public. A person's own price file (--prices) can add
// a model of their own to the run, but that must not make its name shareable.
var publicModelFamily = sync.OnceValue(func() func(string) (string, bool) {
	return stats.DefaultPriceTable().FamilyLookup()
})

// modelNamer decides how the model names of one page are shown. A model id can
// name a client (a fine-tune "ft:gpt-4o:acme::abc", a deployment called
// "acme-prod"), so unless real names were asked for, a page shows a model's
// name only when the built-in price table lists it. Any other model is a
// stand-in ("model A", "model B") in the order of the run's own ranking, one
// stand-in per model throughout the page.
type modelNamer struct {
	reveal bool
	stand  *namer
	// rows maps a row's label to the text shown for it, and ids maps each
	// model id of a row to that row's label.
	rows map[string]string
	ids  map[string]string
	// classes maps a row's label to its color class. Only a model the page
	// names gets its family's color; a stand-in is neutral, so the color of a
	// hidden model tells nothing about it.
	classes map[string]string
	// hidden is whether a row of the run was replaced by a stand-in.
	hidden bool
}

// newModelNamer names every row of the run, best-ranked first, so the letters
// follow the ranking and are fixed before any section asks.
func newModelNamer(reveal bool, rows []stats.ModelRow) *modelNamer {
	m := &modelNamer{
		reveal: reveal, stand: newNamer(false, "model"),
		rows: map[string]string{}, ids: map[string]string{}, classes: map[string]string{},
	}
	for _, r := range rows {
		for _, id := range r.Models {
			m.ids[stats.NormalizeModel(id)] = r.Label
		}
		if reveal || publicRow(r) {
			m.rows[r.Label] = clean(r.Label)
			m.classes[r.Label] = familyClass(r.Label)
			continue
		}
		m.rows[r.Label] = m.stand.name(r.Label)
		m.hidden = true
	}
	return m
}

// publicRow is whether a row's label and every model id under it are ones the
// built-in price table lists, the label being the family it groups them under.
// A label that came from a person's own price file, or from an id the table
// does not list, is not public. The archive's own placeholders for a model it
// could not name say nothing about anyone's work.
func publicRow(r stats.ModelRow) bool {
	if len(r.Models) == 0 {
		return isPlaceholderModel(r.Label)
	}
	familyFor := publicModelFamily()
	for _, id := range r.Models {
		if isPlaceholderModel(id) && id == r.Label {
			continue
		}
		family, ok := familyFor(id)
		if !ok || family != r.Label {
			return false
		}
	}
	return true
}

// familyClass is the color class of a model family: the Claude families each
// have a color and the OpenAI ones (gpt and codex models) share one. The class
// is chosen from this list, never made from the label's text.
func familyClass(label string) string {
	name := strings.ToLower(label)
	switch {
	case strings.HasPrefix(name, "opus"):
		return "opus"
	case strings.HasPrefix(name, "fable"):
		return "fable"
	case strings.HasPrefix(name, "sonnet"):
		return "sonnet"
	case strings.HasPrefix(name, "haiku"):
		return "haiku"
	case strings.HasPrefix(name, "gpt"), strings.HasPrefix(name, "codex"):
		return "gpt"
	}
	return "other"
}

// class is the color class of a row's bar: its family's when the page names the
// model, neutral when it is a stand-in or unknown to the page.
func (m *modelNamer) class(label string) string {
	if class, ok := m.classes[label]; ok {
		return class
	}
	return "other"
}

func isPlaceholderModel(id string) bool {
	return slices.Contains([]string{archive.UnknownModel, archive.OtherModels}, id)
}

// label is the text shown for a row's label (a cost-by-model row, the
// favorite model). A label no row carries is never shown as it is.
func (m *modelNamer) label(label string) string {
	if text, ok := m.rows[label]; ok {
		return text
	}
	if m.reveal {
		return clean(label)
	}
	return m.stand.name(label)
}

// dateOrVersion is what may follow a listed model id once it is normalized
// without being text of the person's: a date ("-20251001", "@20251001"), a
// platform's version ("-v1:0") and Claude Code's context marker ("[1m]").
var dateOrVersion = regexp.MustCompile(`^[-@:0-9v]*(?:\[[0-9]+[km]\])?$`)

// carriesOwnText is whether a model id that the built-in table lists once it is
// normalized has text of its own around the listed part. The engine looks a
// model up after dropping a path ("acme-client/claude-opus-5"), a cloud
// vendor's prefix ("acme-prod.anthropic.claude-opus-5") or a bracketed suffix
// ("claude-opus-5[acme-client]"), and whatever was dropped can name a client,
// so such an id is never echoed as typed. Only a date, a platform's version or
// a context marker may follow the listed part.
func carriesOwnText(id string) bool {
	typed := strings.TrimSpace(strings.ToLower(id))
	rest, ok := strings.CutPrefix(typed, stats.NormalizeModel(id))
	return !ok || !dateOrVersion.MatchString(rest)
}

// id is the text shown for a model id the caller named (the --model filter):
// the id itself when the built-in table lists it, else the stand-in of the row
// it belongs to, so it reads the same as in the table, else a stand-in of its
// own.
func (m *modelNamer) id(id string) string {
	if m.reveal {
		return clean(id)
	}
	if family, ok := publicModelFamily()(id); ok {
		if carriesOwnText(id) {
			return family
		}
		return clean(id)
	}
	if row, ok := m.ids[stats.NormalizeModel(id)]; ok {
		return m.label(row)
	}
	return m.stand.name(stats.NormalizeModel(id))
}
