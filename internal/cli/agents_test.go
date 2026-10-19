package cli

import (
	"bytes"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"reflect"
	"strings"
	"testing"
)

const syntheticID agentmeta.ID = "synthetic"

const syntheticDestination handoffDestination = "synthetic"

type syntheticLauncher struct{}

func (syntheticLauncher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"synthetic"}, Install: "Synthetic"}
}

func (syntheticLauncher) Args(r agentapi.LaunchRequest) ([]string, error) {
	return []string{"--synthetic", r.ProjectDir, r.HandoffPath, r.Prompt}, nil
}

func TestInjectedCatalogReachesCLIFlagsAndLaunch(t *testing.T) {
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: syntheticID, Aliases: []string{"test-agent"}, DisplayName: "Synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtin.New(c, []builtin.Integration{{Descriptor: agentmeta.Descriptor{ID: syntheticID}, Launcher: syntheticLauncher{}}})
	if err != nil {
		t.Fatal(err)
	}
	env := Env{Agents: reg, LookPath: func(name string) (string, error) { return "/bin/" + name, nil }, Environ: func() []string { return nil }}
	var stderr bytes.Buffer
	opts, ok := parseHandoffOptions([]string{"--file", "/synthetic.jsonl", "--harness", " TEST-AGENT ", "--to", "TEST-AGENT"}, &stderr, env, false)
	if !ok || opts.to != "synthetic" || opts.harness != "synthetic" {
		t.Fatalf("flags %+v %v %s", opts, ok, stderr.String())
	}
	spec, err := buildLaunchSpec(handoffDestination(opts.to), "prompt", "/handoff", "/project", nil, env)
	if err != nil || spec.Binary != "/bin/synthetic" || !reflect.DeepEqual(spec.Args, []string{"--synthetic", "/project", "/handoff", "prompt"}) {
		t.Fatalf("launch %+v %v", spec, err)
	}
	var choiceOut bytes.Buffer
	choice, err := chooseDestination(newPrompter(strings.NewReader("TEST-AGENT\n"), &choiceOut), []handoffDestination{syntheticDestination}, syntheticDestination, false, c, nil)
	if err != nil || choice.dest != syntheticDestination || !strings.Contains(choiceOut.String(), "Continue in Synthetic?") {
		t.Fatalf("choice %+v %v %s", choice, err, choiceOut.String())
	}
	fs := env.newCommandFlags("list", &stderr)
	list, code := listOptionsFromFlags(fs, listFlagValues{harness: "TEST-AGENT", skillUsage: "used"}, env.now())
	if code != 0 || list.filter.Harness != "synthetic" {
		t.Fatalf("list flags %+v %d %s", list, code, stderr.String())
	}
}
