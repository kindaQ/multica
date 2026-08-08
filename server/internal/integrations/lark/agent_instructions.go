package lark

import (
	"fmt"
	"strings"
)

type AgentBotCapability struct {
	InstallationID string
	TargetType     string
	TargetID       string
	TargetName     string
}

// BuildAgentBotInstructions renders the server-owned instruction block sent
// through the existing daemon claim Agent.Instructions field. It deliberately
// includes installation_id because public/older multica CLIs require that
// flag before they will send the request to the server.
func BuildAgentBotInstructions(bots []AgentBotCapability) string {
	if len(bots) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Feishu Notifications\n\n")
	b.WriteString("You can proactively send a Feishu message to the user who installed your shared Bot. This capability is available to you through your squad membership and must not be used for recipients outside that squad.\n\n")
	if len(bots) == 1 {
		fmt.Fprintf(&b, "Use this active Bot installation: `%s`", bots[0].InstallationID)
		if bots[0].TargetName != "" {
			fmt.Fprintf(&b, " (%s: %s)", bots[0].TargetType, bots[0].TargetName)
		}
		b.WriteString(".\n\n")
		fmt.Fprintf(&b, "`multica feishu push --installation-id %s --content \"<message>\" --idempotency-key \"<unique-stable-key>\" [--issue-id <issue-id>]`\n\n", bots[0].InstallationID)
	} else {
		b.WriteString("More than one active Bot is available. Choose the installation that matches the intended squad:\n\n")
		for _, bot := range bots {
			label := bot.TargetName
			if label == "" {
				label = bot.TargetID
			}
			fmt.Fprintf(&b, "- `%s` — %s: %s\n", bot.InstallationID, bot.TargetType, label)
		}
		b.WriteString("\nRun `multica feishu push --installation-id <installation-id-above> --content \"<message>\" --idempotency-key \"<unique-stable-key>\" [--issue-id <issue-id>]`.\n\n")
	}
	b.WriteString("Use a different idempotency key for each distinct message. Include `--issue-id` when a quoted reply should return to this agent in that issue; omit it for this agent's chat session. A successful issue-routed push already persists the pushed content as the task's final issue comment. Do not post a second comment containing a delivery receipt or message ID.")
	return b.String()
}
