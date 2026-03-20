package api

import (
	"net/http"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// SetupHandler handles first-run admin creation.
type SetupHandler struct {
	db  *database.DB
	log zerolog.Logger
}

func NewSetupHandler(db *database.DB, log zerolog.Logger) *SetupHandler {
	return &SetupHandler{db: db, log: log}
}

// Setup creates the first admin user. Only works when zero users exist.
// POST /auth/setup {"username": "admin@example.com", "password": "..."}
func (h *SetupHandler) Setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	// Check if any users exist
	count, err := h.db.CountUsers(r.Context())
	if err != nil {
		h.log.Error().Err(err).Msg("setup: count users failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if count > 0 {
		WriteError(w, http.StatusConflict, "setup already completed — users exist")
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !database.ValidateUsername(req.Username) {
		WriteError(w, http.StatusBadRequest, "username must be a valid email address")
		return
	}
	if len(req.Password) < 8 {
		WriteError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		h.log.Error().Err(err).Msg("setup: bcrypt failed")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	user, err := h.db.CreateUser(r.Context(), req.Username, string(hash), "admin")
	if err != nil {
		h.log.Error().Err(err).Msg("setup: create user failed")
		WriteError(w, http.StatusInternalServerError, "failed to create admin user")
		return
	}

	h.log.Info().
		Int("user_id", user.ID).
		Str("username", user.Username).
		Msg("first admin user created via setup")

	WriteJSON(w, http.StatusCreated, map[string]any{
		"message": "admin account created",
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"role":     user.Role,
		},
	})
}
