package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// jwtKeyFunc returns a key function that validates the signing algorithm is HS256
// before returning the HMAC secret. Without this check, an attacker could forge
// tokens using alg:"none" or switch to an asymmetric algorithm.
func jwtKeyFunc(secret []byte) jwt.Keyfunc {
	return func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != "HS256" {
			return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
		}
		return secret, nil
	}
}

// AuthHandler handles login, token refresh, and logout.
type AuthHandler struct {
	db        *database.DB
	jwtSecret []byte
	log       zerolog.Logger
}

// Claims is the JWT claims structure for access tokens.
type Claims struct {
	jwt.RegisteredClaims
	Username string `json:"username"`
	Role     string `json:"role"`
}

// RefreshClaims is the JWT claims structure for refresh tokens.
type RefreshClaims struct {
	jwt.RegisteredClaims
	Type string `json:"type"`
}

const (
	accessTokenExpiry  = 1 * time.Hour
	refreshTokenExpiry = 7 * 24 * time.Hour
	refreshCookieName  = "tr_refresh_token"
	refreshCookiePath  = "/api/v1/auth/"
)

func NewAuthHandler(db *database.DB, jwtSecret []byte, log zerolog.Logger) *AuthHandler {
	return &AuthHandler{db: db, jwtSecret: jwtSecret, log: log}
}

// Login validates credentials and returns an access token + sets refresh cookie.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" || req.Password == "" {
		WriteError(w, http.StatusBadRequest, "username and password required")
		return
	}

	user, err := h.db.GetUserByUsername(r.Context(), req.Username)
	if err != nil {
		h.log.Error().Err(err).Msg("login: database error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if user == nil {
		WriteError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if !user.Enabled {
		WriteError(w, http.StatusUnauthorized, "account disabled")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// Generate access token
	accessToken, err := h.generateAccessToken(user)
	if err != nil {
		h.log.Error().Err(err).Msg("login: failed to generate access token")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Generate refresh token and set as httpOnly cookie
	refreshToken, err := h.generateRefreshToken(user.ID)
	if err != nil {
		h.log.Error().Err(err).Msg("login: failed to generate refresh token")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    refreshToken,
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(refreshTokenExpiry.Seconds()),
	})

	WriteJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"role":     user.Role,
		},
	})
}

// Refresh validates the refresh cookie and returns a new access token.
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		WriteError(w, http.StatusUnauthorized, "no refresh token")
		return
	}

	claims := &RefreshClaims{}
	token, err := jwt.ParseWithClaims(cookie.Value, claims, jwtKeyFunc(h.jwtSecret))
	if err != nil || !token.Valid {
		WriteError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	if claims.Type != "refresh" {
		WriteError(w, http.StatusUnauthorized, "invalid token type")
		return
	}

	subStr, err := claims.GetSubject()
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	userID, err := strconv.Atoi(subStr)
	if err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}

	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil {
		h.log.Error().Err(err).Msg("refresh: database error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if user == nil || !user.Enabled {
		WriteError(w, http.StatusUnauthorized, "account not found or disabled")
		return
	}

	accessToken, err := h.generateAccessToken(user)
	if err != nil {
		h.log.Error().Err(err).Msg("refresh: failed to generate access token")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"user": map[string]any{
			"id":       user.ID,
			"username": user.Username,
			"role":     user.Role,
		},
	})
}

// Logout clears the refresh cookie.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	WriteJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// Me returns the current user from JWT context.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID := ContextUserID(r)
	if userID == 0 {
		WriteError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil {
		h.log.Error().Err(err).Msg("me: database error")
		WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if user == nil {
		WriteError(w, http.StatusNotFound, "user not found")
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
		"enabled":  user.Enabled,
	})
}

func (h *AuthHandler) generateAccessToken(user *database.User) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.Itoa(user.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenExpiry)),
		},
		Username: user.Username,
		Role:     user.Role,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(h.jwtSecret)
}

func (h *AuthHandler) generateRefreshToken(userID int) (string, error) {
	now := time.Now()
	claims := RefreshClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.Itoa(userID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(refreshTokenExpiry)),
		},
		Type: "refresh",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(h.jwtSecret)
}
