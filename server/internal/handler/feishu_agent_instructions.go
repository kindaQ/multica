package handler

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// injectFeishuAgentInstructions appends a server-owned capability block to
// the existing per-agent instructions carried by a daemon task claim. The
// daemon already injects Agent.Instructions into every supported runtime, so
// this works with public, older daemon/CLI installations without requiring a
// runtime upgrade. Installation IDs are routing identifiers, not credentials;
// the delivery endpoint still authenticates the task and rechecks membership.
func (h *Handler) injectFeishuAgentInstructions(ctx context.Context, workspaceID, agentID pgtype.UUID, agent *TaskAgentData) {
	if h == nil || h.Queries == nil || agent == nil || !workspaceID.Valid || !agentID.Valid {
		return
	}
	rows, err := h.Queries.ListActiveChannelInstallationsAccessibleToAgent(ctx, db.ListActiveChannelInstallationsAccessibleToAgentParams{
		WorkspaceID: workspaceID,
		ChannelType: "feishu",
		AgentID:     agentID,
	})
	if err != nil {
		slog.Warn("daemon claim: list accessible Feishu Bots failed", "workspace_id", uuidToString(workspaceID), "agent_id", uuidToString(agentID), "error", err)
		return
	}
	bots := make([]lark.AgentBotCapability, 0, len(rows))
	for _, row := range rows {
		bot := lark.AgentBotCapability{
			InstallationID: uuidToString(row.ID),
			TargetType:     row.TargetType,
			TargetID:       uuidToString(row.TargetID),
		}
		switch row.TargetType {
		case "squad":
			if squad, loadErr := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: row.TargetID, WorkspaceID: workspaceID}); loadErr == nil {
				bot.TargetName = squad.Name
			}
		case "agent":
			if target, loadErr := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: row.TargetID, WorkspaceID: workspaceID}); loadErr == nil {
				bot.TargetName = target.Name
			}
		}
		bots = append(bots, bot)
	}
	capability := lark.BuildAgentBotInstructions(bots)
	if capability == "" {
		return
	}
	if strings.TrimSpace(agent.Instructions) == "" {
		agent.Instructions = capability
	} else {
		agent.Instructions = strings.TrimSpace(agent.Instructions) + "\n\n" + capability
	}
}
