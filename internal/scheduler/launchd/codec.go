// Package launchd is the macOS scheduler: launchd, driven through launchctl.
// It holds everything that knows launchd's vocabulary: the LaunchAgent plist
// (rendered and read back here), the labels of this tool's jobs, and the
// launchctl calls that ask about, load and stop a job.
package launchd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// LaunchLabel is the background collector's launchd label for the default
// data directory.
const LaunchLabel = "com.agent-archive.collector"

// CollectorLabel is the launchd label of the collector for dataHome when it
// is not the account's default installation, which keeps LaunchLabel so an
// existing installation keeps its job. The label is derived from the
// directory: launchd labels are global to the login session, unlike HOME,
// so a test or secondary installation needs one of its own. defaultDataHome
// may be "" when the caller has already decided dataHome is not the default.
func CollectorLabel(dataHome, defaultDataHome string) string {
	if defaultDataHome != "" && filepath.Clean(dataHome) == filepath.Clean(defaultDataHome) {
		return LaunchLabel
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dataHome)))
	return LaunchLabel + "." + hex.EncodeToString(sum[:])[:12]
}

// LaunchAgent is the collector's plist; label is its CollectorLabel.
// environment holds variables the collector runs with besides
// AGENT_ARCHIVE_HOME, which is always dataHome; launchd gives a job nothing
// of the shell's environment, so what setup relied on must be written here.
func LaunchAgent(executable, dataHome, label string, environment map[string]string) ([]byte, error) {
	return launchAgent(executable, dataHome, label, environment, scheduler.Collector)
}

func launchAgent(executable, dataHome, label string, environment map[string]string, role scheduler.Role) ([]byte, error) {
	command, err := role.Command()
	if err != nil {
		return nil, err
	}
	logName := "collector"
	if role == scheduler.Search {
		logName = "search"
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(dataHome) {
		return nil, errors.New("LaunchAgent paths must be absolute")
	}
	escape := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails to write
		return b.String()
	}
	var variables strings.Builder
	variables.WriteString("<key>AGENT_ARCHIVE_HOME</key><string>" + escape(dataHome) + "</string>")
	for _, name := range slices.Sorted(maps.Keys(environment)) {
		if name == "" || name == "AGENT_ARCHIVE_HOME" {
			return nil, fmt.Errorf("LaunchAgent cannot set %q", name)
		}
		variables.WriteString("<key>" + escape(name) + "</key><string>" + escape(environment[name]) + "</string>")
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>%s</string></array>
<key>EnvironmentVariables</key><dict>%s</dict>
<key>RunAtLoad</key><true/><key>StartInterval</key><integer>60</integer>
<key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, escape(label), escape(executable), command, variables.String(), escape(filepath.Join(dataHome, logName+".log")), escape(filepath.Join(dataHome, logName+"-error.log")))), nil
}

// LaunchAgentDataHome returns the AGENT_ARCHIVE_HOME a LaunchAgent plist
// sets, or "" when it sets none.
func LaunchAgentDataHome(plist []byte) (string, error) {
	environment, err := LaunchAgentEnvironment(plist)
	if err != nil {
		return "", err
	}
	return environment["AGENT_ARCHIVE_HOME"], nil
}

// LaunchAgentEnvironment returns the EnvironmentVariables a LaunchAgent
// plist sets: the only environment launchd gives the job besides its own
// defaults. It is empty, not nil, for a plist that sets none.
func LaunchAgentEnvironment(plist []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	decoder.Strict = false
	var (
		lastKey  string
		depth    int
		envDepth = -1
		reading  bool
		text     strings.Builder
	)
	environment := map[string]string{}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return environment, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read LaunchAgent: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key", "string":
				reading = true
				text.Reset()
			case "dict":
				if lastKey == "EnvironmentVariables" && envDepth < 0 {
					envDepth = depth
				}
			}
		case xml.CharData:
			if reading {
				text.Write(t)
			}
		case xml.EndElement:
			reading = false
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				lastKey = strings.TrimSpace(text.String())
			case "string":
				if envDepth >= 0 && depth == envDepth+1 && lastKey != "" {
					if _, seen := environment[lastKey]; !seen {
						environment[lastKey] = strings.TrimSpace(text.String())
					}
				}
			case "dict":
				if depth == envDepth {
					envDepth = -1
				}
			}
			depth--
		}
	}
}

// LaunchAgentProgram returns the executable a LaunchAgent plist runs: the
// first ProgramArguments string. It reads any well-formed plist, not only one
// LaunchAgent wrote, and reports an error when there is no program to read.
func LaunchAgentProgram(plist []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	decoder.Strict = false
	var (
		lastKey      string
		inArguments  bool
		readingKey   bool
		readingValue bool
		text         strings.Builder
	)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", errors.New("LaunchAgent has no ProgramArguments")
		}
		if err != nil {
			return "", fmt.Errorf("read LaunchAgent: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				readingKey = true
				text.Reset()
			case "array":
				inArguments = lastKey == "ProgramArguments"
			case "string":
				readingValue = inArguments
				text.Reset()
			}
		case xml.CharData:
			if readingKey || readingValue {
				text.Write(t)
			}
		case xml.EndElement:
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				readingKey = false
				lastKey = strings.TrimSpace(text.String())
			case "string":
				if readingValue {
					if program := strings.TrimSpace(text.String()); program != "" {
						return program, nil
					}
					return "", errors.New("LaunchAgent program is empty")
				}
			case "array":
				if inArguments {
					return "", errors.New("LaunchAgent ProgramArguments is empty")
				}
			}
		}
	}
}
