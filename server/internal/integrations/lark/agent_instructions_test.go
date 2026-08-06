package lark

import (
	"strings"
	"testing"
)

func TestBuildAgentBotInstructionsSingleBotSupportsOldCLI(t *testing.T) {
	const installationID = "4bb0b363-1111-2222-3333-444444444444"
	out := BuildAgentBotInstructions([]AgentBotCapability{{
		InstallationID: installationID,
		TargetType:     "squad",
		TargetID:       "522188aa-1111-2222-3333-444444444444",
		TargetName:     "研发工作流小队",
	}})
	for _, want := range []string{
		"## Feishu Notifications",
		"squad: 研发工作流小队",
		"multica feishu push --installation-id " + installationID,
		"--issue-id <issue-id>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("instructions missing %q:\n%s", want, out)
		}
	}
}

func TestBuildAgentBotInstructionsMultipleBotsRequiresSelection(t *testing.T) {
	out := BuildAgentBotInstructions([]AgentBotCapability{
		{InstallationID: "install-a", TargetType: "squad", TargetName: "Alpha"},
		{InstallationID: "install-b", TargetType: "squad", TargetName: "Beta"},
	})
	for _, want := range []string{"install-a", "Alpha", "install-b", "Beta", "<installation-id-above>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("instructions missing %q:\n%s", want, out)
		}
	}
}

func TestBuildAgentBotInstructionsNoBotIsEmpty(t *testing.T) {
	if got := BuildAgentBotInstructions(nil); got != "" {
		t.Fatalf("instructions = %q, want empty", got)
	}
}
