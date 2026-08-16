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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

type EvaluationHandler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client
}

func NewEvaluationHandler(pool *pgxpool.Pool, sheetsClient *sheets.Client) *EvaluationHandler {
	return &EvaluationHandler{Pool: pool, Sheets: sheetsClient}
}

func judgeID(r *http.Request) (int64, bool) {
	id, ok := r.Context().Value(appmiddleware.UserIDKey).(int64)
	return id, ok
}

type queueItem struct {
	EvaluationID   int64      `json:"evaluation_id"`
	CandidateID    int64      `json:"candidate_id"`
	CandidateName  string     `json:"candidate_name"`
	CandidateEmail string     `json:"candidate_email"`
	SlotID         int64      `json:"slot_id"`
	SlotStart      time.Time  `json:"slot_start"`
	Status         string     `json:"status"`
	CheckedInAt    *time.Time `json:"checked_in_at,omitempty"`
	SkipCount      int        `json:"skip_count"`
}

func (h *EvaluationHandler) Queue(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT e.id, e.candidate_id, COALESCE(u.name,''), u.campus_email, e.slot_id, s.start_time,
		       e.status, e.checked_in_at, e.skip_count
		FROM evaluations e
		JOIN slots s ON s.id = e.slot_id
		JOIN users u ON u.id = e.candidate_id
		WHERE s.round_id = $1
		AND (e.status = 'checked_in' OR (e.status = 'in_progress' AND e.judge_id = $2))
		ORDER BY e.checked_in_at ASC NULLS LAST
	`, roundID, uid)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []queueItem{}
	for rows.Next() {
		var it queueItem
		if err := rows.Scan(&it.EvaluationID, &it.CandidateID, &it.CandidateName, &it.CandidateEmail,
			&it.SlotID, &it.SlotStart, &it.Status, &it.CheckedInAt, &it.SkipCount); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		items = append(items, it)
	}
	json.NewEncoder(w).Encode(items)
}

// CheckIn marks a candidate as arrived. Any judge at the station can do
// this - it's a shared fact, not tied to who ends up interviewing them.
func (h *EvaluationHandler) CheckIn(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE evaluations
		SET status = 'checked_in', checked_in_at = now()
		WHERE id = $1 AND status = 'not_arrived'
	`, id)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "evaluation not found or already checked in", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Claim atomically assigns a checked-in candidate to the requesting judge.
// The WHERE status='checked_in' guard is what prevents two judges claiming
// the same candidate under a race - if this UPDATE affects 0 rows, someone
// else got there first.
func (h *EvaluationHandler) Claim(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE evaluations
		SET status = 'in_progress', judge_id = $1, started_at = now()
		WHERE id = $2 AND status = 'checked_in'
	`, uid, id)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "candidate already claimed by another judge", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type submitRequest struct {
	Score    int    `json:"score"`
	Comments string `json:"comments"`
}

// Submit finalizes an interview the requesting judge is currently running.
func (h *EvaluationHandler) Submit(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}

	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Score < 1 || req.Score > 5 {
		http.Error(w, "score must be between 1 and 5", http.StatusBadRequest)
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE evaluations
		SET status = 'completed',
		    score = $1,
		    comments = $2,
		    completed_at = now(),
		    duration_seconds = EXTRACT(EPOCH FROM (now() - started_at))::int
		WHERE id = $3 AND status = 'in_progress' AND judge_id = $4
	`, req.Score, req.Comments, id, uid)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "evaluation not found, not yours, or not in progress", http.StatusConflict)
		return
	}
	if h.Sheets != nil {
		go h.syncToSheet(context.Background(), id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Skip reverts an in-progress interview back to the shared queue, so
// another judge (or the same one later) can pick it up.
func (h *EvaluationHandler) Skip(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE evaluations
		SET status = 'checked_in', judge_id = NULL, started_at = NULL,
		    skip_count = skip_count + 1
		WHERE id = $1 AND status = 'in_progress' AND judge_id = $2
	`, id, uid)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "evaluation not found, not yours, or not in progress", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// NoShow marks a candidate who never checked in as absent, once their
// slot window has clearly passed. Terminal - no judge required.
func (h *EvaluationHandler) NoShow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE evaluations
		SET status = 'no_show'
		WHERE id = $1 AND status IN ('not_arrived', 'checked_in')
	`, id)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "evaluation not found or already in progress/completed", http.StatusConflict)
		return
	}
	if h.Sheets != nil {
		go h.syncToSheet(context.Background(), id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *EvaluationHandler) syncToSheet(ctx context.Context, evaluationID int64) {
	if h.Sheets == nil {
		return
	}

	var email, name, locationName, status string
	var startTime time.Time
	var score *int
	var comments *string
	var roundNumber int16

	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name, ''), u.campus_email, l.name, s.start_time, ro.number, e.status, e.score, e.comments
		FROM evaluations e
		JOIN users u ON u.id = e.candidate_id
		JOIN slots s ON s.id = e.slot_id
		JOIN locations l ON l.id = s.location_id
		JOIN rounds ro ON ro.id = s.round_id
		WHERE e.id = $1
	`, evaluationID).Scan(&name, &email, &locationName, &startTime, &roundNumber, &status, &score, &comments)
	if err != nil {
		return
	}

	scoreStr, commentsStr := "", ""
	if score != nil {
		scoreStr = fmt.Sprint(*score)
	}
	if comments != nil {
		commentsStr = *comments
	}
	ratingsStr := h.formatPropertyRatings(ctx, "evaluation_property_ratings", "evaluation_id", evaluationID)

	tab := fmt.Sprintf("Round%d", roundNumber)
	row := []interface{}{
		name, email, locationName, startTime.Format(time.RFC3339),
		status, scoreStr, ratingsStr, commentsStr, time.Now().Format(time.RFC3339),
	}
	h.Sheets.UpsertRowAtColumn(ctx, tab, "B", email, row)
}

func (h *EvaluationHandler) formatPropertyRatings(ctx context.Context, table, fkCol string, id int64) string {
	rows, err := h.Pool.Query(ctx, fmt.Sprintf(`
		SELECT rp.name, t.rating::text FROM %s t
		JOIN round_scoring_properties rp ON rp.id = t.property_id
		WHERE t.%s = $1 ORDER BY rp.position
	`, table, fkCol), id)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var name, rating string
		rows.Scan(&name, &rating)
		parts = append(parts, name+": "+rating)
	}
	return strings.Join(parts, ", ")
}

// LookupByEmail finds a candidate's evaluation row for a round by email,
// so a front-desk judge can check someone in without knowing their
// evaluation_id in advance.
func (h *EvaluationHandler) LookupByEmail(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}
	email := r.URL.Query().Get("email")
	if email == "" {
		http.Error(w, "email query param required", http.StatusBadRequest)
		return
	}

	var result struct {
		EvaluationID int64  `json:"evaluation_id"`
		CandidateID  int64  `json:"candidate_id"`
		Status       string `json:"status"`
		LocationName string `json:"location_name"`
		SlotStart    string `json:"slot_start"`
	}

	err = h.Pool.QueryRow(r.Context(), `
		SELECT e.id, e.candidate_id, e.status, l.name, s.start_time::text
		FROM evaluations e
		JOIN users u ON u.id = e.candidate_id
		JOIN slots s ON s.id = e.slot_id
		JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1 AND u.campus_email = $2 AND u.is_active = true
	`, roundID, email).Scan(&result.EvaluationID, &result.CandidateID, &result.Status, &result.LocationName, &result.SlotStart)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "no evaluation found for this candidate in this round", http.StatusNotFound)
			return
		}
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(result)
}
