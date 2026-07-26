package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	defaultPublicWorkspaceName = "Multica Public Gateway"
	defaultPublicWorkspaceSlug = "multica-public-gateway"
)

type InstanceBootstrapResponse struct {
	Status                      string  `json:"status"`
	IsSuperAdmin                bool    `json:"is_super_admin"`
	SuperAdminUserID            string  `json:"super_admin_user_id"`
	PublicWorkspaceID           *string `json:"public_workspace_id"`
	PublicAgentID               *string `json:"public_agent_id"`
	PublicChannelInstallationID *string `json:"public_channel_installation_id"`
	InitializedAt               *string `json:"initialized_at"`
}

type BootstrapInstanceRequest struct {
	WorkspaceName string `json:"workspace_name"`
	WorkspaceSlug string `json:"workspace_slug"`
}

func instanceBootstrapResponse(state db.InstanceState, userID string) InstanceBootstrapResponse {
	return InstanceBootstrapResponse{
		Status:                      state.Status,
		IsSuperAdmin:                uuidToString(state.SuperAdminUserID) == userID,
		SuperAdminUserID:            uuidToString(state.SuperAdminUserID),
		PublicWorkspaceID:           optionalUUIDString(state.PublicWorkspaceID),
		PublicAgentID:               optionalUUIDString(state.PublicAgentID),
		PublicChannelInstallationID: optionalUUIDString(state.PublicChannelInstallationID),
		InitializedAt:               timestampToPtr(state.InitializedAt),
	}
}

func optionalUUIDString(id pgtype.UUID) *string {
	if !id.Valid {
		return nil
	}
	value := uuidToString(id)
	return &value
}

// GetInstanceBootstrap returns the instance-level gateway state. It is
// user-visible because every account needs to know whether setup is complete;
// only the super administrator receives mutation routes.
func (h *Handler) GetInstanceBootstrap(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	state, err := h.Queries.GetInstanceState(r.Context())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusServiceUnavailable, "instance has no super administrator")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load instance state")
		return
	}
	writeJSON(w, http.StatusOK, instanceBootstrapResponse(state, userID))
}

// BootstrapInstance creates the protected public workspace, its transport-only
// runtime, and the public gateway agent in one transaction. The instance row is
// locked first, making retries and concurrent submissions idempotent.
func (h *Handler) BootstrapInstance(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}

	req := BootstrapInstanceRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.WorkspaceName = strings.TrimSpace(req.WorkspaceName)
	req.WorkspaceSlug = strings.ToLower(strings.TrimSpace(req.WorkspaceSlug))
	if req.WorkspaceName == "" {
		req.WorkspaceName = defaultPublicWorkspaceName
	}
	if req.WorkspaceSlug == "" {
		req.WorkspaceSlug = defaultPublicWorkspaceSlug
	}
	if !workspaceSlugPattern.MatchString(req.WorkspaceSlug) || isReservedSlug(req.WorkspaceSlug) {
		writeError(w, http.StatusBadRequest, "invalid public workspace slug")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin instance bootstrap")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	state, err := qtx.LockInstanceState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock instance state")
		return
	}
	if uuidToString(state.SuperAdminUserID) != userID {
		writeError(w, http.StatusForbidden, "only the instance super administrator can initialize the public gateway")
		return
	}
	if state.PublicWorkspaceID.Valid && state.PublicAgentID.Valid {
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to finish instance bootstrap")
			return
		}
		writeJSON(w, http.StatusOK, instanceBootstrapResponse(state, userID))
		return
	}

	workspace, err := qtx.CreateWorkspace(r.Context(), db.CreateWorkspaceParams{
		Name:        req.WorkspaceName,
		Slug:        req.WorkspaceSlug,
		Description: pgtype.Text{String: "System-owned workspace for the Feishu public gateway.", Valid: true},
		Context:     pgtype.Text{},
		IssuePrefix: "PUB",
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "public workspace slug already exists")
			return
		}
		slog.Warn("create public workspace failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create public workspace")
		return
	}
	if _, err := qtx.CreateMember(r.Context(), db.CreateMemberParams{
		WorkspaceID: workspace.ID,
		UserID:      userUUID,
		Role:        "owner",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create public workspace owner")
		return
	}

	runtime, err := qtx.CreatePublicGatewayRuntime(r.Context(), db.CreatePublicGatewayRuntimeParams{
		WorkspaceID: workspace.ID,
		OwnerID:     userUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create public gateway runtime")
		return
	}
	publicAgent, err := qtx.CreatePublicGatewayAgent(r.Context(), db.CreatePublicGatewayAgentParams{
		WorkspaceID: workspace.ID,
		RuntimeID:   runtime.ID,
		OwnerID:     userUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create public gateway agent")
		return
	}
	state, err = qtx.CompleteInstanceBootstrap(r.Context(), db.CompleteInstanceBootstrapParams{
		PublicWorkspaceID: workspace.ID,
		PublicAgentID:     publicAgent.ID,
		SuperAdminUserID:  userUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist instance bootstrap")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit instance bootstrap")
		return
	}

	h.notifyDaemonWorkspacesChanged(userID)
	slog.Info("instance public gateway initialized",
		append(logger.RequestAttrs(r),
			"workspace_id", uuidToString(workspace.ID),
			"agent_id", uuidToString(publicAgent.ID),
			"super_admin_user_id", userID,
		)...,
	)
	writeJSON(w, http.StatusCreated, instanceBootstrapResponse(state, userID))
}

func (h *Handler) isPublicWorkspace(ctx context.Context, workspaceID pgtype.UUID) (bool, error) {
	state, err := h.Queries.GetInstanceState(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state.PublicWorkspaceID.Valid && state.PublicWorkspaceID == workspaceID, nil
}

func (h *Handler) isPublicAgent(ctx context.Context, agentID pgtype.UUID) (bool, error) {
	state, err := h.Queries.GetInstanceState(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state.PublicAgentID.Valid && state.PublicAgentID == agentID, nil
}
