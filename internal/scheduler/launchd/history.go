package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// The macOS history of this tool's jobs lives here and nowhere else: the
// labels earlier releases gave the collector, and the prototype's upload job.
// Shared code asks Installed which jobs an installation has, and never
// compares labels or paths itself.

// LegacyLaunchLabel is the launchd label of the prototype's upload job,
// which the default installation's setup retires.
const LegacyLaunchLabel = "com.agent-skills.skill-runs-upload"

// prototypeFix is what to do about a prototype job launchd runs from another
// plist, or one whose plist is not the prototype's.
const prototypeFix = "preserve it and resolve it before setup"

// Installed is the jobs of inst under launchd, own first (whether or not its
// plist exists): then, for the account's default installation, the
// prototype's upload job when its plist is there (an error, and no job, when
// it is not the prototype's own plist: only its exact label and command shape
// establish ownership, and nothing here executes it), and then every
// collector plist an earlier release installed for this data directory under
// another label, in the order the LaunchAgents folder lists them.
//
// Earlier releases used two other labels. The default one, which releases
// before labels were derived from the directory gave a non-default data
// directory. And the label derived from the directory as spelled (its
// symlinks resolved but not its case), which releases before CanonicalPath
// gave a directory spelled in another case than it is listed in, the default
// directory included; setup run with two such spellings left one job for each.
// A plist for any other directory is never returned, so another
// installation's is never touched, and stopping the job one defines still
// needs launchd to have loaded it from that very file (Unload).
func (s Scheduler) Installed(_ context.Context, site scheduler.Site, inst scheduler.Installation) ([]scheduler.Job, error) {
	own := s.Ref(inst)
	jobs := []scheduler.Job{{Ref: own}}
	if inst.Role == scheduler.Search {
		return jobs, nil
	}
	var blocked error
	if inst.Default {
		found, err := prototypeJob(site)
		if found {
			jobs = append(jobs, scheduler.Job{Ref: LegacyLaunchLabel, Alias: scheduler.Prototype})
		}
		blocked = err
	}
	return append(jobs, earlierLabels(site, inst, own)...), blocked
}

// earlierLabels are the collectors earlier releases installed for inst's data
// directory under other labels than own.
func earlierLabels(site scheduler.Site, inst scheduler.Installation, own scheduler.Ref) []scheduler.Job {
	dir := filepath.Dir(PlistPath(site, own))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []scheduler.Job
	for _, entry := range entries {
		label, ok := strings.CutSuffix(entry.Name(), ".plist")
		if !ok || scheduler.Ref(label) == own || !isCollectorLabel(label) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		dataHome, err := LaunchAgentDataHome(data)
		if err != nil || dataHome == "" || !local.SameLocation(dataHome, inst.DataHome) {
			continue
		}
		found = append(found, scheduler.Job{Ref: scheduler.Ref(label), Alias: scheduler.EarlierLabel})
	}
	return found
}

// isCollectorLabel reports whether label is one CollectorLabel produces: the
// default label, or it followed by 12 hex digits.
func isCollectorLabel(label string) bool {
	if label == LaunchLabel {
		return true
	}
	suffix, ok := strings.CutPrefix(label, LaunchLabel+".")
	return ok && len(suffix) == 12 && strings.Trim(suffix, "0123456789abcdef") == ""
}

// prototypeJob reports whether the prototype's upload job has a plist at
// site, and an error when it has one that is not the prototype's own. Do not
// execute the plist or remove the private records referenced by --home.
func prototypeJob(site scheduler.Site) (found bool, err error) {
	path := PlistPath(site, LegacyLaunchLabel)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var key, label string
	var args []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("cannot verify legacy upload job ownership: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		//lint:ignore LV1001 XML element names from a launchd plist, an external format
		switch start.Name.Local {
		case "key":
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return false, err
			}
		case "string":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return false, err
			}
			if key == "Label" {
				label = value
			}
			key = ""
		case "array":
			if key == "ProgramArguments" {
				var a struct {
					Values []string `xml:"string"`
				}
				if err := decoder.DecodeElement(&a, &start); err != nil {
					return false, err
				}
				args = a.Values
				key = ""
			}
		}
	}
	if label != LegacyLaunchLabel || len(args) != 5 || filepath.Base(args[1]) != "skill_runs.py" || args[2] != "--home" || args[3] == "" || args[4] != "upload" {
		return false, fmt.Errorf("legacy job path contains an unrecognized command; preserve %s and resolve it before setup", path)
	}
	return true, nil
}
