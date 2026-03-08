# Transcription Backfill Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a background drip-feed backfill system that finds untranscribed calls and feeds them into the existing transcription queue without delaying real-time transcriptions.

**Architecture:** A `BackfillManager` in the ingest package owns a goroutine that processes a FIFO queue of backfill jobs sequentially. For each job, it queries the DB for untranscribed call IDs in batches and drip-feeds them into the transcription worker pool, only enqueuing when the transcription queue has <= 1 pending job. The API layer talks to it through 3 new methods on the `LiveDataSource` interface, following the same pattern as the maintenance system.

**Tech Stack:** Go, pgx, chi router, zerolog

---

### Task 1: Database queries for untranscribed calls

**Files:**
- Modify: `internal/database/queries_calls.go`

**Step 1: Add the BackfillFilter struct and query functions**

Add after the existing `CallFilter` struct area (around line 7):

```go
// BackfillFilter specifies which untranscribed calls to backfill.
type BackfillFilter struct {
	SystemID  *int
	Tgids     []int
	StartTime *time.Time
	EndTime   *time.Time
	// MinDuration and MaxDuration come from the transcriber config.
	MinDuration float64
	MaxDuration float64
}
```

Add two new methods to `DB`:

```go
// CountUntranscribedCalls returns the number of calls matching the backfill filter
// that have no transcription and are not encrypted.
func (db *DB) CountUntranscribedCalls(ctx context.Context, f BackfillFilter) (int, error) {
	query := `SELECT count(*) FROM calls
		WHERE (has_transcription = false OR has_transcription IS NULL)
		  AND (encrypted IS NULL OR encrypted = false)
		  AND ($1::float8 IS NULL OR duration >= $1)
		  AND ($2::float8 IS NULL OR duration <= $2)
		  AND ($3::int IS NULL OR system_id = $3)
		  AND ($4::int[] IS NULL OR tgid = ANY($4))
		  AND ($5::timestamptz IS NULL OR start_time >= $5)
		  AND ($6::timestamptz IS NULL OR start_time < $6)`

	args := []any{
		nilIfZero(f.MinDuration), nilIfZero(f.MaxDuration),
		f.SystemID, pqIntArray(f.Tgids),
		f.StartTime, f.EndTime,
	}

	var count int
	err := db.Pool.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

// ListUntranscribedCallIDs returns a batch of call IDs matching the backfill filter,
// ordered newest-first. Use offset for cursor-based pagination.
func (db *DB) ListUntranscribedCallIDs(ctx context.Context, f BackfillFilter, limit, offset int) ([]int64, error) {
	query := `SELECT call_id FROM calls
		WHERE (has_transcription = false OR has_transcription IS NULL)
		  AND (encrypted IS NULL OR encrypted = false)
		  AND ($1::float8 IS NULL OR duration >= $1)
		  AND ($2::float8 IS NULL OR duration <= $2)
		  AND ($3::int IS NULL OR system_id = $3)
		  AND ($4::int[] IS NULL OR tgid = ANY($4))
		  AND ($5::timestamptz IS NULL OR start_time >= $5)
		  AND ($6::timestamptz IS NULL OR start_time < $6)
		ORDER BY start_time DESC
		LIMIT $7 OFFSET $8`

	args := []any{
		nilIfZero(f.MinDuration), nilIfZero(f.MaxDuration),
		f.SystemID, pqIntArray(f.Tgids),
		f.StartTime, f.EndTime,
		limit, offset,
	}

	rows, err := db.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

Check if `nilIfZero` exists already. If not, add a helper:

```go
func nilIfZero(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}
```

**Step 2: Verify it compiles**

Run: `cd /c/Users/drewm/tr-engine && go build ./internal/database/...`
Expected: clean compile

**Step 3: Commit**

```
feat(db): add queries for untranscribed call backfill
```

---

### Task 2: BackfillManager in the ingest package

**Files:**
- Create: `internal/ingest/backfill.go`

**Step 1: Create the BackfillManager**

```go
package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/transcribe"
)

// BackfillJob represents a queued backfill request.
type BackfillJob struct {
	ID        int
	Filters   BackfillFilters
	Total     int
	Completed atomic.Int64
	Failed    atomic.Int64
	StartedAt time.Time
	CreatedAt time.Time
}

// BackfillFilters are the user-provided filters for a backfill job.
type BackfillFilters struct {
	SystemID  *int       `json:"system_id,omitempty"`
	Tgids     []int      `json:"tgids,omitempty"`
	StartTime *time.Time `json:"start_time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}

// BackfillStatus is the API-facing status of the backfill manager.
type BackfillStatus struct {
	Active *BackfillJobStatus   `json:"active"`
	Queued []BackfillJobStatus  `json:"queued"`
}

// BackfillJobStatus is the API-facing status of a single backfill job.
type BackfillJobStatus struct {
	JobID     int             `json:"job_id"`
	Filters   BackfillFilters `json:"filters"`
	Total     int             `json:"total"`
	Completed int64           `json:"completed"`
	Failed    int64           `json:"failed"`
	StartedAt *time.Time      `json:"started_at,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// BackfillManager processes a queue of backfill jobs sequentially,
// drip-feeding untranscribed calls into the transcription worker pool.
type BackfillManager struct {
	db          *database.DB
	transcriber *transcribe.WorkerPool
	log         zerolog.Logger
	minDuration float64
	maxDuration float64

	mu       sync.Mutex
	nextID   int
	queue    []*BackfillJob
	active   *BackfillJob
	cancelFn context.CancelFunc // cancels the active job

	submit chan struct{} // signals the loop that a new job was submitted
	ctx    context.Context
}

// NewBackfillManager creates a new backfill manager.
func NewBackfillManager(ctx context.Context, db *database.DB, transcriber *transcribe.WorkerPool, log zerolog.Logger) *BackfillManager {
	return &BackfillManager{
		db:          db,
		transcriber: transcriber,
		log:         log.With().Str("component", "backfill").Logger(),
		minDuration: transcriber.MinDuration(),
		maxDuration: transcriber.MaxDuration(),
		submit:      make(chan struct{}, 1),
		ctx:         ctx,
	}
}

// Start launches the background processing goroutine.
func (bm *BackfillManager) Start() {
	go bm.loop()
	bm.log.Info().Msg("backfill manager started")
}

// Submit adds a backfill job to the queue. Returns the job ID, queue position, and total count.
func (bm *BackfillManager) Submit(ctx context.Context, filters BackfillFilters) (jobID, position, total int, err error) {
	// Count matching calls
	dbFilter := bm.toDBFilter(filters)
	total, err = bm.db.CountUntranscribedCalls(ctx, dbFilter)
	if err != nil {
		return 0, 0, 0, err
	}

	bm.mu.Lock()
	bm.nextID++
	job := &BackfillJob{
		ID:        bm.nextID,
		Filters:   filters,
		Total:     total,
		CreatedAt: time.Now(),
	}
	bm.queue = append(bm.queue, job)
	position = len(bm.queue) - 1
	if bm.active != nil {
		position++ // account for the running job
	}
	jobID = job.ID
	bm.mu.Unlock()

	// Signal the loop
	select {
	case bm.submit <- struct{}{}:
	default:
	}

	bm.log.Info().Int("job_id", jobID).Int("total", total).Int("position", position).Msg("backfill job submitted")
	return jobID, position, total, nil
}

// Status returns the current backfill status.
func (bm *BackfillManager) Status() BackfillStatus {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	var status BackfillStatus
	if bm.active != nil {
		sa := bm.active.StartedAt
		status.Active = &BackfillJobStatus{
			JobID:     bm.active.ID,
			Filters:   bm.active.Filters,
			Total:     bm.active.Total,
			Completed: bm.active.Completed.Load(),
			Failed:    bm.active.Failed.Load(),
			StartedAt: &sa,
			CreatedAt: bm.active.CreatedAt,
		}
	}
	status.Queued = make([]BackfillJobStatus, 0, len(bm.queue))
	for _, j := range bm.queue {
		status.Queued = append(status.Queued, BackfillJobStatus{
			JobID:     j.ID,
			Filters:   j.Filters,
			Total:     j.Total,
			CreatedAt: j.CreatedAt,
		})
	}
	return status
}

// Cancel cancels a job by ID. If id <= 0, cancels all jobs.
// Returns true if a job was found and cancelled.
func (bm *BackfillManager) Cancel(id int) bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if id <= 0 {
		// Cancel all
		found := bm.active != nil || len(bm.queue) > 0
		bm.queue = nil
		if bm.cancelFn != nil {
			bm.cancelFn()
		}
		return found
	}

	// Cancel active job
	if bm.active != nil && bm.active.ID == id {
		if bm.cancelFn != nil {
			bm.cancelFn()
		}
		return true
	}

	// Remove from queue
	for i, j := range bm.queue {
		if j.ID == id {
			bm.queue = append(bm.queue[:i], bm.queue[i+1:]...)
			return true
		}
	}
	return false
}

func (bm *BackfillManager) loop() {
	for {
		// Try to grab the next job
		bm.mu.Lock()
		if len(bm.queue) == 0 {
			bm.mu.Unlock()
			// Wait for a submission or shutdown
			select {
			case <-bm.ctx.Done():
				return
			case <-bm.submit:
				continue
			}
		}
		job := bm.queue[0]
		bm.queue = bm.queue[1:]
		jobCtx, cancel := context.WithCancel(bm.ctx)
		bm.active = job
		bm.cancelFn = cancel
		bm.mu.Unlock()

		job.StartedAt = time.Now()
		bm.log.Info().Int("job_id", job.ID).Int("total", job.Total).Msg("backfill job starting")

		bm.processJob(jobCtx, job)
		cancel()

		bm.mu.Lock()
		bm.active = nil
		bm.cancelFn = nil
		bm.mu.Unlock()

		bm.log.Info().
			Int("job_id", job.ID).
			Int64("completed", job.Completed.Load()).
			Int64("failed", job.Failed.Load()).
			Msg("backfill job finished")
	}
}

func (bm *BackfillManager) processJob(ctx context.Context, job *BackfillJob) {
	dbFilter := bm.toDBFilter(job.Filters)
	offset := 0
	const batchSize = 100

	for {
		if ctx.Err() != nil {
			return
		}

		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		ids, err := bm.db.ListUntranscribedCallIDs(queryCtx, dbFilter, batchSize, offset)
		cancel()

		if err != nil {
			bm.log.Warn().Err(err).Int("job_id", job.ID).Msg("backfill query failed")
			return
		}
		if len(ids) == 0 {
			return // done
		}

		for _, callID := range ids {
			if ctx.Err() != nil {
				return
			}
			// Wait until the transcription queue has room
			bm.waitForQueueRoom(ctx)
			if ctx.Err() != nil {
				return
			}

			if bm.enqueueCall(ctx, callID) {
				job.Completed.Add(1)
			} else {
				job.Failed.Add(1)
			}
		}

		if len(ids) < batchSize {
			return // last batch
		}
		// Don't advance offset — successfully transcribed calls will drop out of
		// the result set (has_transcription flips to true), so we re-query at offset 0.
		// Only advance if some failed (they'd stay in the result set and cause an infinite loop).
		if job.Failed.Load() > 0 {
			offset += int(job.Failed.Load())
		}
	}
}

// waitForQueueRoom blocks until the transcription queue has <= 1 pending jobs.
func (bm *BackfillManager) waitForQueueRoom(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		stats := bm.transcriber.Stats()
		if stats.Pending <= 1 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (bm *BackfillManager) enqueueCall(ctx context.Context, callID int64) bool {
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	c, err := bm.db.GetCallForTranscription(queryCtx, callID)
	if err != nil {
		bm.log.Warn().Err(err).Int64("call_id", callID).Msg("backfill: failed to load call")
		return false
	}
	return bm.transcriber.Enqueue(transcribe.Job{
		CallID:        c.CallID,
		CallStartTime: c.StartTime,
		SystemID:      c.SystemID,
		Tgid:          c.Tgid,
		Duration:      derefFloat32(c.Duration),
		AudioFilePath: c.AudioFilePath,
		CallFilename:  c.CallFilename,
		SrcList:       c.SrcList,
		TgAlphaTag:    c.TgAlphaTag,
		TgDescription: c.TgDescription,
		TgTag:         c.TgTag,
		TgGroup:       c.TgGroup,
	})
}

func (bm *BackfillManager) toDBFilter(f BackfillFilters) database.BackfillFilter {
	return database.BackfillFilter{
		SystemID:    f.SystemID,
		Tgids:       f.Tgids,
		StartTime:   f.StartTime,
		EndTime:     f.EndTime,
		MinDuration: bm.minDuration,
		MaxDuration: bm.maxDuration,
	}
}
```

**Step 2: Verify it compiles**

Run: `cd /c/Users/drewm/tr-engine && go build ./internal/ingest/...`
Expected: clean compile

**Step 3: Commit**

```
feat(ingest): add BackfillManager for transcription backfill
```

---

### Task 3: LiveDataSource interface + pipeline wiring

**Files:**
- Modify: `internal/api/live_data.go`
- Modify: `internal/ingest/pipeline.go`
- Modify: `internal/api/affiliations_test.go` (update mock)

**Step 1: Add 3 methods to the LiveDataSource interface**

In `internal/api/live_data.go`, add to the `LiveDataSource` interface (after `RunMaintenance`):

```go
	// SubmitBackfill queues a transcription backfill job.
	SubmitBackfill(ctx context.Context, filters BackfillFiltersData) (jobID, position, total int, err error)

	// BackfillStatus returns the active and queued backfill jobs.
	BackfillStatus() *BackfillStatusData

	// CancelBackfill cancels a backfill job by ID. If id <= 0, cancels all.
	CancelBackfill(id int) bool
```

Add the API-facing data types in `live_data.go`:

```go
// BackfillFiltersData contains the user-provided filters for a backfill job.
type BackfillFiltersData struct {
	SystemID  *int       `json:"system_id,omitempty"`
	Tgids     []int      `json:"tgids,omitempty"`
	StartTime *time.Time `json:"start_time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}

// BackfillStatusData reports the state of the backfill manager.
type BackfillStatusData struct {
	Active *BackfillJobData  `json:"active"`
	Queued []BackfillJobData `json:"queued"`
}

// BackfillJobData reports the state of a single backfill job.
type BackfillJobData struct {
	JobID     int                 `json:"job_id"`
	Filters   BackfillFiltersData `json:"filters"`
	Total     int                 `json:"total"`
	Completed int64               `json:"completed,omitempty"`
	Failed    int64               `json:"failed,omitempty"`
	StartedAt *time.Time          `json:"started_at,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
}
```

**Step 2: Add pipeline field and implement methods**

In `internal/ingest/pipeline.go`, add a field to the Pipeline struct:

```go
	backfill *BackfillManager
```

In `Pipeline.Start()`, after `p.transcriber.Start()`:

```go
	if p.transcriber != nil {
		p.backfill = NewBackfillManager(p.ctx, p.db, p.transcriber, p.log)
		p.backfill.Start()
	}
```

Add interface methods to pipeline.go:

```go
func (p *Pipeline) SubmitBackfill(ctx context.Context, filters api.BackfillFiltersData) (int, int, int, error) {
	if p.backfill == nil {
		return 0, 0, 0, fmt.Errorf("transcription not configured")
	}
	return p.backfill.Submit(ctx, BackfillFilters{
		SystemID:  filters.SystemID,
		Tgids:     filters.Tgids,
		StartTime: filters.StartTime,
		EndTime:   filters.EndTime,
	})
}

func (p *Pipeline) BackfillStatus() *api.BackfillStatusData {
	if p.backfill == nil {
		return nil
	}
	s := p.backfill.Status()
	result := &api.BackfillStatusData{
		Queued: make([]api.BackfillJobData, 0, len(s.Queued)),
	}
	if s.Active != nil {
		result.Active = &api.BackfillJobData{
			JobID:     s.Active.JobID,
			Filters:   api.BackfillFiltersData{SystemID: s.Active.Filters.SystemID, Tgids: s.Active.Filters.Tgids, StartTime: s.Active.Filters.StartTime, EndTime: s.Active.Filters.EndTime},
			Total:     s.Active.Total,
			Completed: s.Active.Completed,
			Failed:    s.Active.Failed,
			StartedAt: s.Active.StartedAt,
			CreatedAt: s.Active.CreatedAt,
		}
	}
	for _, q := range s.Queued {
		result.Queued = append(result.Queued, api.BackfillJobData{
			JobID:     q.JobID,
			Filters:   api.BackfillFiltersData{SystemID: q.Filters.SystemID, Tgids: q.Filters.Tgids, StartTime: q.Filters.StartTime, EndTime: q.Filters.EndTime},
			Total:     q.Total,
			CreatedAt: q.CreatedAt,
		})
	}
	return result
}

func (p *Pipeline) CancelBackfill(id int) bool {
	if p.backfill == nil {
		return false
	}
	return p.backfill.Cancel(id)
}
```

**Step 3: Update mock in tests**

In `internal/api/affiliations_test.go`, add to `mockLiveData`:

```go
func (m *mockLiveData) SubmitBackfill(context.Context, BackfillFiltersData) (int, int, int, error) { return 0, 0, 0, nil }
func (m *mockLiveData) BackfillStatus() *BackfillStatusData { return nil }
func (m *mockLiveData) CancelBackfill(int) bool { return false }
```

**Step 4: Verify it compiles**

Run: `cd /c/Users/drewm/tr-engine && go build ./...`
Expected: clean compile

**Step 5: Commit**

```
feat: wire BackfillManager into LiveDataSource and pipeline
```

---

### Task 4: Admin API endpoints

**Files:**
- Modify: `internal/api/admin.go`

**Step 1: Add 3 handler methods and register routes**

Add to `AdminHandler`:

```go
// SubmitBackfill queues a transcription backfill job.
func (h *AdminHandler) SubmitBackfill(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}

	var body BackfillFiltersData
	if r.ContentLength > 0 {
		if err := DecodeJSON(r, &body); err != nil {
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

// GetBackfillStatus returns the current backfill status.
func (h *AdminHandler) GetBackfillStatus(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}
	status := h.live.BackfillStatus()
	if status == nil {
		WriteJSON(w, http.StatusOK, map[string]any{
			"active": nil,
			"queued": []any{},
		})
		return
	}
	WriteJSON(w, http.StatusOK, status)
}

// CancelBackfill cancels a backfill job by ID, or all jobs if no ID given.
func (h *AdminHandler) CancelBackfill(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteError(w, http.StatusServiceUnavailable, "pipeline not running")
		return
	}

	idStr := chi.URLParam(r, "id")
	id := 0
	if idStr != "" {
		var err error
		id, err = strconv.Atoi(idStr)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "invalid job ID")
			return
		}
	}

	if !h.live.CancelBackfill(id) {
		WriteError(w, http.StatusNotFound, "job not found")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}
```

Update the `Routes` method — add to existing routes:

```go
	r.Post("/admin/transcribe-backfill", h.SubmitBackfill)
	r.Get("/admin/transcribe-backfill", h.GetBackfillStatus)
	r.Delete("/admin/transcribe-backfill/{id}", h.CancelBackfill)
	r.Delete("/admin/transcribe-backfill", h.CancelBackfill)
```

Add `"strconv"` to the imports if not already present.

**Step 2: Verify it compiles**

Run: `cd /c/Users/drewm/tr-engine && go build ./...`
Expected: clean compile

**Step 3: Commit**

```
feat(api): add transcription backfill admin endpoints
```

---

### Task 5: Update openapi.yaml

**Files:**
- Modify: `openapi.yaml`

**Step 1: Add the 3 new endpoints to the paths section**

Add under the admin section of `paths:`:

```yaml
  /api/v1/admin/transcribe-backfill:
    post:
      operationId: submitTranscribeBackfill
      summary: Submit transcription backfill job
      description: >-
        Queues a background job to find untranscribed calls matching the given
        filters and drip-feed them into the transcription queue. Jobs are
        processed sequentially — if a job is already running, the new job is
        queued behind it. All filter fields are optional; omit everything to
        backfill all untranscribed calls.
      tags: [admin]
      security:
        - BearerAuth: []
      requestBody:
        required: false
        content:
          application/json:
            schema:
              type: object
              properties:
                system_id:
                  type: integer
                  description: Filter to a specific system
                tgids:
                  type: array
                  items:
                    type: integer
                  description: Filter to specific talkgroup IDs
                start_time:
                  type: string
                  format: date-time
                  description: Only backfill calls after this time
                end_time:
                  type: string
                  format: date-time
                  description: Only backfill calls before this time
      responses:
        '202':
          description: Job accepted
          content:
            application/json:
              schema:
                type: object
                properties:
                  job_id:
                    type: integer
                  position:
                    type: integer
                    description: 0 if immediately active, otherwise queue position
                  total:
                    type: integer
                    description: Number of untranscribed calls matching filters
                  filters:
                    $ref: '#/components/schemas/BackfillFilters'
        '503':
          description: Transcription not configured or pipeline not running
    get:
      operationId: getTranscribeBackfillStatus
      summary: Get transcription backfill status
      description: Returns the currently active backfill job (if any) and queued jobs.
      tags: [admin]
      security:
        - BearerAuth: []
      responses:
        '200':
          description: Backfill status
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/BackfillStatus'
    delete:
      operationId: cancelAllTranscribeBackfill
      summary: Cancel all backfill jobs
      description: Cancels the active job and removes all queued jobs.
      tags: [admin]
      security:
        - BearerAuth: []
      responses:
        '200':
          description: All jobs cancelled
        '404':
          description: No jobs to cancel

  /api/v1/admin/transcribe-backfill/{job_id}:
    delete:
      operationId: cancelTranscribeBackfill
      summary: Cancel a specific backfill job
      description: >-
        Cancels a specific backfill job by ID. If the job is currently active,
        it is stopped and the next queued job begins. If the job is queued,
        it is removed from the queue.
      tags: [admin]
      security:
        - BearerAuth: []
      parameters:
        - name: job_id
          in: path
          required: true
          schema:
            type: integer
      responses:
        '200':
          description: Job cancelled
        '404':
          description: Job not found
```

Add to the `components/schemas` section:

```yaml
    BackfillFilters:
      type: object
      properties:
        system_id:
          type: integer
        tgids:
          type: array
          items:
            type: integer
        start_time:
          type: string
          format: date-time
        end_time:
          type: string
          format: date-time

    BackfillJob:
      type: object
      properties:
        job_id:
          type: integer
        filters:
          $ref: '#/components/schemas/BackfillFilters'
        total:
          type: integer
        completed:
          type: integer
        failed:
          type: integer
        started_at:
          type: string
          format: date-time
          nullable: true
        created_at:
          type: string
          format: date-time

    BackfillStatus:
      type: object
      properties:
        active:
          nullable: true
          allOf:
            - $ref: '#/components/schemas/BackfillJob'
        queued:
          type: array
          items:
            $ref: '#/components/schemas/BackfillJob'
```

**Step 2: Commit**

```
docs(openapi): add transcription backfill endpoints
```

---

### Task 6: Build and smoke test

**Step 1: Full build**

Run: `cd /c/Users/drewm/tr-engine && go build ./...`
Expected: clean compile

**Step 2: Run existing tests**

Run: `cd /c/Users/drewm/tr-engine && go test ./internal/api/... ./internal/ingest/... ./internal/database/...`
Expected: all pass (the mock update in Task 3 should keep `affiliations_test.go` happy)

**Step 3: Manual smoke test against live instance**

Deploy and test the 3 endpoints:

```bash
# Check status (should be idle)
curl -H "Authorization: Bearer $WRITE_TOKEN" \
  http://tr-dashboard.pizzly-manta.ts.net:8000/api/v1/admin/transcribe-backfill

# Submit a small backfill (last 24h)
curl -X POST -H "Authorization: Bearer $WRITE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"start_time":"2026-03-06T00:00:00Z"}' \
  http://tr-dashboard.pizzly-manta.ts.net:8000/api/v1/admin/transcribe-backfill

# Check progress
curl -H "Authorization: Bearer $WRITE_TOKEN" \
  http://tr-dashboard.pizzly-manta.ts.net:8000/api/v1/admin/transcribe-backfill

# Cancel
curl -X DELETE -H "Authorization: Bearer $WRITE_TOKEN" \
  http://tr-dashboard.pizzly-manta.ts.net:8000/api/v1/admin/transcribe-backfill
```

**Step 4: Commit any fixes, then final commit**

```
feat: transcription backfill API complete
```
