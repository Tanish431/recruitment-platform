package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

type Round2Handler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client
}

func NewRound2Handler(pool *pgxpool.Pool, sheetsClient *sheets.Client) *Round2Handler {
	return &Round2Handler{Pool: pool, Sheets: sheetsClient}
}

// AvailableSlots lists open (location, time) combos a judge can still claim
// for round 2 — only shown while the round's slot_creation_open flag is true.
func (h *Round2Handler) AvailableSlots(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}
	if blockIfRoundInactive(r.Context(), h.Pool, w, r, roundID) { // or roundID, matching whichever var name is local
		return
	}

	var open bool
	if err := h.Pool.QueryRow(r.Context(), `SELECT slot_creation_open FROM rounds WHERE id = $1`, roundID).Scan(&open); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}
	if !open {
		http.Error(w, "slot creation is closed for this round", http.StatusForbidden)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT s.id, s.location_id, l.name, s.start_time, s.duration_min
		FROM slots s
		JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1 AND s.created_by IS NULL
		ORDER BY s.start_time
	`, roundID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type slotView struct {
		ID           int64     `json:"id"`
		LocationID   int64     `json:"location_id"`
		LocationName string    `json:"location_name"`
		StartTime    time.Time `json:"start_time"`
		DurationMin  int       `json:"duration_min"`
	}

	slots := []slotView{}
	for rows.Next() {
		var s slotView
		if err := rows.Scan(&s.ID, &s.LocationID, &s.LocationName, &s.StartTime, &s.DurationMin); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		slots = append(slots, s)
	}

	json.NewEncoder(w).Encode(slots)
}

type claimSlotRequest struct {
	RoundID     int64  `json:"round_id"`
	LocationID  int64  `json:"location_id"`
	StartTime   string `json:"start_time"` // RFC3339
	DurationMin int    `json:"duration_min"`
}

// ClaimSlot lets a judge create-and-claim a (location, time) slot in one
// step. The DB's uq_slot_location_time constraint is the actual FCFS
// enforcement — this handler just surfaces a clean 409 when it fires.
func (h *Round2Handler) ClaimSlot(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req claimSlotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.DurationMin < 1 {
		req.DurationMin = 75 // default to 1hr15 per the debate format
	}
	if blockIfRoundInactive(r.Context(), h.Pool, w, r, req.RoundID) { // or roundID, matching whichever var name is local
		return
	}
	var open bool
	if err := h.Pool.QueryRow(r.Context(), `SELECT slot_creation_open FROM rounds WHERE id = $1`, req.RoundID).Scan(&open); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}
	if !open {
		http.Error(w, "slot creation is closed for this round", http.StatusForbidden)
		return
	}

	startTime, err := time.Parse(time.RFC3339, req.StartTime)
	if err != nil {
		http.Error(w, "start_time must be RFC3339", http.StatusBadRequest)
		return
	}

	row := h.Pool.QueryRow(r.Context(), `
		INSERT INTO slots (round_id, location_id, start_time, duration_min, capacity, created_by, claimed_at)
		VALUES ($1, $2, $3, $4, 6, $5, now())
		RETURNING id
	`, req.RoundID, req.LocationID, startTime, req.DurationMin, uid)

	var id int64
	if err := row.Scan(&id); err != nil {
		// unique constraint fires here — someone else claimed this exact
		// (location, time) first. This IS the FCFS mechanism.
		http.Error(w, "slot already claimed by another judge", http.StatusConflict)
		return
	}

	json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

// Participants lists the 6 candidates in a slot the judge is hosting,
// with current attendance/score state — the judge's scoring screen.
func (h *Round2Handler) Participants(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}

	var hostID int64
	if err := h.Pool.QueryRow(r.Context(), `SELECT created_by FROM slots WHERE id = $1`, slotID).Scan(&hostID); err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}
	if hostID != uid {
		http.Error(w, "you did not claim this slot", http.StatusForbidden)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT dp.id, dp.candidate_id, COALESCE(u.name,''), u.campus_email, dp.team, dp.attendance, dp.score, dp.comments
		FROM debate_participants dp
		JOIN users u ON u.id = dp.candidate_id
		WHERE dp.slot_id = $1 AND u.is_active = true
		ORDER BY dp.team, u.campus_email
	`, slotID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type participantView struct {
		ID             int64   `json:"id"`
		CandidateID    int64   `json:"candidate_id"`
		CandidateName  string  `json:"candidate_name"`
		CandidateEmail string  `json:"candidate_email"`
		Team           string  `json:"team"`
		Attendance     string  `json:"attendance"`
		Score          *int    `json:"score,omitempty"`
		Comments       *string `json:"comments,omitempty"`
	}

	participants := []participantView{}
	for rows.Next() {
		var p participantView
		if err := rows.Scan(&p.ID, &p.CandidateID, &p.CandidateName, &p.CandidateEmail, &p.Team, &p.Attendance, &p.Score, &p.Comments); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		participants = append(participants, p)
	}

	json.NewEncoder(w).Encode(participants)
}

type attendanceRequest struct {
	Attendance string `json:"attendance"` // "present" | "no_show"
}

func (h *Round2Handler) SetAttendance(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid participant id", http.StatusBadRequest)
		return
	}

	var req attendanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Attendance != "present" && req.Attendance != "no_show" {
		http.Error(w, "attendance must be 'present' or 'no_show'", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE debate_participants dp
		SET attendance = $1
		FROM slots s
		WHERE dp.id = $2 AND dp.slot_id = s.id AND s.created_by = $3
	`, req.Attendance, id, uid)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "participant not found or not your slot", http.StatusForbidden)
		return
	}
	if h.Sheets != nil {
		go h.syncParticipantToSheet(context.Background(), id)
	}

	w.WriteHeader(http.StatusNoContent)
}

type scoreRequest struct {
	Score    int    `json:"score"`
	Comments string `json:"comments"`
}

func (h *Round2Handler) SetScore(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid participant id", http.StatusBadRequest)
		return
	}

	var req scoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE debate_participants dp
		SET score = $1, comments = $2, submitted_at = now()
		FROM slots s
		WHERE dp.id = $3 AND dp.slot_id = s.id AND s.created_by = $4 AND dp.attendance = 'present'
	`, req.Score, req.Comments, id, uid)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "participant not found, not your slot, or not marked present", http.StatusForbidden)
		return
	}

	if h.Sheets != nil {
		go h.syncParticipantToSheet(context.Background(), id)
	}

	w.WriteHeader(http.StatusNoContent)
}

var _ = appmiddleware.UserIDKey // keep import used if judgeID moves later

func (h *Round2Handler) syncParticipantToSheet(ctx context.Context, participantID int64) {
	if h.Sheets == nil {
		return
	}

	var name, email, locationName, team, attendance string
	var startTime time.Time
	var score *int
	var comments *string

	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name, ''), u.campus_email, l.name, s.start_time, dp.team, dp.attendance, dp.score, dp.comments
		FROM debate_participants dp
		JOIN users u ON u.id = dp.candidate_id
		JOIN slots s ON s.id = dp.slot_id
		JOIN locations l ON l.id = s.location_id
		WHERE dp.id = $1
	`, participantID).Scan(&name, &email, &locationName, &startTime, &team, &attendance, &score, &comments)
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

	row := []interface{}{
		name, email, locationName, startTime.Format(time.RFC3339), team,
		attendance, scoreStr, commentsStr, time.Now().Format(time.RFC3339),
	}
	h.Sheets.UpsertRowAtColumn(ctx, "Round2", "B", email, row)
}

type myClaimedSlot struct {
	ID           int64     `json:"id"`
	LocationName string    `json:"location_name"`
	StartTime    time.Time `json:"start_time"`
	FilledCount  int       `json:"filled_count"`
	Capacity     int       `json:"capacity"`
}

// MyClaimedSlots lists slots this judge has personally claimed for the
// round — lets them jump back into scoring without re-claiming.
func (h *Round2Handler) MyClaimedSlots(w http.ResponseWriter, r *http.Request) {
	uid, ok := judgeID(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id required", http.StatusBadRequest)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT s.id, l.name, s.start_time, s.filled_count, s.capacity
		FROM slots s JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1 AND s.created_by = $2
		ORDER BY s.start_time DESC
	`, roundID, uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	results := []myClaimedSlot{}
	for rows.Next() {
		var s myClaimedSlot
		if err := rows.Scan(&s.ID, &s.LocationName, &s.StartTime, &s.FilledCount, &s.Capacity); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, s)
	}
	json.NewEncoder(w).Encode(results)
}
