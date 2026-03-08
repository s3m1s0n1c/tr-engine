package api

import (
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/database"
)

type AdminHandler struct {
	db            *database.DB
	live          LiveDataSource
	onSystemMerge func(sourceID, targetID int)
}

func NewAdminHandler(db *database.DB, live LiveDataSource, onSystemMerge func(int, int)) *AdminHandler {
	return &AdminHandler{db: db, live: live, onSystemMerge: onSystemMerge}
}

// MergeSystems merges two systems.
func (h *AdminHandler) MergeSystems(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID int `json:"source_id"`
		TargetID int `json:"target_id"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
		return
	}

	if req.SourceID == 0 || req.TargetID == 0 {
		WriteError(w, http.StatusBadRequest, "source_id and target_id are required")
		return
	}
	if req.SourceID == req.TargetID {
		WriteError(w, http.StatusBadRequest, "source_id and target_id must be different")
		return
	}

	callsMoved, tgMoved, tgMerged, unitsMoved, unitsMerged, eventsMoved, err :=
		h.db.MergeSystems(r.Context(), req.SourceID, req.TargetID, "api")
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "merge failed: "+err.Error())
		return
	}

	// Invalidate in-memory identity cache so new messages resolve to the target system
	if h.onSystemMerge != nil {
		h.onSystemMerge(req.SourceID, req.TargetID)
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"target_id":         req.TargetID,
		"source_id":         req.SourceID,
		"calls_moved":       callsMoved,
		"talkgroups_moved":  tgMoved,
		"talkgroups_merged": tgMerged,
		"units_moved":       unitsMoved,
		"units_merged":      unitsMerged,
		"events_moved":      eventsMoved,
	})
}

// GetMaintenance returns current maintenance config and last run results.
func (h *AdminHandler) GetMaintenance(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}
	status := h.live.MaintenanceStatus()
	WriteJSON(w, http.StatusOK, status)
}

// RunMaintenance triggers an immediate maintenance run.
func (h *AdminHandler) RunMaintenance(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}
	result, err := h.live.RunMaintenance(r.Context())
	if err != nil {
		WriteError(w, http.StatusConflict, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

// SubmitBackfill queues a transcription backfill job.
func (h *AdminHandler) SubmitBackfill(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}

	var body BackfillFiltersData
	if r.ContentLength != 0 {
		if err := DecodeJSON(r, &body); err != nil && err != io.EOF {
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body")
			return
		}
	}

	jobID, position, total, err := h.live.SubmitBackfill(r.Context(), body)
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	WriteJSON(w, http.StatusAccepted, map[string]any{
		"job_id":   jobID,
		"position": position,
		"total":    total,
		"filters":  body,
	})
}

// GetBackfillStatus returns the active and queued backfill jobs.
func (h *AdminHandler) GetBackfillStatus(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}

	status := h.live.BackfillStatus()
	if status == nil {
		WriteJSON(w, http.StatusOK, map[string]any{
			"active": nil,
			"queued": []BackfillJobData{},
		})
		return
	}
	WriteJSON(w, http.StatusOK, status)
}

// CancelBackfill cancels a backfill job by ID, or all jobs if no ID is given.
func (h *AdminHandler) CancelBackfill(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}

	id := 0
	if idStr := chi.URLParam(r, "id"); idStr != "" {
		var err error
		id, err = strconv.Atoi(idStr)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "invalid job id")
			return
		}
	}

	if !h.live.CancelBackfill(id) {
		WriteError(w, http.StatusNotFound, "job not found")
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"cancelled": true,
	})
}

// Routes registers admin routes on the given router.
func (h *AdminHandler) Routes(r chi.Router) {
	r.Post("/admin/systems/merge", h.MergeSystems)
	r.Get("/admin/maintenance", h.GetMaintenance)
	r.Post("/admin/maintenance", h.RunMaintenance)
	r.Post("/admin/transcribe-backfill", h.SubmitBackfill)
	r.Get("/admin/transcribe-backfill", h.GetBackfillStatus)
	r.Delete("/admin/transcribe-backfill/{id}", h.CancelBackfill)
	r.Delete("/admin/transcribe-backfill", h.CancelBackfill)
}
