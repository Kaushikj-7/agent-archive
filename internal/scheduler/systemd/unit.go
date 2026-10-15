package systemd

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// The unit files are written and read here and nowhere else. A unit file is
// the INI-like syntax of systemd.syntax(7), and three of its rules decide
// what a value must look like to survive a round trip:
//
//   - a `%` in any value starts a specifier (%h, %t...), so it is written
//     doubled (`%%`);
//   - ExecStart= expands `$VAR` and `${VAR}` in its arguments, so a `$` there
//     is written doubled (`$$`); Environment= does not expand ("the `$`
//     character has no special meaning", systemd.exec(5)). The program's
//     path is not expanded but the argv[0] made from it is ("the program to
//     execute may not be a variable", systemd.service(5)), so a program
//     whose path has a `$` is refused rather than written in a way that
//     does not run what it names;
//   - Environment= and ExecStart= split their value into words on white
//     space, honoring single and double quotes and C-style backslash
//     escapes, so a word with anything but the plainest characters is
//     written in double quotes, with `\` and `"` escaped. A program's path
//     is the exception: systemd refuses one with a quote or a backslash in
//     it, however it is written, and from v261 one with a `*`, `?` or `[`
//     too ("Executable name contains special characters", "Executable path
//     contains special characters" from v261; string_is_safe in
//     load-fragment.c), so Plan refuses all of them: a unit must keep
//     working when the system's systemd is upgraded.
//
// StandardOutput=append: takes the rest of the line as a path, with only its
// specifiers expanded, so a path is written as it is with `%` doubled.
// Control characters and text that is not valid UTF-8 are refused (a unit
// file is UTF-8, one setting to a line); a collector has no use for them.

// unsafeInProgram are the characters systemd refuses in a program's path
// (string_is_safe: the quotes and the backslash, and from v261 the glob
// characters; control characters are refused with the rest of the text).
const unsafeInProgram = `"'\*?[`

// reinstall is what to do about a program path systemd cannot run. The path
// is where agent-archive is installed, which install.sh makes ~/.local/bin
// when /usr/local/bin is not writable, so a home whose name has such a
// character in it (/home/o'brien) needs another directory; the data
// directory can stay where it is.
const reinstall = "install agent-archive in a directory whose path has no such character (install.sh installs into AGENT_ARCHIVE_INSTALL_DIR when it is set, /usr/local/bin for example) and run setup from that copy"

// renderService is the collector's service unit: a oneshot that runs the
// executable with `_collect`, with AGENT_ARCHIVE_HOME and environment, and
// logs appended to the data directory's collector.log and collector-error.log.
// It runs from /, as launchd does and credential helper diagnostics expect,
// rather than the user's home (systemd's default for a user service).
// There is no PrivateTmp or other sandboxing: the collector reads the
// user's own agent files.
func renderService(executable, dataHome string, environment map[string]string) ([]byte, error) {
	return renderRoleService(executable, dataHome, environment, scheduler.Collector)
}

func renderRoleService(executable, dataHome string, environment map[string]string, role scheduler.Role) ([]byte, error) {
	command, err := role.Command()
	if err != nil {
		return nil, err
	}
	name := "collector"
	if role == scheduler.Search {
		name = "search"
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(dataHome) {
		return nil, errors.New("a systemd unit's paths must be absolute")
	}
	if strings.Contains(executable, "$") {
		return nil, fmt.Errorf("a systemd unit cannot run %q: a `$` in the program's path is expanded in ways that cannot be relied on; %s", executable, reinstall)
	}
	if strings.ContainsAny(executable, unsafeInProgram) {
		return nil, fmt.Errorf("a systemd unit cannot run %q: systemd refuses a program whose path has a quote, a backslash, `*`, `?` or `[` in it; %s", executable, reinstall)
	}
	texts := []string{executable, dataHome}
	var env strings.Builder
	env.WriteString("Environment=" + word("AGENT_ARCHIVE_HOME="+dataHome, false) + "\n")
	for _, name := range slices.Sorted(maps.Keys(environment)) {
		if !validName(name) || name == "AGENT_ARCHIVE_HOME" {
			return nil, fmt.Errorf("a systemd unit cannot set %q", name)
		}
		texts = append(texts, environment[name])
		env.WriteString("Environment=" + word(name+"="+environment[name], false) + "\n")
	}
	for _, text := range texts {
		if !validText(text) {
			return nil, fmt.Errorf("a systemd unit cannot hold %q: control characters and text that is not UTF-8 have no place in a unit file", text)
		}
	}
	return []byte(`# Written by agent-archive setup; setup and setup --refresh rewrite this file.
[Unit]
Description=agent-archive background ` + name + `

[Service]
Type=oneshot
WorkingDirectory=/
ExecStart=` + word(executable, true) + ` ` + command + `
` + env.String() + `StandardOutput=append:` + percent(filepath.Join(dataHome, name+".log")) + `
StandardError=append:` + percent(filepath.Join(dataHome, name+"-error.log")) + "\n"), nil
}

// renderTimer is the collector's timer unit: it starts the service a minute
// after boot, which is at once when the timer is started later (the moment
// has passed), and then a minute after each start. OnUnitActiveSec alone
// never fires until the service has run once, and there is no Persistent=:
// a missed run is not made up, the next one is a minute away.
func renderTimer(ref string) []byte {
	return renderRoleTimer(ref, scheduler.Collector)
}

func renderRoleTimer(ref string, role scheduler.Role) []byte {
	name := "collector"
	if role == scheduler.Search {
		name = "search"
	}
	return []byte(`# Written by agent-archive setup; setup and setup --refresh rewrite this file.
[Unit]
Description=agent-archive background ` + name + ` schedule

[Timer]
OnBootSec=60s
OnUnitActiveSec=60s
AccuracySec=1s
Unit=` + ref + `.service

[Install]
WantedBy=timers.target
`)
}

// validName is what systemd accepts as an environment variable's name.
func validName(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, r := range name {
		if r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// validText reports whether s can be written to a unit file and read back:
// valid UTF-8 without control characters or the non-characters systemd
// rejects.
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0xfdd0 && r <= 0xfdef) || r&0xfffe == 0xfffe {
			return false
		}
	}
	return true
}

// percent doubles the `%` of a specifier.
func percent(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// word writes s as one word of ExecStart= (arguments true: `$` doubled too)
// or Environment=: bare when it holds only characters no rule reads, else in
// double quotes with `\` and `"` escaped.
func word(s string, arguments bool) string {
	quoted := s == ""
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '%':
			b.WriteString("%%")
		case r == '$' && arguments:
			b.WriteString("$$")
			quoted = true
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
			quoted = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:@+,=", r):
			b.WriteRune(r)
		default:
			b.WriteRune(r)
			quoted = true
		}
	}
	if quoted {
		return `"` + b.String() + `"`
	}
	return b.String()
}

// readService reads the program and the environment a service unit sets: the
// first word of its ExecStart= and the Environment= assignments of its
// [Service] section, later ones over earlier, an empty one clearing what came
// before, as systemd does. It reads any unit, not only one this package
// wrote: white space, comments, continued lines, quotes and escapes are
// honored, and a setting it does not use is skipped. The environment is
// empty, not nil, when the unit sets none.
func readService(data []byte) (program string, environment map[string]string, err error) {
	environment = map[string]string{}
	var commands []string
	inService := false
	for _, line := range logicalLines(string(data)) {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			continue
		}
		if line[0] == '[' {
			inService = strings.TrimSpace(line) == "[Service]"
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !inService {
			continue
		}
		value = strings.Trim(value, " \t")
		switch strings.TrimSpace(key) {
		case "ExecStart":
			if value == "" {
				commands = nil
			} else {
				commands = append(commands, value)
			}
		case "Environment":
			if value == "" {
				clear(environment)
				continue
			}
			words, err := splitWords(value)
			if err != nil {
				return "", nil, fmt.Errorf("read Environment=%s: %w", value, err)
			}
			for _, w := range words {
				if name, val, ok := strings.Cut(w, "="); ok && name != "" {
					environment[name] = val
				}
			}
		}
	}
	if len(commands) == 0 {
		return "", nil, errors.New("the unit has no ExecStart= in its [Service] section")
	}
	words, err := splitWords(commands[0])
	if err != nil {
		return "", nil, fmt.Errorf("read ExecStart=%s: %w", commands[0], err)
	}
	if len(words) > 0 {
		program = strings.TrimLeft(words[0], "@-:+!")
	}
	if program == "" {
		return "", nil, errors.New("the unit's ExecStart= names no program")
	}
	return program, environment, nil
}

// logicalLines are the lines of a unit file with each line that ends in an
// unescaped backslash joined to the next by a space, as systemd reads them.
// A comment line is dropped before lines are joined, so a comment between a
// line and its continuation is skipped, not joined (systemd.syntax(7)), and
// an empty line ends a continued one. That is config_parse in conf-parser.c
// from v243. Before it, v240 to v242 skipped a blank line as if it were a
// comment, so it did not end a continued line, and v240 took a comment only
// in the first column; that differs only for a unit continued past one.
func logicalLines(text string) []string {
	var lines []string
	var pending string
	for raw := range strings.SplitSeq(text, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		if trimmed := strings.TrimLeft(raw, " \t"); trimmed != "" && (trimmed[0] == '#' || trimmed[0] == ';') {
			continue
		}
		if (len(raw)-len(strings.TrimRight(raw, `\`)))%2 == 1 {
			pending += raw[:len(raw)-1] + " "
			continue
		}
		lines = append(lines, pending+raw)
		pending = ""
	}
	if pending != "" {
		lines = append(lines, pending)
	}
	return lines
}

// escapes are the C-style escapes a value may hold, which splitWords
// resolves; any other backslash sequence is an error rather than a guess.
var escapes = map[rune]string{'\\': `\`, '"': `"`, '\'': `'`, 'n': "\n", 't': "\t", 'r': "\r", 's': " ", 'a': "\a", 'b': "\b", 'f': "\f", 'v': "\v"}

// splitWords splits a setting's value into words the way systemd does for
// ExecStart= and Environment=: on white space outside quotes, with single and
// double quotes removed and C-style escapes resolved, and the `%%` of a
// specifier read as `%` (any other specifier is left as written, since it
// cannot be resolved here).
func splitWords(value string) ([]string, error) {
	var words []string
	var current strings.Builder
	var quote rune
	inWord := false
	endWord := func() {
		if inWord {
			words = append(words, strings.ReplaceAll(current.String(), "%%", "%"))
			current.Reset()
			inWord = false
		}
	}
	escaped := false
	for _, r := range value {
		switch {
		case escaped:
			decoded, ok := escapes[r]
			if !ok {
				return nil, fmt.Errorf(`the escape \%c is not one this reader knows`, r)
			}
			current.WriteString(decoded)
			escaped = false
		case r == '\\':
			escaped, inWord = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, inWord = r, true
		case quote == 0 && (r == ' ' || r == '\t'):
			endWord()
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("the value ends inside a quote or an escape")
	}
	endWord()
	return words, nil
}
