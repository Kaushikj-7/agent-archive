package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// mountTableWith is a mount table (the format of /proc/self/mountinfo) of a
// machine whose root is a local disk and on which each of the paths is
// mounted as the filesystem type it maps to. A path is resolved first, as
// the check resolves what it looks at, so a temporary directory that is
// itself behind a link is the one that is on the share.
func mountTableWith(t *testing.T, mounts map[string]string) func() ([]byte, error) {
	t.Helper()
	var table strings.Builder
	table.WriteString("1 0 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n")
	id := 2
	for path, fstype := range mounts {
		target, err := filepath.EvalSymlinks(path)
		must(t, err)
		fmt.Fprintf(&table, "%d 1 0:%d / %s rw,relatime - %s server:/export rw\n", id, id, strings.ReplaceAll(target, " ", `\040`), fstype)
		id++
	}
	return func() ([]byte, error) { return []byte(table.String()), nil }
}

// resolved is path with its links resolved, as the mount table names it.
func resolved(t *testing.T, path string) string {
	t.Helper()
	target, err := filepath.EvalSymlinks(path)
	must(t, err)
	return target
}

// flat is text on one line, so a phrase is found wherever status wrapped it.
func flat(text string) string { return strings.Join(strings.Fields(text), " ") }

// Setup on a data directory that is on a network filesystem stops before its
// first question and changes nothing: no lock, no data directory made, no
// hooks, configuration, unit files or journal. The refusal names the
// directory and the filesystem, why that matters, and both fixes.
func TestLinuxSetupRefusesADataDirectoryOnANetworkFilesystem(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	share := t.TempDir()
	l.home = filepath.Join(share, "not", "made", "yet")
	l.env.Home = func() (string, error) { return l.home, nil }
	l.env.MountTable = mountTableWith(t, map[string]string{share: "nfs4"})

	output := setupRun(t, l.env, l.setupInput(), 1)
	point := resolved(t, share)
	want := "  ✗ Data directory: " + l.home + " is on a network filesystem (nfs4, mounted at " + point + ")\n" +
		"    Machines that share this directory would share one machine ID and claim the same sessions; file locks are not reliable over a network filesystem, so concurrent collectors can corrupt its state; and every machine's background job would run against the same files.\n" +
		"    Keep the data directory on local disk: set AGENT_ARCHIVE_HOME to a path there and run agent-archive setup again. If only one machine ever mounts this directory, run agent-archive setup --allow-network-home instead.\n" +
		"Setup incomplete. Nothing was changed, and any unfinished setup is kept. Fix what is marked ✗ above, then run agent-archive setup again.\n"
	// Setup wraps long lines to the terminal; compare the words.
	if flat(output) != flat(want) {
		t.Errorf("the refusal is:\n%s\nwant:\n%s", output, want)
	}
	if _, err := os.Stat(l.home); !os.IsNotExist(err) {
		t.Errorf("setup made the data directory it refused (%v)", err)
	}
	l.nothingChanged()
}

// The same from a script: setup --yes refuses the same way, with the check
// named on standard error for a log nobody sees the checklist of, and says to
// run the same command again.
func TestLinuxSetupYesRefusesANetworkFilesystemAndChangesNothing(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.MountTable = mountTableWith(t, map[string]string{l.home: "cifs"})
	output := setupYes(t, l.env, "", 1, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--region", "us-east-1", "--project", t.TempDir(), "--apps", "claude")
	for _, want := range []string{
		"Data directory: " + l.home + " is on a network filesystem (cifs, mounted at " + resolved(t, l.home) + ")",
		"Setup incomplete: Data directory: " + l.home + " is on a network filesystem (cifs, mounted at " + resolved(t, l.home) + "): Machines that share this directory",
		"run agent-archive setup --allow-network-home instead",
		"Nothing was changed. Fix what is marked ✗ above, then run the same agent-archive setup --yes command again.",
	} {
		if !strings.Contains(flat(output), want) {
			t.Errorf("setup --yes lacks %q:\n%s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(l.home, "setup.lock")); !os.IsNotExist(err) {
		t.Errorf("setup --yes locked the directory it refused (%v)", err)
	}
	l.nothingChanged()
}

// The unit directory is checked on its own, through the nearest directory
// above it that exists (~/.config/systemd/user is not made yet), and moving
// the data directory does not answer it: the refusal says so.
func TestLinuxSetupRefusesAUnitDirectoryOnANetworkFilesystem(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.MountTable = mountTableWith(t, map[string]string{l.userHome: "nfs"})
	if _, err := os.Stat(filepath.Join(l.userHome, ".config")); !os.IsNotExist(err) {
		t.Fatalf("the home already has a .config (%v)", err)
	}
	output := setupRun(t, l.env, l.setupInput(), 1)
	point := resolved(t, l.userHome)
	for _, want := range []string{
		"✗ Systemd unit directory: ~/.config/systemd/user is on a network filesystem (nfs, mounted at " + point + ")",
		"Every machine that mounts this home directory loads the units in it, so each of them would run the collector.",
		"AGENT_ARCHIVE_HOME does not move the unit directory: it is under your home directory, where systemd looks for user units.",
		"agent-archive setup --allow-network-home.",
		"Nothing was changed",
	} {
		if !strings.Contains(flat(output), want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Data directory") {
		t.Errorf("the refusal names a data directory that is on local disk:\n%s", output)
	}
	l.nothingChanged()
}

// A home shared as a whole puts both on the share, and both are named.
func TestLinuxSetupNamesBothDirectoriesOnANetworkFilesystem(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.MountTable = mountTableWith(t, map[string]string{l.userHome: "nfs4"})
	l.home = filepath.Join(l.userHome, ".local", "share", "agent-archive")
	l.env.Home = func() (string, error) { return l.home, nil }
	output := flat(setupRun(t, l.env, l.setupInput(), 1))
	for _, want := range []string{"✗ Data directory: ~/.local/share/agent-archive is on a network filesystem (nfs4", "✗ Systemd unit directory: ~/.config/systemd/user is on a network filesystem (nfs4"} {
		if !strings.Contains(output, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, output)
		}
	}
	l.nothingChanged()
}

// Network filesystems only: a directory on any other filesystem is set up as
// ever, and so is one the mount table cannot place.
func TestLinuxSetupGoesAheadOffANetworkFilesystem(t *testing.T) {
	t.Parallel()
	for name, mount := range map[string]func(l *linuxInstall) func() ([]byte, error){
		"an ordinary local filesystem": func(l *linuxInstall) func() ([]byte, error) {
			table := mountTableWith(t, map[string]string{l.home: "ext4"})
			return table
		},
		"a filesystem of this machine seen another way": func(l *linuxInstall) func() ([]byte, error) {
			return mountTableWith(t, map[string]string{l.home: "virtiofs", l.userHome: "fuse.gocryptfs"})
		},
		"a mount table that cannot be read": func(*linuxInstall) func() ([]byte, error) {
			return func() ([]byte, error) { return nil, errors.New("permission denied") }
		},
		"a mount table with no line for the directory": func(*linuxInstall) func() ([]byte, error) {
			return func() ([]byte, error) { return []byte("22 1 0:21 / /elsewhere rw - nfs4 s:/e rw\n"), nil }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := newLinuxInstall(t)
			l.env.MountTable = mount(l)
			output := setupRun(t, l.env, l.setupInput(), 0)
			if strings.Contains(output, "network filesystem") {
				t.Errorf("setup says of a local home:\n%s", output)
			}
			if _, ok := l.rawConfig()["allow_network_home"]; ok {
				t.Error("an opt-in nobody needed was recorded")
			}
		})
	}
}

// The opt-in lets setup through, says on the checklist that it did so, and is
// recorded, so setup --refresh and a later setup do not refuse again. Status
// keeps warning, in both outputs, that the home is on a network filesystem.
func TestLinuxSetupAllowNetworkHomeIsRecordedAndStatusStillWarns(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.MountTable = mountTableWith(t, map[string]string{l.userHome: "nfs4"})
	l.home = filepath.Join(l.userHome, "data")
	l.env.Home = func() (string, error) { return l.home, nil }

	out, errOut := setupRunWith(t, l.env, []string{"--allow-network-home"}, l.setupInput(), 0)
	output := flat(out + errOut)
	for _, want := range []string{
		"✓ Data directory: ~/data is on a network filesystem (nfs4, mounted at " + resolved(t, l.userHome) + "), which you allowed with --allow-network-home; it must stay on one machine",
		"✓ Systemd unit directory: ~/.config/systemd/user is on a network filesystem (nfs4",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("setup does not say it was allowed: lacks %q:\n%s", want, output)
		}
	}
	if got := string(l.rawConfig()["allow_network_home"]); got != "true" {
		t.Errorf("config.json has allow_network_home %q", got)
	}
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("setup with the opt-in left no unit file: %v", err)
		}
	}

	// Recorded, so nothing asks for it again: refresh, a rerun of setup --yes.
	if code := Run([]string{"setup", "--refresh"}, nil, new(strings.Builder), new(strings.Builder), l.env); code != 0 {
		t.Errorf("setup --refresh refused a home whose opt-in was recorded (exit %d)", code)
	}
	if out := setupYes(t, l.env, "", 0, "--yes"); !strings.Contains(flat(out), "which you allowed with --allow-network-home") {
		t.Errorf("setup --yes without the flag does not say the saved opt-in applies:\n%s", out)
	}
	if got := string(l.rawConfig()["allow_network_home"]); got != "true" {
		t.Errorf("a rerun forgot the opt-in: %q", got)
	}

	wantWarning := "is on a network filesystem (nfs4, mounted at " + resolved(t, l.userHome) + "). It is safe only while one machine uses this home."
	var found int
	for _, w := range statusWarnings(t, l.env) {
		if strings.Contains(w, wantWarning) {
			found++
			if !strings.Contains(w, "You allowed this with agent-archive setup --allow-network-home.") || strings.Contains(w, "refuses") {
				t.Errorf("an allowed home's warning is %q", w)
			}
		}
	}
	if found != 2 {
		t.Errorf("status --json has %d warnings of the network filesystem, want 2 (data and unit directory)", found)
	}
	if human := flat(statusOutput(t, l.env, "--verbose")); !strings.Contains(human, "The data directory ~/data is on a network filesystem (nfs4") || !strings.Contains(human, "The systemd unit directory ~/.config/systemd/user is on a network filesystem") {
		t.Errorf("status says in words:\n%s", human)
	}
}

// The same opt-in from a script, and a second machine's view: the same
// installation on a home that is not on a network filesystem records none.
func TestLinuxSetupYesAllowNetworkHome(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.env.MountTable = mountTableWith(t, map[string]string{l.home: "fuse.sshfs"})
	setupYes(t, l.env, "", 0, "--yes", "--allow-network-home", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--region", "us-east-1", "--project", t.TempDir(), "--apps", "claude")
	if got := string(l.rawConfig()["allow_network_home"]); got != "true" {
		t.Errorf("config.json has allow_network_home %q", got)
	}
	// The data directory moves to local disk: setup drops the opt-in it no
	// longer needs, so a later move onto a network filesystem is refused again.
	l.env.MountTable = mountTableWith(t, nil)
	setupYes(t, l.env, "", 0, "--yes")
	if _, ok := l.rawConfig()["allow_network_home"]; ok {
		t.Error("an opt-in that is no longer needed is still recorded")
	}
	l.env.MountTable = mountTableWith(t, map[string]string{l.home: "fuse.sshfs"})
	setupYes(t, l.env, "", 1, "--yes")
}

// An installation made before this check, on a network home, is not refused
// quietly: setup --refresh stops and names the way through, status warns and
// says what setup now does, and nothing is changed.
func TestLinuxRefreshRefusesAnInstallationOnANetworkFilesystemThatWasNeverAllowed(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.env.MountTable = mountTableWith(t, map[string]string{l.home: "nfs4"})
	before := rawFiles(t, l)

	var out, errOut strings.Builder
	if code := Run([]string{"setup", "--refresh"}, nil, &out, &errOut, l.env); code != 1 {
		t.Fatalf("setup --refresh exit %d, want 1\n%s%s", code, &out, &errOut)
	}
	for _, want := range []string{
		"agent-archive: setup --refresh: the data directory " + l.home + " is on a network filesystem (nfs4, mounted at " + resolved(t, l.home) + ")",
		"which this installation was not set up to allow. Machines that share this directory would share one machine ID",
		"If only one machine ever mounts this home, run agent-archive setup --allow-network-home. Nothing was changed",
	} {
		if !strings.Contains(flat(errOut.String()), want) {
			t.Errorf("the refusal lacks %q:\n%s", want, &errOut)
		}
	}
	if after := rawFiles(t, l); after != before {
		t.Errorf("a refused refresh changed files:\n%s\nvs\n%s", before, after)
	}

	var found []string
	for _, w := range statusWarnings(t, l.env) {
		if strings.Contains(w, "network filesystem") {
			found = append(found, w)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0], "Setup now refuses a network filesystem. If only one machine ever mounts it, run agent-archive setup --allow-network-home to allow it; otherwise move the data directory to local disk with AGENT_ARCHIVE_HOME.") {
		t.Errorf("status warnings of the network filesystem: %q", found)
	}
}

// Uninstall is the way off a network home, so it is never refused there,
// allowed or not: the units, hooks and job go as anywhere else.
func TestLinuxUninstallIsNotRefusedOnANetworkFilesystem(t *testing.T) {
	t.Parallel()
	l := newLinuxInstall(t)
	l.setup()
	l.env.MountTable = mountTableWith(t, map[string]string{l.home: "nfs4", l.userHome: "nfs4"})
	code, output := l.uninstall()
	if code != 0 || strings.Contains(output, "network filesystem") {
		t.Fatalf("uninstall on a network home: exit %d\n%s", code, output)
	}
	timer, service := l.units()
	for _, path := range []string{timer, service} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s (%v)", path, err)
		}
	}
	if l.hooksInstalled() {
		t.Error("uninstall left the hooks")
	}
}

// The data directory as setup really finds it (AGENT_ARCHIVE_HOME, through
// the resolution that makes the directory for every other command): a
// refusal is decided without making it, so a refused setup leaves nothing on
// the share. It sets the environment, so it is not parallel.
func TestLinuxSetupRefusalDoesNotMakeTheRealDataDirectory(t *testing.T) {
	l := newLinuxInstall(t)
	share := t.TempDir()
	data := filepath.Join(share, "agent-archive")
	t.Setenv("AGENT_ARCHIVE_HOME", data)
	l.env.Home = nil // the real resolution, from AGENT_ARCHIVE_HOME
	l.env.MountTable = mountTableWith(t, map[string]string{share: "nfs"})
	output := setupYes(t, l.env, "", 1, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--region", "us-east-1", "--project", t.TempDir(), "--apps", "claude")
	if !strings.Contains(flat(output), "is on a network filesystem (nfs") {
		t.Errorf("setup --yes did not refuse:\n%s", output)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Errorf("a refused setup made the data directory %s (%v)", data, err)
	}
}

// rawFiles is the installation's files, for telling that nothing changed.
func rawFiles(t *testing.T, l *linuxInstall) string {
	t.Helper()
	var out strings.Builder
	timer, service := l.units()
	for _, path := range []string{filepath.Join(l.home, "config.json"), timer, service} {
		data, err := os.ReadFile(path)
		must(t, err)
		out.WriteString(path + "\n" + string(data) + "\n")
	}
	return out.String()
}

// macOS never looks: nothing reads the mount table, however the file system
// is mounted, setup records no opt-in, and status says nothing of it.
func TestMacOSNeverChecksForANetworkFilesystem(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	var reads atomic.Int32
	env.MountTable = func() ([]byte, error) {
		reads.Add(1)
		return []byte("1 0 0:1 / / rw - nfs4 server:/ rw\n"), nil
	}
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	// Even asked for, the opt-in is not recorded.
	setupYes(t, env, "", 0, "--yes", "--allow-network-home")
	if code := Run([]string{"setup", "--refresh"}, nil, new(strings.Builder), new(strings.Builder), env); code != 0 {
		t.Errorf("setup --refresh exit %d", code)
	}
	for _, w := range statusWarnings(t, env) {
		if strings.Contains(w, "network") {
			t.Errorf("status on macOS warned: %q", w)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("macOS read the mount table %d times", n)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.json"))
	must(t, err)
	if strings.Contains(string(data), "allow_network_home") {
		t.Errorf("a Mac's config.json has the opt-in:\n%s", data)
	}
}

// Another system than Linux or macOS reads nothing either.
func TestUnknownSystemNeverChecksForANetworkFilesystem(t *testing.T) {
	t.Parallel()
	env := Env{OS: platform.Unknown, MountTable: func() ([]byte, error) {
		t.Error("read the mount table on a system that is not Linux")
		return nil, nil
	}}
	if spots := env.networkSpots("/h/data", "/h"); len(spots) != 0 {
		t.Errorf("spots %v", spots)
	}
}

// Env's default reads this process's mount table through readMountTable,
// which the tests replace: no Linux test reads the machine running it.
func TestMountTableDefaultIsIsolatedInTests(t *testing.T) {
	t.Parallel()
	if _, err := (Env{OS: platform.Linux}).mountTable(); err == nil {
		t.Error("a test read the real mount table")
	}
	env := Env{MountTable: func() ([]byte, error) { return []byte("x"), nil }}
	if got, err := env.mountTable(); err != nil || string(got) != "x" {
		t.Errorf("Env.MountTable is not used: %q, %v", got, err)
	}
}

// The opt-in is kept while it is needed and dropped when it is not; it never
// appears unasked.
func TestNetworkHomeOptIn(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	network := Env{OS: platform.Linux, MountTable: mountTableWith(t, map[string]string{home: "nfs4"})}
	local := Env{OS: platform.Linux, MountTable: mountTableWith(t, nil)}
	for _, tc := range []struct {
		name  string
		env   Env
		flag  bool
		saved bool
		want  bool
	}{
		{"asked on a network filesystem", network, true, false, true},
		{"saved on a network filesystem", network, false, true, true},
		{"not asked on a network filesystem", network, false, false, false},
		{"asked on local disk", local, true, false, false},
		{"saved, then moved to local disk", local, false, true, false},
	} {
		if got := tc.env.networkHomeOptIn(home, userHome, tc.flag, config.Config{AllowNetworkHome: tc.saved}); got != tc.want {
			t.Errorf("%s: opt-in %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The saved opt-in is a field old binaries ignore and that is absent unless
// set: config.json round-trips it.
func TestAllowNetworkHomeIsAnOptionalConfigField(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(config.Config{})
	must(t, err)
	if strings.Contains(string(encoded), "allow_network_home") {
		t.Errorf("an unset opt-in is written: %s", encoded)
	}
	encoded, err = json.Marshal(config.Config{AllowNetworkHome: true})
	must(t, err)
	if !strings.Contains(string(encoded), `"allow_network_home":true`) {
		t.Errorf("a set opt-in is not written: %s", encoded)
	}
}
