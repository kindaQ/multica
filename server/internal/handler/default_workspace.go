package handler

import (
	"encoding/json"
	"net/http"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type UpdateDefaultWorkspaceRequest struct {
	WorkspaceID string `json:"workspace_id"`
}

type DefaultWorkspaceResponse struct {
	WorkspaceID *string `json:"workspace_id"`
}

func (h *Handler) GetDefaultWorkspace(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	user, err := h.Queries.GetUser(r.Context(), parseUUID(userID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load default workspace")
		return
	}
	writeJSON(w, http.StatusOK, DefaultWorkspaceResponse{
		WorkspaceID: optionalUUIDString(user.DefaultWorkspaceID),
	})
}

func (h *Handler) UpdateDefaultWorkspace(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req UpdateDefaultWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, req.WorkspaceID, "workspace_id")
	if !ok {
		return
	}
	if protected, err := h.isPublicWorkspace(r.Context(), workspaceID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to verify default workspace")
		return
	} else if protected {
		writeError(w, http.StatusBadRequest, "the public gateway workspace cannot be a user default")
		return
	}
	userUUID := parseUUID(userID)
	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      userUUID,
		WorkspaceID: workspaceID,
	}); err != nil {
		writeError(w, http.StatusForbidden, "default workspace must be one of your workspaces")
		return
	}
	updated, err := h.Queries.SetDefaultWorkspace(r.Context(), db.SetDefaultWorkspaceParams{
		WorkspaceID: workspaceID,
		UserID:      userUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update default workspace")
		return
	}
	writeJSON(w, http.StatusOK, DefaultWorkspaceResponse{
		WorkspaceID: optionalUUIDString(updated.DefaultWorkspaceID),
	})
}
