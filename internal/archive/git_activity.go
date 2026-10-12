package archive

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitEventKind names one kind of git or pull request work a session did.
type GitEventKind string

// The kinds deriveGitActivity writes. A reader must accept any other
// lowercase kind, which a newer writer may add.
const (
	GitEventCommit    GitEventKind = "commit"
	GitEventPush      GitEventKind = "push"
	GitEventPRCreated GitEventKind = "pr_created"
	GitEventPRMerged  GitEventKind = "pr_merged"
)

// GitEventSource says which recognizer confirmed a GitEvent: a shell
// command's output (git, gh) or an MCP tool's result.
type GitEventSource string

// The sources deriveGitActivity writes.
const (
	GitEventSourceShell GitEventSource = "shell"
	GitEventSourceMCP   GitEventSource = "mcp"
)

// GitEvent is one commit, push, or pull request created or merged that a
// tool call made and its result confirmed. Every string is parsed and
// validated against a narrow shape, never copied from free text, and URL is
// rebuilt from the parsed parts. Commit messages, pull request text, and
// command lines are never kept.
type GitEvent struct {
	Kind GitEventKind `json:"kind"`
	// At is the timestamp of the record carrying the confirming result;
	// nil when the harness wrote none.
	At     *time.Time     `json:"at,omitempty"`
	Source GitEventSource `json:"source"`
	// SHA is the commit a commit event created, the new tip a push moved a
	// ref to, or a merge's commit, when the output reported one.
	SHA        string `json:"sha,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Repository string `json:"repository,omitempty"`
	PRNumber   int    `json:"pr_number,omitempty"`
	URL        string `json:"url,omitempty"`
}

// MaxGitActivity is the most entries Metadata.GitActivity holds, the first
// ones in transcript order. The schema's git_activity maxItems matches it;
// the counts stay exact beyond it.
const MaxGitActivity = 100

// gitCounts are the uncapped totals behind Metadata.GitActivity.
type gitCounts struct {
	commits    int
	pushes     int
	prsCreated int
	prsMerged  int
}

func (c *gitCounts) add(kind GitEventKind) {
	switch kind {
	case GitEventCommit:
		c.commits++
	case GitEventPush:
		c.pushes++
	case GitEventPRCreated:
		c.prsCreated++
	case GitEventPRMerged:
		c.prsMerged++
	}
}

// maxBranchLength and maxPRNumber bound what an event may carry; the schema
// matches them.
const (
	maxBranchLength = 255
	maxPRNumber     = 1 << 30
)

var (
	shaPattern        = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	repoPartPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	hostPattern       = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	gitCommitCommand  = regexp.MustCompile(`\bgit(?:\s+-[cC]\s+\S+|\s+--[\w.-]+(?:=\S+)?)*\s+commit\b`)
	gitPushCommand    = regexp.MustCompile(`\bgit(?:\s+-[cC]\s+\S+|\s+--[\w.-]+(?:=\S+)?)*\s+push\b`)
	ghPRCreateCommand = regexp.MustCompile(`\bgh\s+pr\s+create\b`)
	ghPRMergeCommand  = regexp.MustCompile(`\bgh\s+pr\s+merge\b`)
	commitLine        = regexp.MustCompile(`(?m)^\[([^\]\n]+) ([0-9a-f]{7,40})\] `)
	pushRemoteLine    = regexp.MustCompile(`^To (\S+)$`)
	pushUpdateLine    = regexp.MustCompile(`^\s*\+?\s*[0-9a-f]{7,40}\.\.\.?([0-9a-f]{7,40})\s+\S+ -> (\S+)`)
	pushNewBranchLine = regexp.MustCompile(`^\s*\*\s+\[new branch\]\s+\S+ -> (\S+)`)
	pullURLLine       = regexp.MustCompile(`(?m)^https://([A-Za-z0-9.-]+)/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)\s*$`)
	pullURLAnywhere   = regexp.MustCompile(`https://([A-Za-z0-9.-]+)/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)`)
	ghMergedLine      = regexp.MustCompile(`(?:Merged|Squashed and merged|Rebased and merged) pull request (?:([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+))?#([0-9]+)`)
	ghHeadFlag        = regexp.MustCompile(`(?:^|\s)(?:-H|--head)(?:=|\s+)["']?([^\s"']+)`)
	ghRepoFlag        = regexp.MustCompile(`(?:-R|--repo)[= ]\s*(?:([A-Za-z0-9.-]+)/)?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)(?:\s|$)`)
	exitCodeLine      = regexp.MustCompile(`(?mi)^(?:exit code:?|process exited with code)\s+(-?[0-9]+)\s*$`)
	commandSeparator  = regexp.MustCompile(`[|;&\n]`)
	pushDryRunFlag    = regexp.MustCompile(`(?:^|\s)(?:--dry-run|-n|--porcelain)(?:\s|$)`)
	ghAutoMergeFlag   = regexp.MustCompile(`(?:^|\s)--auto(?:\s|$)`)
)

// mcpServerTool splits an MCP tool name, mcp__<server>__<tool>.
func mcpServerTool(name string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, mcpToolPrefix)
	if !ok {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, mcpNameSeparator)
	if !ok || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// shellCommandText is the command a shell call ran, as toolSummary reads it,
// or a Codex exec script whole (its exec_command calls are in it). "" for
// any other call.
func shellCommandText(_ string, call NormalizedToolCall) string { return call.ShellCommand }

// shellGitEvents reads the events one shell command confirmed. The command
// only gates which outputs are read; the evidence is the output, so a
// chained command (git commit … && git push) can yield several events.
func shellGitEvents(command, output, branch string) []GitEvent {
	output, ok := shellOutput(output)
	if !ok {
		return nil
	}
	var events []GitEvent
	if gitCommitCommand.MatchString(command) {
		for _, match := range commitLine.FindAllStringSubmatch(output, -1) {
			events = append(events, GitEvent{
				Kind: GitEventCommit, Source: GitEventSourceShell,
				SHA: match[2], Branch: commitBranch(match[1]),
			})
		}
	}
	events = append(events, pushEvents(command, output)...)
	if segments := commandSegments(command, ghPRCreateCommand); len(segments) > 0 {
		head := branch
		if flagged := ghHeadBranch(segments[0]); flagged != "" {
			head = flagged
		}
		for _, match := range pullURLLine.FindAllStringSubmatch(output, -1) {
			if event, ok := pullRequestEvent(GitEventPRCreated, match[1], match[2], match[3], match[4]); ok {
				event.Source, event.Branch = GitEventSourceShell, head
				events = append(events, event)
			}
		}
	}
	// Enabling auto-merge prints no merge confirmation, so the output is
	// read when any gh pr merge in the command is a real merge; its flags
	// name the repository.
	for _, segment := range commandSegments(command, ghPRMergeCommand) {
		if !ghAutoMergeFlag.MatchString(segment) {
			events = append(events, ghMergeEvents(segment, output)...)
			break
		}
	}
	return events
}

// shellOutput unwraps a Codex shell result ({"output": …, "metadata":
// {"exit_code": N}}) and reports false when the output says the command
// exited non-zero, which a harness without an is_error flag records only
// there.
func shellOutput(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var wrapped struct {
			Output   *string `json:"output"`
			Metadata struct {
				ExitCode *float64 `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(trimmed), &wrapped) == nil && wrapped.Output != nil {
			if wrapped.Metadata.ExitCode != nil && *wrapped.Metadata.ExitCode != 0 {
				return "", false
			}
			text = *wrapped.Output
		}
	}
	for _, match := range exitCodeLine.FindAllStringSubmatch(text, -1) {
		if match[1] != "0" {
			return "", false
		}
	}
	return text, true
}

// commandSegments returns, for each match of pattern in command, the part
// from the match to the next command separator, so a flag is read only from
// the command it belongs to.
func commandSegments(command string, pattern *regexp.Regexp) []string {
	var segments []string
	for _, at := range pattern.FindAllStringIndex(command, -1) {
		rest := command[at[0]:]
		if end := commandSeparator.FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		segments = append(segments, rest)
	}
	return segments
}

// commitBranch reads the branch from git commit's "[<branch> <sha>]" line:
// "main", "main (root-commit)", or "detached HEAD" (no branch).
func commitBranch(label string) string {
	label = strings.TrimSuffix(label, " (root-commit)")
	if label == "detached HEAD" {
		return ""
	}
	return validBranch(label)
}

// pushEvents reads the pushes a command's git push calls made. Each push
// that reached a remote prints its own "To <remote>" block, so when some of
// the command's pushes are dry runs, blocks are paired with the pushes in
// order, and a dry run's block is skipped. A push with nothing to send
// prints no block, so when the counts differ that pairing cannot be
// trusted and nothing is recorded.
func pushEvents(command, output string) []GitEvent {
	segments := commandSegments(command, gitPushCommand)
	if len(segments) == 0 {
		return nil
	}
	dryRuns := 0
	for _, segment := range segments {
		if pushDryRunFlag.MatchString(segment) {
			dryRuns++
		}
	}
	blocks := pushBlocks(output)
	if dryRuns > 0 && (dryRuns == len(segments) || len(blocks) != len(segments)) {
		return nil
	}
	var events []GitEvent
	for i, block := range blocks {
		if dryRuns > 0 && pushDryRunFlag.MatchString(segments[i]) {
			continue
		}
		events = append(events, block...)
	}
	return events
}

// pushBlocks reads git push's report, one block per "To <remote>" line,
// each with one event per ref it updated or created. Rejected, deleted,
// and up-to-date refs are not events.
func pushBlocks(output string) [][]GitEvent {
	var blocks [][]GitEvent
	host, repository := "", ""
	// Ref lines count only under a push's "To <remote>" header: git fetch
	// and git pull print the same shapes under "From <remote>".
	pushing := false
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if match := pushRemoteLine.FindStringSubmatch(line); match != nil {
			host, repository = parseRemote(match[1])
			pushing = true
			blocks = append(blocks, nil)
			continue
		}
		if strings.HasPrefix(line, "From ") {
			pushing = false
			continue
		}
		if !pushing {
			continue
		}
		var sha, ref string
		if match := pushUpdateLine.FindStringSubmatch(line); match != nil {
			sha, ref = match[1], match[2]
		} else if match := pushNewBranchLine.FindStringSubmatch(line); match != nil {
			ref = match[1]
		} else {
			continue
		}
		event := GitEvent{Kind: GitEventPush, Source: GitEventSourceShell, SHA: sha, Branch: validBranch(ref), Repository: repository}
		if event.Branch != "" && repository != "" && publicHost(host) {
			event.URL = "https://" + host + "/" + repository + "/tree/" + escapeBranch(event.Branch)
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], event)
	}
	return blocks
}

// parseRemote reads a push remote (https://host/owner/repo.git,
// git@host:owner/repo.git, ssh://git@host/owner/repo) as a host and an
// owner/repo, from the last two path segments. A local path or a remote
// that does not parse yields neither.
func parseRemote(remote string) (host, repository string) {
	var path string
	if scheme, rest, ok := strings.Cut(remote, "://"); ok {
		// The filter rewrites a remote's userinfo as [REDACTED]@, which is
		// not a valid URL; userinfo is never kept, so drop it first.
		authority, _, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		parsed, err := url.Parse(scheme + "://" + rest)
		// A file:// remote, or any URL without a host, is a local path.
		if err != nil || strings.EqualFold(scheme, "file") || parsed.Host == "" {
			return "", ""
		}
		host, path = parsed.Hostname(), parsed.Path
	} else if at := strings.Index(remote, ":"); at > 0 && !strings.HasPrefix(remote, "/") && !strings.HasPrefix(remote, ".") {
		host, path = remote[:at], remote[at+1:]
		if user := strings.LastIndex(host, "@"); user >= 0 {
			host = host[user+1:]
		}
	} else {
		return "", ""
	}
	segments := strings.Split(strings.Trim(strings.TrimSuffix(strings.TrimRight(path, "/"), ".git"), "/"), "/")
	if len(segments) < 2 {
		return "", ""
	}
	owner, name := segments[len(segments)-2], segments[len(segments)-1]
	if !repoPartPattern.MatchString(owner) || !repoPartPattern.MatchString(name) {
		return "", ""
	}
	if !hostPattern.MatchString(host) {
		host = ""
	}
	return host, owner + "/" + name
}

// publicHost reports whether host is a DNS name a URL may be built on: not
// empty, not localhost, not an IP address (a local git proxy), and dotted.
func publicHost(host string) bool {
	if host == "" || !hostPattern.MatchString(host) || strings.EqualFold(host, "localhost") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	return strings.Contains(host, ".")
}

// escapeBranch path-escapes each segment of a validated branch name.
func escapeBranch(branch string) string {
	segments := strings.Split(branch, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// validBranch returns branch when it has the published shape, else "".
func validBranch(branch string) string {
	if branch == "" || len(branch) > maxBranchLength || !branchPattern.MatchString(branch) {
		return ""
	}
	return branch
}

// pullRequestEvent builds a pull request event from a parsed URL's parts,
// rebuilding the URL from them. False when a part is out of shape.
func pullRequestEvent(kind GitEventKind, host, owner, name, number string) (GitEvent, bool) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > maxPRNumber || !repoPartPattern.MatchString(owner) || !repoPartPattern.MatchString(name) {
		return GitEvent{}, false
	}
	event := GitEvent{Kind: kind, Repository: owner + "/" + name, PRNumber: n}
	if publicHost(host) {
		event.URL = "https://" + host + "/" + event.Repository + "/pull/" + strconv.Itoa(n)
	}
	return event, true
}

// ghHeadBranch is the branch gh pr create's --head (-H) flag names, without
// a fork owner's "owner:" prefix, or "" when the flag is absent or out of
// shape. Without it gh uses the checked-out branch.
func ghHeadBranch(segment string) string {
	match := ghHeadFlag.FindStringSubmatch(segment)
	if match == nil {
		return ""
	}
	head := match[1]
	if at := strings.LastIndex(head, ":"); at >= 0 {
		head = head[at+1:]
	}
	return validBranch(head)
}

// ghMergeEvents reads gh pr merge's confirmation. The repository comes from
// the confirmation when gh named it, else from the command's --repo flag or
// pull request URL argument; fillMergedFromCreated may supply it later. The
// host comes only from the command (a --repo HOST/OWNER/REPO or a pull
// request URL), since the confirmation never names one: gh may have found
// the repository on an enterprise host through the checkout's remote, so
// without one no URL is built.
func ghMergeEvents(segment, output string) []GitEvent {
	host, commandOwner, commandName := "", "", ""
	if flag := ghRepoFlag.FindStringSubmatch(segment); flag != nil {
		host, commandOwner, commandName = flag[1], flag[2], flag[3]
	} else if pr := pullURLAnywhere.FindStringSubmatch(segment); pr != nil {
		host, commandOwner, commandName = pr[1], pr[2], pr[3]
	}
	var events []GitEvent
	for _, match := range ghMergedLine.FindAllStringSubmatch(output, -1) {
		owner, name := match[1], match[2]
		if owner == "" {
			owner, name = commandOwner, commandName
		}
		if owner == "" {
			n, err := strconv.Atoi(match[3])
			if err == nil && n >= 1 && n <= maxPRNumber {
				events = append(events, GitEvent{Kind: GitEventPRMerged, Source: GitEventSourceShell, PRNumber: n})
			}
			continue
		}
		if event, ok := pullRequestEvent(GitEventPRMerged, host, owner, name, match[3]); ok {
			event.Source = GitEventSourceShell
			events = append(events, event)
		}
	}
	return events
}

// gitHubMCPTool names the GitHub MCP tools whose results mcpGitEvents reads.
type gitHubMCPTool string

// The GitHub MCP tools mcpGitEvents recognizes, by the tool part of an
// mcp__<server>__<tool> name.
const (
	mcpCreatePullRequest gitHubMCPTool = "create_pull_request"
	mcpMergePullRequest  gitHubMCPTool = "merge_pull_request"
	mcpPushFiles         gitHubMCPTool = "push_files"
	mcpCreateOrUpdate    gitHubMCPTool = "create_or_update_file"
)

// mcpGitEvents reads a GitHub MCP tool's result: create_pull_request,
// merge_pull_request, and the tools that commit on the server (push_files,
// create_or_update_file). Other tools, including enable_pr_auto_merge,
// yield nothing.
func mcpGitEvents(server, tool string, call NormalizedToolCall) []GitEvent {
	result := decodeObject(call.ResultText)
	owner, name := firstString(call.Input, "owner"), firstString(call.Input, "repo")
	githubServer := strings.Contains(strings.ToLower(server), "github")
	switch gitHubMCPTool(tool) {
	case mcpCreatePullRequest:
		event, ok := mcpPullRequest(GitEventPRCreated, call.ResultText, result, owner, name, numberText(result["number"]), githubServer)
		if !ok {
			return nil
		}
		event.Branch = validBranch(firstString(call.Input, "head"))
		return []GitEvent{event}
	case mcpMergePullRequest:
		// The merged flag decides when the result has one; a text result
		// must say the merge succeeded.
		merged, flagged := result["merged"].(bool)
		if !flagged {
			merged = strings.Contains(strings.ToLower(call.ResultText), "successfully merged")
		}
		if !merged {
			return nil
		}
		event, ok := mcpPullRequest(GitEventPRMerged, "", nil, owner, name, mcpPullNumber(call.Input), githubServer)
		if !ok {
			return nil
		}
		if sha := firstString(result, "sha"); shaPattern.MatchString(sha) {
			event.SHA = sha
		}
		return []GitEvent{event}
	case mcpPushFiles, mcpCreateOrUpdate:
		sha := nestedString(result, "commit", "sha")
		if sha == "" {
			sha = nestedString(result, "object", "sha")
		}
		if !shaPattern.MatchString(sha) {
			return nil
		}
		var repository string
		if repoPartPattern.MatchString(owner) && repoPartPattern.MatchString(name) {
			repository = owner + "/" + name
		}
		return []GitEvent{{Kind: GitEventCommit, Source: GitEventSourceMCP, SHA: sha, Branch: validBranch(firstString(call.Input, "branch")), Repository: repository}}
	}
	return nil
}

// mcpPullRequest builds a pull request event from a pull request URL in the
// result, or else from a number and the call's owner and repo; a URL is
// then built only for a GitHub server.
func mcpPullRequest(kind GitEventKind, text string, result map[string]any, owner, name, number string, githubServer bool) (GitEvent, bool) {
	for _, candidate := range []string{firstString(result, "html_url"), firstString(result, "url"), text} {
		if match := pullURLAnywhere.FindStringSubmatch(candidate); match != nil {
			event, ok := pullRequestEvent(kind, match[1], match[2], match[3], match[4])
			event.Source = GitEventSourceMCP
			return event, ok
		}
	}
	host := ""
	if githubServer {
		host = "github.com"
	}
	event, ok := pullRequestEvent(kind, host, owner, name, number)
	event.Source = GitEventSourceMCP
	return event, ok
}

// mcpPullNumber is a merge call's pull request number: the GitHub MCP
// server's pullNumber, or the reference server's pull_number.
func mcpPullNumber(input map[string]any) string {
	if number := numberText(input["pullNumber"]); number != "" {
		return number
	}
	return numberText(input["pull_number"])
}

// fillMergedFromCreated completes a merge event from the pull request the
// session created with the same number (and, when the merge names one, the
// same repository), when exactly one repository did: a merge that could not
// name its repository takes it, and one that could not name its host takes
// the created pull request's URL.
func fillMergedFromCreated(events []GitEvent) {
	for i := range events {
		if events[i].Kind != GitEventPRMerged || (events[i].Repository != "" && events[i].URL != "") {
			continue
		}
		var source *GitEvent
		for j := range events {
			if events[j].Kind == GitEventPRCreated && events[j].PRNumber == events[i].PRNumber && events[j].Repository != "" &&
				(events[i].Repository == "" || events[j].Repository == events[i].Repository) {
				if source != nil && source.Repository != events[j].Repository {
					source = nil
					break
				}
				source = &events[j]
			}
		}
		if source != nil {
			events[i].Repository, events[i].URL = source.Repository, source.URL
		}
	}
}

// decodeObject decodes text as a JSON object, or returns nil.
func decodeObject(text string) map[string]any {
	var out map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &out) != nil {
		return nil
	}
	return out
}

// nestedString returns object[outer][inner] when it is a string.
func nestedString(object map[string]any, outer, inner string) string {
	child, _ := object[outer].(map[string]any)
	value, _ := child[inner].(string)
	return value
}

// numberText renders a JSON number or numeric string argument as decimal
// text, or "" for anything else.
func numberText(value any) string {
	switch v := value.(type) {
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
	case string:
		return v
	}
	return ""
}

func deriveAnalyzedGitActivity(calls []NormalizedToolCall) ([]GitEvent, gitCounts) {
	var events []GitEvent
	for _, call := range calls {
		if call.ResultRecordIndex == nil || (call.IsError != nil && *call.IsError) {
			continue
		}
		name := callToolName(call)
		var found []GitEvent
		if server, tool, ok := mcpServerTool(name); ok {
			found = mcpGitEvents(server, tool, call)
		} else if command := shellCommandText(name, call); command != "" {
			found = shellGitEvents(command, call.ResultText, call.RecordedBranch)
		}
		at := call.ResultAt
		for i := range found {
			found[i].At = at
		}
		events = append(events, found...)
	}
	fillMergedFromCreated(events)
	var counts gitCounts
	for _, event := range events {
		counts.add(event.Kind)
	}
	if len(events) > MaxGitActivity {
		events = events[:MaxGitActivity]
	}
	return events, counts
}
