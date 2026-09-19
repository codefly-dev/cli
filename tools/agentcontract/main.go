package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/agentrequirements"
	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func command(directory, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = directory
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func stableTags(directory, currentTag string) ([]string, error) {
	output, err := command(directory, "git", "tag", "--merged", "HEAD", "--sort=-version:refname")
	if err != nil {
		return nil, err
	}
	stable := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	var tags []string
	for _, tag := range strings.Fields(string(output)) {
		if tag != currentTag && stable.MatchString(tag) {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}

func previousRelease(directory, currentTag string) (string, error) {
	tags, err := stableTags(directory, currentTag)
	if err != nil || len(tags) == 0 {
		return "", err
	}
	publishedOutput, err := command(directory, "gh", "api", "--paginate", "repos/codefly-dev/cli/releases?per_page=100", "--jq", ".[] | select(.draft == false and .prerelease == false) | .tag_name")
	if err != nil {
		return "", fmt.Errorf("list published CLI releases: %w", err)
	}
	published := make(map[string]bool)
	for _, tag := range strings.Fields(string(publishedOutput)) {
		published[tag] = true
	}
	for _, tag := range tags {
		if published[tag] {
			return tag, nil
		}
	}
	return "", fmt.Errorf("no published stable CLI release precedes %s", currentTag)
}

func releaseNotes(current, previous *agentv0.AgentContract, previousTag string) string {
	lines := []string{
		"## CLI-agent compatibility", "",
		fmt.Sprintf("Lifecycle protocol: **%d**. Startup protocol: **%d**.", current.ProtocolVersion, current.StartupProtocolVersion),
		"Container/free runtime initialization and builder operations require `" + contract.ContainerRecoveryScope + "` and an exact acknowledgement of this flow's recovery scope.",
		"Native/Nix runtime initialization requires the declared protocols but no container-recovery capability.", "",
	}
	switch {
	case previous == nil:
		lines = append(lines, "Contract enforcement introduced: agents without a protocol declaration must adopt the contract before this CLI can load them, including native agents. Coordinate agent publication and consumer pins before upgrading.")
	case current.ProtocolVersion != previous.ProtocolVersion || current.StartupProtocolVersion != previous.StartupProtocolVersion:
		lines = append(lines, "Agent protocol changed from "+previousTag+"; publish compatible agents before upgrading.")
	default:
		before, after := slices.Clone(previous.Capabilities), slices.Clone(current.Capabilities)
		slices.Sort(before)
		slices.Sort(after)
		if slices.Equal(before, after) {
			lines = append(lines, "Agent requirements unchanged from "+previousTag+". No agent rebuild is required solely for this CLI/Core bump.")
		} else {
			lines = append(lines, "Required capabilities changed from "+previousTag+"; publish agents implementing the capabilities required by the selected operation.")
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func generate(directory, tag, output string) error {
	moduleDir, err := command(directory, "go", "list", "-m", "-f", "{{.Dir}}", "github.com/codefly-dev/core")
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(moduleDir)), "agents", "contract", "contract.json"))
	if err != nil {
		return err
	}
	var compiled agentv0.AgentContract
	if err = protojson.Unmarshal(manifest, &compiled); err != nil {
		return err
	}
	if !proto.Equal(&compiled, contract.Current()) {
		return fmt.Errorf("core manifest differs from the compiled agent contract")
	}
	required := agentrequirements.ContainerRecovery()
	previous, err := previousRelease(directory, tag)
	if err != nil {
		return err
	}
	var previousRequirements *agentv0.AgentContract
	if previous != "" {
		introduced, lookupErr := command(directory, "git", "ls-tree", "--name-only", previous, "--", "tools/agentcontract/main.go")
		if lookupErr != nil {
			return lookupErr
		}
		if len(introduced) != 0 {
			asset, downloadErr := command(directory, "gh", "release", "download", previous, "--repo", "codefly-dev/cli", "--pattern", "agent-requirements.json", "--output", "-")
			if downloadErr != nil {
				return fmt.Errorf("read previous CLI contract: %w", downloadErr)
			}
			previousRequirements = &agentv0.AgentContract{}
			if err = protojson.Unmarshal(asset, previousRequirements); err != nil {
				return err
			}
		}
	}
	requirements, err := protojson.MarshalOptions{Indent: "  "}.Marshal(required)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"contract.json":           manifest,
		"agent-requirements.json": requirements,
		"agent-compatibility.md":  []byte(releaseNotes(required, previousRequirements, previous)),
	} {
		if err = os.WriteFile(filepath.Join(output, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	tag := flag.String("tag", "", "CLI release tag")
	output := flag.String("output", ".release", "artifact directory")
	flag.Parse()
	if *tag == "" {
		fmt.Fprintln(os.Stderr, "--tag is required")
		os.Exit(1)
	}
	if err := generate(".", *tag, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
