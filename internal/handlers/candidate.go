package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

type CandidateHandler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client
}

func NewCandidateHandler(pool *pgxpool.Pool, sheets *sheets.Client) *CandidateHandler {
	return &CandidateHandler{Pool: pool, Sheets: sheets}
}

func candidateID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(appmiddleware.UserIDKey).(int64)
	return id, ok
}

type assignmentView struct {
	QueryID      *int64    `json:"query_id,omitempty"`
	AssignmentID int64     `json:"assignment_id"`
	Status       string    `json:"status"`
	RoundNumber  int16     `json:"round_number"`
	LocationName string    `json:"location_name"`
	StartTime    time.Time `json:"start_time"`
	DurationMin  int       `json:"duration_min"`
	Team         *string   `json:"team,omitempty"`
	QueryStatus  *string   `json:"query_status,omitempty"` // null if no query raised
}

func (h *CandidateHandler) MyAssignments(w http.ResponseWriter, r *http.Request) {
	uid, ok := candidateID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT a.id, a.status, ro.number, l.name, s.start_time, s.duration_min, a.team,
		       (SELECT q.id FROM queries q WHERE q.assignment_id = a.id ORDER BY q.created_at DESC LIMIT 1),
		       (SELECT q.status FROM queries q WHERE q.assignment_id = a.id ORDER BY q.created_at DESC LIMIT 1)
		FROM assignments a
		JOIN slots s ON s.id = a.slot_id
		JOIN locations l ON l.id = s.location_id
		JOIN rounds ro ON ro.id = s.round_id
		WHERE a.candidate_id = $1 AND ro.is_active = true
		ORDER BY s.start_time DESC
	`, uid)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	results := []assignmentView{}
	for rows.Next() {
		var a assignmentView
		if err := rows.Scan(&a.AssignmentID, &a.Status, &a.RoundNumber, &a.LocationName,
			&a.StartTime, &a.DurationMin, &a.Team, &a.QueryID, &a.QueryStatus); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, a)
	}

	json.NewEncoder(w).Encode(results)
}

type raiseQueryRequest struct {
	AssignmentID int64  `json:"assignment_id"`
	Reason       string `json:"reason"`
}

// RaiseQuery lets a candidate flag a problem with their assigned slot.
// Marks the assignment pending_query so the admin queue picks it up.
func (h *CandidateHandler) RaiseQuery(w http.ResponseWriter, r *http.Request) {
	uid, ok := candidateID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req raiseQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		http.Error(w, "reason is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	// Ownership + state check: only the candidate who owns this assignment
	// can raise a query, and only while it's still confirmed (no piling on
	// duplicate queries against the same assignment).
	tag, err := tx.Exec(ctx, `
		UPDATE assignments SET status = 'pending_query'
		WHERE id = $1 AND candidate_id = $2 AND status = 'confirmed'
	`, req.AssignmentID, uid)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "assignment not found, not yours, or already has a pending query", http.StatusConflict)
		return
	}

	var queryID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO queries (assignment_id, reason, status)
		VALUES ($1, $2, 'pending')
		RETURNING id
	`, req.AssignmentID, req.Reason).Scan(&queryID)
	if err != nil {
		http.Error(w, "failed to create query", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}
	if h.Sheets != nil {
		var email string
		h.Pool.QueryRow(ctx, `SELECT campus_email FROM users WHERE id = $1`, uid).Scan(&email)
		go h.Sheets.LogEvent(context.Background(), "Queries", email, "query_raised", req.Reason)
	}
	json.NewEncoder(w).Encode(map[string]int64{"query_id": queryID})
}

var _ = strconv.Itoa // placeholder if unused later

type submitUnavailabilityRequest struct {
	RoundID          int64    `json:"round_id"`
	UnavailableDates []string `json:"unavailable_dates"` // ["2026-08-16", "2026-08-20"]
	Note             string   `json:"note,omitempty"`
}

// SubmitUnavailability lets a candidate flag dates they can't attend for a
// round, once it's been announced - upserts, so resubmitting replaces their
// previous answer rather than piling up entries.
func (h *CandidateHandler) SubmitUnavailability(w http.ResponseWriter, r *http.Request) {
	uid, ok := candidateID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req submitUnavailabilityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if blockIfRoundInactive(ctx, h.Pool, w, r, req.RoundID) {
		return
	}
	if len(req.UnavailableDates) == 0 {
		http.Error(w, "unavailable_dates cannot be empty", http.StatusBadRequest)
		return
	}
	for _, d := range req.UnavailableDates {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			http.Error(w, "dates must be YYYY-MM-DD, got: "+d, http.StatusBadRequest)
			return
		}
	}

	_, err := h.Pool.Exec(ctx, `
		INSERT INTO candidate_unavailability (candidate_id, round_id, unavailable_dates, note)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (candidate_id, round_id)
		DO UPDATE SET unavailable_dates = EXCLUDED.unavailable_dates,
		              note = EXCLUDED.note,
		              submitted_at = now()
	`, uid, req.RoundID, req.UnavailableDates, req.Note)
	if err != nil {
		http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if h.Sheets != nil {
		go h.syncUnavailabilityToSheet(context.Background(), uid, req.RoundID)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *CandidateHandler) syncUnavailabilityToSheet(ctx context.Context, candidateID, roundID int64) {
	var name, email, note string
	var dates []string
	var roundNumber int16
	var submittedAt time.Time

	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name, ''), u.campus_email, cu.unavailable_dates::text[], COALESCE(cu.note, ''), ro.number, cu.submitted_at
		FROM candidate_unavailability cu
		JOIN users u ON u.id = cu.candidate_id
		JOIN rounds ro ON ro.id = cu.round_id
		WHERE cu.candidate_id = $1 AND cu.round_id = $2
	`, candidateID, roundID).Scan(&name, &email, &dates, &note, &roundNumber, &submittedAt)
	if err != nil {
		return
	}

	// Composite key so the same candidate can have separate rows per round
	// on one shared tab without UpsertRow's email-based matching colliding.
	key := fmt.Sprintf("%s-R%d", name, roundNumber)
	row := []interface{}{
		key, name, email, roundNumber, strings.Join(dates, ", "), note, submittedAt.Format(time.RFC3339),
	}
	h.Sheets.UpsertRowAtColumn(ctx, "Unavailability", "B", key, row)
}

// MyUnavailability returns what the candidate has submitted so far, so
// they can verify it before/after editing.
func (h *CandidateHandler) MyUnavailability(w http.ResponseWriter, r *http.Request) {
	uid, ok := candidateID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT ro.number, cu.unavailable_dates::text[], COALESCE(cu.note, ''), cu.submitted_at
		FROM candidate_unavailability cu
		JOIN rounds ro ON ro.id = cu.round_id
		WHERE cu.candidate_id = $1
		ORDER BY ro.number
	`, uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type view struct {
		RoundNumber int16     `json:"round_number"`
		Dates       []string  `json:"unavailable_dates"`
		Note        string    `json:"note"`
		SubmittedAt time.Time `json:"submitted_at"`
	}

	results := []view{}
	for rows.Next() {
		var v view
		if err := rows.Scan(&v.RoundNumber, &v.Dates, &v.Note, &v.SubmittedAt); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}

	json.NewEncoder(w).Encode(results)
}

// CancelQuery lets a candidate withdraw a pending query themselves - reverts
// the assignment back to confirmed, same as if it were never raised.
func (h *CandidateHandler) CancelQuery(w http.ResponseWriter, r *http.Request) {
	uid, ok := candidateID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	queryID, err := strconv.ParseInt(chi.URLParam(r, "queryID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid query id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var assignmentID int64
	err = tx.QueryRow(ctx, `
		SELECT q.assignment_id FROM queries q
		JOIN assignments a ON a.id = q.assignment_id
		WHERE q.id = $1 AND a.candidate_id = $2 AND q.status = 'pending'
	`, queryID, uid).Scan(&assignmentID)
	if err != nil {
		http.Error(w, "query not found, not yours, or already resolved", http.StatusNotFound)
		return
	}

	if _, err := tx.Exec(ctx, `DELETE FROM queries WHERE id = $1`, queryID); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE assignments SET status = 'confirmed' WHERE id = $1`, assignmentID); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AcknowledgeResult marks a round's advancement/elimination status as seen,
// so the dashboard toast doesn't fire again on future visits.
func (h *CandidateHandler) AcknowledgeResult(w http.ResponseWriter, r *http.Request) {
	uid, ok := candidateID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Round int `json:"round"` // 1 or 2
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Round != 1 && req.Round != 2) {
		http.Error(w, "round must be 1 or 2", http.StatusBadRequest)
		return
	}

	col := "round1_result_seen"
	if req.Round == 2 {
		col = "round2_result_seen"
	}
	_, err := h.Pool.Exec(r.Context(), fmt.Sprintf(`UPDATE users SET %s = true WHERE id = $1`, col), uid)
	// safe: col is one of two hardcoded literals above, never derived from request input
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
