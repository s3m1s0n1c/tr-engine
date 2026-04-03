package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/config"
)

// registerAuthInit wires just the auth-init endpoint for testing.
func registerAuthInit(cfg *config.Config) http.Handler {
	r := chi.NewRouter()

	r.Get("/api/v1/auth-init", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"jwt_enabled": cfg.AdminPassword != "",
		}
		switch {
		case cfg.AuthToken == "" && cfg.AdminPassword == "":
			resp["mode"] = "open"
		case cfg.AuthToken != "" && cfg.AdminPassword == "":
			resp["mode"] = "token"
		default:
			resp["mode"] = "full"
			if cfg.AuthToken != "" {
				resp["read_token"] = cfg.AuthToken
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		b, _ := json.Marshal(resp)
		w.Write(b)
	})
	return r
}

func TestAuthInit_OpenMode(t *testing.T) {
	h := registerAuthInit(&config.Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "open" {
		t.Errorf("expected mode=open, got %v", resp["mode"])
	}
	if resp["jwt_enabled"] != false {
		t.Errorf("expected jwt_enabled=false, got %v", resp["jwt_enabled"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Error("read_token should not be present in open mode")
	}
}

func TestAuthInit_TokenMode(t *testing.T) {
	h := registerAuthInit(&config.Config{AuthToken: "my-secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "token" {
		t.Errorf("expected mode=token, got %v", resp["mode"])
	}
	if resp["jwt_enabled"] != false {
		t.Errorf("expected jwt_enabled=false, got %v", resp["jwt_enabled"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Error("read_token should NOT be present in token mode")
	}
}

func TestAuthInit_FullMode_WithPublicRead(t *testing.T) {
	h := registerAuthInit(&config.Config{AuthToken: "read-tok", AdminPassword: "admin123"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "full" {
		t.Errorf("expected mode=full, got %v", resp["mode"])
	}
	if resp["jwt_enabled"] != true {
		t.Errorf("expected jwt_enabled=true, got %v", resp["jwt_enabled"])
	}
	if resp["read_token"] != "read-tok" {
		t.Errorf("expected read_token=read-tok, got %v", resp["read_token"])
	}
}

func TestAuthInit_FullMode_NoPublicRead(t *testing.T) {
	h := registerAuthInit(&config.Config{AdminPassword: "admin123"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["mode"] != "full" {
		t.Errorf("expected mode=full, got %v", resp["mode"])
	}
	if _, ok := resp["read_token"]; ok {
		t.Error("read_token should not be present when AUTH_TOKEN is empty")
	}
}

func TestAuthInit_CacheControl(t *testing.T) {
	h := registerAuthInit(&config.Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/auth-init", nil)
	h.ServeHTTP(rec, req)

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control: no-store, got %q", cc)
	}
}
