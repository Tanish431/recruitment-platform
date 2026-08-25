package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Tanish431/recruitment-platform/internal/sheets"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type QueryHandler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client
}

func NewQueryHandler(pool *pgxpool.Pool, sheets *sheets.Client) *QueryHandler {
	return &QueryHandler{Pool: pool, Sheets: sheets}
}

type pendingQueryView struct {
	QueryID         int64  `json:"query_id"`
	AssignmentID    int64  `json:"assignment_id"`
	SlotID          int64  `json:"slot_id"`
	CandidateID     int64  `json:"candidate_id"`
	CandidateName   string `json:"candidate_name"`
	CandidateBitsID string `json:"candidate_bits_id"`
	CandidateEmail  string `json:"candidate_email"`
	Reason          string `json:"reason"`
	RoundID         int64  `json:"round_id"`
	RoundNumber     int16  `json:"round_number"`
	LocationName    string `json:"location_name"`
	StartTimeISO    string `json:"start_time"`
}

// ListPending shows every open query for admins to work through.
func (h *QueryHandler) ListPending(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Pool.Query(r.Context(), `
		SELECT q.id, a.id, a.slot_id, a.candidate_id, COALESCE(u.name,''), COALESCE(u.bits_id,''), u.campus_email, q.reason,
		       ro.id, ro.number, l.name, s.start_time::text
		FROM queries q
		JOIN assignments a ON a.id = q.assignment_id
		JOIN users u ON u.id = a.candidate_id
		JOIN slots s ON s.id = a.slot_id
		JOIN locations l ON l.id = s.location_id
		JOIN rounds ro ON ro.id = s.round_id
		WHERE q.status = 'pending'
		ORDER BY q.created_at ASC
	`)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	results := []pendingQueryView{}
	for rows.Next() {
		var v pendingQueryView
		if err := rows.Scan(
			&v.QueryID, &v.AssignmentID, &v.SlotID, &v.CandidateID,
			&v.CandidateName, &v.CandidateBitsID, &v.CandidateEmail, &v.Reason,
			&v.RoundID, &v.RoundNumber, &v.LocationName, &v.StartTimeISO,
		); err != nil {
			http.Error(w, "scan failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}

	json.NewEncoder(w).Encode(results)
}

type resolveQueryRequest struct {
	Resolution string `json:"resolution"` // "swap" | "reassign"
	// For swap: the other assignment to trade slots with.
	SwapWithAssignmentID *int64 `json:"swap_with_assignment_id,omitempty"`
	// For reassign: the new slot to move the candidate into.
	NewSlotID *int64 `json:"new_slot_id,omitempty"`
	Note      string `json:"note"`
}

// Resolve handles both query resolution paths: swap (trade slots between
// two candidates - the Round 2 pattern, since every slot sits at exactly
// capacity) or reassign (move into a slot with free capacity - the
// Round 1/3 pattern).
func (h *QueryHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	adminID, ok := candidateID(r) // same context key, works for any authed user
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	queryIDStr := chi.URLParam(r, "queryID")
	queryID, err := strconv.ParseInt(queryIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid query id", http.StatusBadRequest)
		return
	}

	var req resolveQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
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
	var qStatus string
	err = tx.QueryRow(ctx, `
		SELECT assignment_id, status FROM queries WHERE id = $1 FOR UPDATE
	`, queryID).Scan(&assignmentID, &qStatus)
	if err != nil {
		http.Error(w, "query not found", http.StatusNotFound)
		return
	}
	if qStatus != "pending" {
		http.Error(w, "query already resolved", http.StatusConflict)
		return
	}

	switch req.Resolution {
	case "swap":
		if req.SwapWithAssignmentID == nil {
			http.Error(w, "swap_with_assignment_id required for swap resolution", http.StatusBadRequest)
			return
		}
		if err := swapAssignments(ctx, tx, assignmentID, *req.SwapWithAssignmentID); err != nil {
			http.Error(w, "swap failed: "+err.Error(), http.StatusConflict)
			return
		}
		_, err = tx.Exec(ctx, `
			UPDATE queries SET status = 'resolved', resolution_type = 'swap',
			       swapped_with_assignment_id = $1, resolved_by = $2, resolved_at = now()
			WHERE id = $3
		`, *req.SwapWithAssignmentID, adminID, queryID)

	case "reassign":
		if req.NewSlotID == nil {
			http.Error(w, "new_slot_id required for reassign resolution", http.StatusBadRequest)
			return
		}
		if err := reassignAssignment(ctx, tx, assignmentID, *req.NewSlotID); err != nil {
			http.Error(w, "reassign failed: "+err.Error(), http.StatusConflict)
			return
		}
		_, err = tx.Exec(ctx, `
			UPDATE queries SET status = 'resolved', resolution_type = 'reassign',
			       resolved_by = $1, resolved_at = now()
			WHERE id = $2
		`, adminID, queryID)

	default:
		http.Error(w, "resolution must be 'swap' or 'reassign'", http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, "failed to finalize query resolution: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}
	if h.Sheets != nil {
		var email string
		h.Pool.QueryRow(context.Background(), `
			SELECT u.campus_email FROM assignments a JOIN users u ON u.id = a.candidate_id WHERE a.id = $1
		`, assignmentID).Scan(&email)
		go h.Sheets.LogEvent(context.Background(), "Queries", email, "query_resolved_"+req.Resolution, req.Note)
	}
	w.WriteHeader(http.StatusNoContent)
}

// OpenSlotsForRound lists slots with free capacity - for the reassign dropdown.
func (h *QueryHandler) OpenSlotsForRound(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id required", http.StatusBadRequest)
		return
	}
	rows, err := h.Pool.Query(r.Context(), `
		SELECT s.id, l.name, s.start_time, s.capacity - s.filled_count
		FROM slots s JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1 AND s.filled_count < s.capacity
		ORDER BY s.start_time
	`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type sv struct {
		ID       int64     `json:"id"`
		Location string    `json:"location_name"`
		Start    time.Time `json:"start_time"`
		Free     int       `json:"free_capacity"`
	}
	results := []sv{}
	for rows.Next() {
		var v sv
		if err := rows.Scan(&v.ID, &v.Location, &v.Start, &v.Free); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}
	json.NewEncoder(w).Encode(results)
}

// OtherAssignmentsForRound lists other candidates' assignments in the same
// round - for the swap dropdown, so admin picks by name, not ID.
func (h *QueryHandler) OtherAssignmentsForRound(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id required", http.StatusBadRequest)
		return
	}
	excludeAssignmentID, _ := strconv.ParseInt(r.URL.Query().Get("exclude_assignment_id"), 10, 64)

	rows, err := h.Pool.Query(r.Context(), `
		SELECT a.id, COALESCE(u.name,''), COALESCE(u.bits_id,''), u.campus_email, l.name, s.start_time
		FROM assignments a
		JOIN users u ON u.id = a.candidate_id
		JOIN slots s ON s.id = a.slot_id
		JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1 AND a.id != $2
		ORDER BY s.start_time
	`, roundID, excludeAssignmentID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type av struct {
		AssignmentID int64     `json:"assignment_id"`
		Name         string    `json:"candidate_name"`
		BitsID       string    `json:"candidate_bits_id"`
		Email        string    `json:"candidate_email"`
		Location     string    `json:"location_name"`
		Start        time.Time `json:"start_time"`
	}
	results := []av{}
	for rows.Next() {
		var v av
		if err := rows.Scan(&v.AssignmentID, &v.Name, &v.BitsID, &v.Email, &v.Location, &v.Start); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}
	json.NewEncoder(w).Encode(results)
}

// AdminCancelQuery lets an admin dismiss a query without swap/reassign -
// reverts the assignment to confirmed, same effect as the candidate
// cancelling their own, but admin-initiated.
func (h *QueryHandler) AdminCancelQuery(w http.ResponseWriter, r *http.Request) {
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
	err = tx.QueryRow(ctx, `SELECT assignment_id FROM queries WHERE id = $1 AND status = 'pending'`, queryID).Scan(&assignmentID)
	if err != nil {
		http.Error(w, "query not found or already resolved", http.StatusNotFound)
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
