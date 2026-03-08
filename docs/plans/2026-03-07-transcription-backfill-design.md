# Transcription Backfill API

## Problem

tr-engine only enqueues transcriptions at ingest time. If the transcriber was down, the queue was full, or the STT provider failed, those calls have no transcription and no automatic way to retry. The only recovery path is manually calling `POST /calls/{id}/transcribe` one at a time.

## Solution

A background drip-feed backfill system that finds untranscribed calls and feeds them into the existing transcription queue at a pace that doesn't delay real-time transcriptions.

## API

All endpoints require `WRITE_TOKEN`. Mounted under `/api/v1/admin/`.

### POST /admin/transcribe-backfill

Submit a backfill job. All filter fields are optional — omit everything to backfill all untranscribed calls.

**Request:**
```json
{
  "system_id": 1,
  "tgids": [500, 501],
  "start_time": "2026-03-01T00:00:00Z",
  "end_time": "2026-03-07T00:00:00Z"
}
```

**Response (202):**
```json
{
  "job_id": 1,
  "position": 0,
  "total": 1200,
  "filters": {"system_id": 1, "tgids": [500, 501], "start_time": "...", "end_time": "..."}
}
```

`position` is 0 if the job is immediately active, or the queue position if another job is running.

### GET /admin/transcribe-backfill

Returns the active job (if any) and queued jobs.

**Response:**
```json
{
  "active": {
    "job_id": 1,
    "filters": {"system_id": 1},
    "total": 1200,
    "completed": 450,
    "failed": 3,
    "started_at": "2026-03-07T10:00:00Z"
  },
  "queued": [
    {"job_id": 2, "filters": {"tgids": [500]}, "total": 85}
  ]
}
```

`active` is null when idle. `total` is a snapshot taken at job start.

### DELETE /admin/transcribe-backfill/{job_id}

Cancel a specific job. Removes from queue if pending, or cancels if active (advances to next queued job). Returns 404 if job_id not found.

### DELETE /admin/transcribe-backfill

Cancel all jobs (active + queued).

## Architecture

### BackfillManager

Lives in `internal/ingest/backfill.go`. Owns a single goroutine that processes jobs sequentially from a queue.

**Job lifecycle:**
1. Job submitted via API -> added to queue (slice protected by mutex)
2. Goroutine picks up next job, queries DB for total count
3. Fetches untranscribed call IDs in batches of 100 (cursor-based, `start_time DESC`)
4. Drip-feeds each call into the transcription queue
5. On completion or cancel, advances to next queued job

### Pacing

The drip loop checks the transcription queue depth before each enqueue. Only enqueues when `pending <= 1`. Otherwise sleeps 500ms and re-checks. This keeps the queue nearly empty for real-time ingest transcriptions.

### Cancellation

- Context cancellation drops out of the drip loop immediately
- The 1-2 calls already in the transcription queue finish normally
- On service shutdown, backfill stops via the pipeline's context. Progress is not persisted — resubmit after restart.

### Integration

The `BackfillManager` is created by the pipeline on startup. The API layer talks to it through 3 new methods on the `LiveDataSource` interface:

- `SubmitBackfill(filters) -> (jobID, position, total, error)`
- `BackfillStatus() -> (active, queued)`
- `CancelBackfill(jobID) -> error`

This follows the same pattern as the maintenance system.

## Data Layer

Two new queries in `queries_calls.go`:

**ListUntranscribedCallIDs** — returns call IDs matching filters, batched:
```sql
SELECT call_id FROM calls
WHERE has_transcription = false
  AND NOT encrypted
  AND duration > $min_duration
  AND ($system_id IS NULL OR system_id = $system_id)
  AND ($tgids IS NULL OR tgid = ANY($tgids))
  AND ($start_time IS NULL OR start_time >= $start_time)
  AND ($end_time IS NULL OR start_time < $end_time)
ORDER BY start_time DESC
LIMIT 100 OFFSET $offset
```

**CountUntranscribedCalls** — same WHERE clause with `SELECT count(*)`. Run once at job start for the `total` field.

No new tables or schema changes. The existing `idx_calls_has_transcription` index covers the core filter. Min/max duration thresholds come from the existing transcriber config.

## Files

**New:**
- `internal/ingest/backfill.go` — BackfillManager

**Modified:**
- `internal/api/live_data.go` — 3 new LiveDataSource methods
- `internal/api/admin.go` — 3 handler methods + routes
- `internal/ingest/pipeline.go` — create BackfillManager, implement interface methods
- `internal/database/queries_calls.go` — 2 new query functions
- `openapi.yaml` — document 3 new endpoints
