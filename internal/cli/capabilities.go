package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/versioninfo"
)

type capabilityEvidence = agentapi.CapabilityEvidence

type captureCapabilities = agentapi.CaptureCapabilities

const (
	capabilityDocumented       = agentapi.CapabilityDocumented
	capabilityFixtureValidated = agentapi.CapabilityFixtureValidated
	capabilityUnavailable      = agentapi.CapabilityUnavailable
	capabilityUnknown          = agentapi.CapabilityUnknown
)

type applicationDiscovery = agentapi.ApplicationDiscovery

// Reason codes for installed_version_support = unverified. They are an API.
const (
	supportReasonNoVerifiedCapture = "no_verified_capture"
	supportReasonNoMatchingVersion = "no_matching_verified_version"
	// supportReasonVersionSourceMismatch: the installed version and every
	// verified capture's version follow different numbering schemes (for
	// example a Cursor app-bundle version against the hook's cursor_version),
	// so they cannot match even when the same build produced both.
	supportReasonVersionSourceMismatch = "version_source_mismatch"
)

func applicationDiscoveriesPath(home string) string {
	return filepath.Join(home, "application-versions.json")
}

func recordApplicationDiscoveries(home string, discoveries map[string]applicationDiscovery, now time.Time) error {
	for name, discovery := range discoveries {
		discovery.ObservedAt = now.UTC()
		discoveries[name] = discovery
	}
	return local.Write(applicationDiscoveriesPath(home), discoveries)
}

func readApplicationDiscoveries(home string) (map[string]applicationDiscovery, error) {
	result := map[string]applicationDiscovery{}
	if err := local.Read(applicationDiscoveriesPath(home), &result); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}
	return result, nil
}

func captureCapabilityProfile(ports agentapi.CapabilityEvidenceLookup, name string) captureCapabilities {
	if p, ok := ports.LookupCapabilityEvidence(name); ok {
		return p.CaptureEvidence()
	}
	unknown := capabilityEvidence{State: capabilityUnknown, Evidence: "No capability contract is registered."}
	return captureCapabilities{FreshStart: unknown, Transcript: unknown, Lifecycle: unknown, SkillEvidence: unknown, SubagentLinkage: unknown, AdapterFixtures: unknown}
}

func discoverApplicationsWith(ports agentapi.VersionsLookup, userHome string, system platform.OS) map[string]applicationDiscovery {
	result := map[string]applicationDiscovery{}
	e := agentapi.VersionEnvironment{UserHome: userHome, MacOS: system == platform.Darwin, Host: versionHost{}}
	for _, name := range ports.VersionAgents() {
		p, _ := ports.LookupVersionInspector(name)
		result[name] = p.ObserveVersion(e)
	}
	return result
}

type versionHost struct{}

func (versionHost) Exists(path string) (bool, bool) {
	info, err := os.Stat(path)
	return err == nil, err == nil && info.IsDir()
}

func (versionHost) ResolveExecutable(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

func (versionHost) Directories(path string) []agentapi.VersionDirectory {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var out []agentapi.VersionDirectory
	for _, entry := range entries {
		out = append(out, agentapi.VersionDirectory{Name: entry.Name(), Directory: entry.IsDir()})
	}
	return out
}

func (versionHost) ProbeVersion(p agentapi.VersionProbe) (string, bool) {
	switch p.Kind {
	case agentapi.VersionCLI:
		return boundedVersionCommand(p.Path, "--version")
	case agentapi.VersionMuseCLI:
		return runBoundedVersionCommand(true, p.Path, "--version")
	case agentapi.VersionBundle:
		return boundedVersionCommand("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", p.Path)
	}
	return "", false
}

func boundedVersionCommand(argv ...string) (string, bool) {
	return runBoundedVersionCommand(false, argv...)
}

func runBoundedVersionCommand(disableMuseUpdates bool, argv ...string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 250 * time.Millisecond
	if disableMuseUpdates {
		cmd.Env = slices.DeleteFunc(cmd.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "MUSE_NO_AUTO_UPDATE=") })
		cmd.Env = append(cmd.Env, "MUSE_NO_AUTO_UPDATE=1")
	}
	var output cappedBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	value := strings.TrimSpace(output.String())
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	return value, true
}

// installedVersionSupportDetail reports the support state and, when it is
// unverified, a reason code saying why the installed version is not matched by
// a verified capture.
func installedVersionSupportDetail(discovery applicationDiscovery, verifiedVersions []string) (string, string) {
	if !discovery.Installed {
		if discovery.VersionState != "absent" {
			return "unknown", ""
		}
		return "absent", ""
	}
	if discovery.Version == "" || discovery.VersionState == "stale" {
		return "unknown", ""
	}
	installed := normalizedVersion(discovery.Version)
	if len(verifiedVersions) == 0 {
		return "unverified", supportReasonNoVerifiedCapture
	}
	shapesMatch := false
	for _, version := range verifiedVersions {
		verified := normalizedVersion(version)
		if installed != "" && installed == verified {
			return "verified_by_capture", ""
		}
		if versionShape(installed) == versionShape(verified) {
			shapesMatch = true
		}
	}
	if !shapesMatch {
		return "unverified", supportReasonVersionSourceMismatch
	}
	return "unverified", supportReasonNoMatchingVersion
}

func normalizedVersion(value string) string { return versioninfo.Normalize(value) }

func versionShape(value string) string { return versioninfo.Shape(value) }

type cappedBuffer struct{ bytes.Buffer }

// ReadFrom must use the capped Write rather than bytes.Buffer.ReadFrom, which
// would bypass the bound when os/exec copies the process output.
func (b *cappedBuffer) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{b}, r)
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	const limit = 4096
	remaining := limit - b.Len()
	if remaining <= 0 {
		return 0, errors.New("version output exceeded limit")
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		return remaining, errors.New("version output exceeded limit")
	}
	return b.Buffer.Write(p)
}
