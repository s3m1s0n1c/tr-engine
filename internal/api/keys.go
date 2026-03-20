package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
)

// RoleLevel returns the integer level for a role string.
func RoleLevel(role string) int {
	switch role {
	case "viewer":
		return 1
	case "editor":
		return 2
	case "admin":
		return 3
	default:
		return 0
	}
}

// KeysHandler handles API key management endpoints.
type KeysHandler struct {
	db  *database.DB
	log zerolog.Logger
}

func NewKeysHandler(db *database.DB, log zerolog.Logger) *KeysHandler {
	return &KeysHandler{db: db, log: log}
}

// Routes registers API key management routes.
// Caller is responsible for wrapping in appropriate auth middleware.
func (h *KeysHandler) Routes(r chi.Router) {
	r.Get("/auth/keys", h.ListOwn)
	r.Post("/auth/keys", h.Create)
	r.Delete("/auth/keys/{id}", h.DeleteOwn)
}

// AdminRoutes registers admin-only API key routes.
func (h *KeysHandler) AdminRoutes(r chi.Router) {
	r.Get("/auth/keys/all", h.ListAll)
	r.Post("/auth/keys/service", h.CreateServiceAccount)
	r.Delete("/auth/keys/{id}/any", h.DeleteAny)
}

// ListOwn returns API keys owned by the current user.
func (h *KeysHandler) ListOwn(w http.ResponseWriter, r *http.Request) {
	userID := ContextUserID(r)
	if userID == 0 {
		WriteError(w, http.StatusUnauthorized, "user authentication required (API keys cannot list keys)")
		return
	}

	keys, err := h.db.ListAPIKeysByUser(r.Context(), userID)
	if err != nil {
		h.log.Error().Err(err).Msg("keys: list own failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if keys == nil {
		keys = []database.APIKey{}
	}
	WriteJSON(w, http.StatusOK, keys)
}

// Create creates a new API key for the current user.
// The key's role is capped at the caller's own role.
func (h *KeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID := ContextUserID(r)
	if userID == 0 {
		WriteError(w, http.StatusUnauthorized, "user authentication required")
		return
	}
	callerRole := ContextRole(r)

	var req struct {
		Label string `json:"label"`
		Role  string `json:"role"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}
	if req.Label == "" {
		WriteError(w, http.StatusBadRequest, "label is required")
		return
	}

	// Cap role at caller's level
	if RoleLevel(req.Role) > RoleLevel(callerRole) {
		WriteError(w, http.StatusForbidden, "cannot create key with higher role than your own")
		return
	}
	if RoleLevel(req.Role) == 0 {
		WriteError(w, http.StatusBadRequest, "invalid role (viewer, editor, admin)")
		return
	}

	key, err := h.db.CreateAPIKey(r.Context(), &userID, req.Role, req.Label, false)
	if err != nil {
		h.log.Error().Err(err).Msg("keys: create failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.log.Info().
		Int("user_id", userID).
		Str("key_prefix", key.KeyPrefix).
		Str("role", req.Role).
		Str("label", req.Label).
		Msg("api key created")

	WriteJSON(w, http.StatusCreated, key)
}

// DeleteOwn deletes an API key owned by the current user.
func (h *KeysHandler) DeleteOwn(w http.ResponseWriter, r *http.Request) {
	userID := ContextUserID(r)
	if userID == 0 {
		WriteError(w, http.StatusUnauthorized, "user authentication required")
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid key ID")
		return
	}

	if err := h.db.DeleteAPIKeyOwned(r.Context(), id, userID); err != nil {
		WriteError(w, http.StatusNotFound, "key not found or not owned by you")
		return
	}

	h.log.Info().Int("user_id", userID).Int("key_id", id).Msg("api key revoked (own)")
	w.WriteHeader(http.StatusNoContent)
}

// ListAll returns all API keys (admin only).
func (h *KeysHandler) ListAll(w http.ResponseWriter, r *http.Request) {
	keys, err := h.db.ListAllAPIKeys(r.Context())
	if err != nil {
		h.log.Error().Err(err).Msg("keys: list all failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if keys == nil {
		keys = []database.APIKey{}
	}
	WriteJSON(w, http.StatusOK, keys)
}

// CreateServiceAccount creates an API key that acts as its own identity.
func (h *KeysHandler) CreateServiceAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
		Role  string `json:"role"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}
	if req.Label == "" {
		WriteError(w, http.StatusBadRequest, "label is required")
		return
	}
	if RoleLevel(req.Role) == 0 {
		WriteError(w, http.StatusBadRequest, "invalid role (viewer, editor, admin)")
		return
	}

	key, err := h.db.CreateAPIKey(r.Context(), nil, req.Role, req.Label, true)
	if err != nil {
		h.log.Error().Err(err).Msg("keys: create service account failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	h.log.Info().
		Str("key_prefix", key.KeyPrefix).
		Str("role", req.Role).
		Str("label", req.Label).
		Msg("service account key created")

	WriteJSON(w, http.StatusCreated, key)
}

// DeleteAny deletes any API key by ID (admin only).
func (h *KeysHandler) DeleteAny(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid key ID")
		return
	}

	if err := h.db.DeleteAPIKey(r.Context(), id); err != nil {
		WriteError(w, http.StatusNotFound, "key not found")
		return
	}

	h.log.Info().Int("key_id", id).Msg("api key revoked (admin)")
	w.WriteHeader(http.StatusNoContent)
}
