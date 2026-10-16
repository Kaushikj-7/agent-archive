package cli

import (
	"fmt"
	"math"

	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// The commands whose output has every row of a list a screen cuts. A hint
// names one only when it is true: plain --json keeps the engine's top five
// projects, skills and MCP servers, so --json --all has every one of them,
// and the models are in --json (the engine never cuts them). The detail
// screen's project table (--by project) is a group table, whose rows are in
// --json --by project.
const (
	allProjectsHint      = "--json --all"
	allModelsHint        = "--json"
	allUsageHint         = "--json --all"
	allProjectGroupsHint = "--json --by project"
)

// listMore is the line under a list that says how many rows it leaves out and
// where to find them all: hint is the command that lists every row. See
// moreAtoms for the interactive screen's variant.
func (p *statsPrinter) listMore(shown, total int, hint string) []string {
	if total <= shown {
		return nil
	}
	return p.dimAll(p.packAtoms(0, 0, p.moreAtoms(total-shown, hint, p.width)))
}

// share is a row's part of a total (0 to 1); a total that is not above zero
// has no shares.
func share(part, total float64) (float64, bool) {
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, false
	}
	return math.Min(part/total, 1), true
}

// projectsPage is the projects by spend, up to statsMaxListRows: a bar, its
// sessions, tokens, spend and share of the spend.
func (p *statsPrinter) projectsPage() [][]string {
	return [][]string{p.header(pageProjects), p.projectsTable(), p.footer("--view overview")}
}

func (p *statsPrinter) projectsTable() []string {
	projects := p.s.Projects
	if len(projects) == 0 {
		return nil
	}
	total := max(p.s.TotalProjects, len(projects))
	shown := projects[:min(len(projects), statsMaxListRows)]
	top, sum := 0.0, 0.0
	for _, pr := range shown {
		if pr.Cost.USD != nil {
			top = math.Max(top, *pr.Cost.USD)
		}
	}
	if p.s.Overview.Cost.Value != nil {
		sum = *p.s.Overview.Cost.Value
	}
	var labels, sessions, tokens, costs, shares []string
	bar := &tableBar{width: 20}
	for _, pr := range shown {
		labels = append(labels, projectLabel(pr.Name))
		sessions = append(sessions, statsfmt.CommaInt(int64(pr.Sessions)))
		tokens = append(tokens, tokensText(pr.Tokens))
		costs = append(costs, p.spend(pr.Cost, pr.Tokens))
		bar.codes = append(bar.codes, statsRoleCodes[roleProject])
		bar.shares = append(bar.shares, -1)
		shares = append(shares, "")
		if pr.Cost.USD != nil {
			if f, ok := share(*pr.Cost.USD, top); ok {
				bar.shares[len(bar.shares)-1] = f
			}
			if f, ok := share(*pr.Cost.USD, sum); ok {
				shares[len(shares)-1] = statsfmt.Percent(f)
			}
		}
	}
	title := "PROJECTS"
	if total > 1 {
		title += fmt.Sprintf(" (%d)", total)
	}
	lines := p.table(title, labels, bar, []tableCol{
		{head: "sessions", cells: sessions},
		{head: "tokens", cells: tokens},
		{head: "est. cost", cells: costs},
		{head: "share", cells: shares, drop: true},
	})
	return append(lines, p.listMore(len(shown), total, allProjectsHint)...)
}

// modelsPage is the model families, up to statsMaxListRows: its spend and share of it, tokens and
// sessions. A model the price table does not list is flagged unpriced, with
// its tokens.
func (p *statsPrinter) modelsPage() [][]string {
	return [][]string{p.header(pageModels), p.modelsTable(), p.footer("--view overview")}
}

func (p *statsPrinter) modelsTable() []string {
	models := p.s.Models
	if len(models) == 0 {
		return p.dimAll(p.wrap("No session in this window reports tokens by model, so there is no spend to split by model."))
	}
	top := 0.0
	for _, m := range models {
		if m.Priced && m.Cost.USD != nil {
			top = math.Max(top, *m.Cost.USD)
		}
	}
	shown := models[:min(len(models), statsMaxListRows)]
	var labels, sessions, tokens, costs, shares []string
	bar := &tableBar{width: 20}
	for _, m := range shown {
		labels = append(labels, clean(m.Label))
		sessions = append(sessions, statsfmt.CommaInt(int64(m.Sessions)))
		tokens = append(tokens, statsfmt.TokenCount(m.Tokens))
		bar.codes = append(bar.codes, modelCode(m.Label))
		bar.shares = append(bar.shares, -1)
		costs = append(costs, "unpriced")
		shares = append(shares, "")
		if m.Priced && m.Cost.USD != nil {
			costs[len(costs)-1] = p.money(*m.Cost.USD)
			if m.Cost.Partial {
				costs[len(costs)-1] += "+"
			}
			if f, ok := share(*m.Cost.USD, top); ok {
				bar.shares[len(bar.shares)-1] = f
			}
			if m.CostShare != nil {
				shares[len(shares)-1] = statsfmt.Percent(*m.CostShare)
			}
		}
	}
	lines := p.table(fmt.Sprintf("MODELS (%d)", len(models)), labels, bar, []tableCol{
		{head: "sessions", cells: sessions},
		{head: "tokens", cells: tokens},
		{head: "est. cost", cells: costs},
		{head: "share", cells: shares, drop: true},
	})
	lines = append(lines, p.listMore(len(shown), len(models), allModelsHint)...)
	if names, total := p.unpricedNames(); total > 0 {
		whose := "its"
		if total > 1 {
			whose = "their"
		}
		lines = append(lines, p.dimAll(p.wrap("Unpriced: the price table does not list "+names+", so "+whose+" tokens are left out of spend."))...)
	}
	return lines
}

// agentsPage is each agent's part of the work: sessions and share, tokens,
// spend and cache hit rate, and what each agent does not record.
func (p *statsPrinter) agentsPage() [][]string {
	return [][]string{p.header(pageAgents), p.agentsTable(true), p.agentNotes(), p.footer("--view overview")}
}

// agentNotes say what each agent leaves unknown: the sessions with no token
// counts, and what its records do not carry.
func (p *statsPrinter) agentNotes() []string {
	var notes []string
	for _, a := range p.s.Agents {
		who := nameOf(a.Label)
		if a.UnknownTokenSessions > 0 {
			notes = append(notes, p.observationAccountingContext(fmt.Sprintf("%s: %s of %s have no token data; left out of tokens and spend.",
				who, statsfmt.CommaInt(int64(a.UnknownTokenSessions)), plural(a.Sessions, "session"))))
		}
		if a.Harness == "codex" {
			notes = append(notes, who+": MCP calls and tool errors are not recorded.")
		}
	}
	notes = append(notes, "Cache hit is cache reads over all input-side tokens.")
	var lines []string
	for _, note := range notes {
		lines = append(lines, p.wrap(note)...)
	}
	return p.dimAll(lines)
}
