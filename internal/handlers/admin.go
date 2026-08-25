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

	"github.com/Tanish431/recruitment-platform/internal/auth"
	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

type AdminHandler struct {
	Pool          *pgxpool.Pool
	AllowedDomain string
	Sheets        *sheets.Client
}

type assignStationRequest struct {
	JudgeID    int64 `json:"judge_id"`
	LocationID int64 `json:"location_id"`
	RoundID    int64 `json:"round_id"`
}

func NewAdminHandler(pool *pgxpool.Pool, allowedDomain string, sheets *sheets.Client) *AdminHandler {
	return &AdminHandler{Pool: pool, AllowedDomain: allowedDomain, Sheets: sheets}
}

type createSlotRequest struct {
	RoundID     int64  `json:"round_id"`
	LocationID  int64  `json:"location_id"`
	StartTime   string `json:"start_time"` // RFC3339
	DurationMin int    `json:"duration_min"`
	Capacity    int    `json:"capacity"`
}

func (h *AdminHandler) CreateSlot(w http.ResponseWriter, r *http.Request) {
	uid, ok := r.Context().Value(appmiddleware.UserIDKey).(int64)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req createSlotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Capacity < 1 || req.DurationMin < 1 {
		http.Error(w, "capacity and duration_min must be positive", http.StatusBadRequest)
		return
	}

	startTime, err := time.Parse(time.RFC3339, req.StartTime)
	if err != nil {
		http.Error(w, "start_time must be RFC3339", http.StatusBadRequest)
		return
	}

	row := h.Pool.QueryRow(r.Context(), `
		INSERT INTO slots (round_id, location_id, start_time, duration_min, capacity, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, req.RoundID, req.LocationID, startTime, req.DurationMin, req.Capacity, uid)

	var id int64
	if err := row.Scan(&id); err != nil {
		http.Error(w, "failed to create slot (possibly a duplicate location+time): "+err.Error(), http.StatusConflict)
		return
	}

	json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

func (h *AdminHandler) ListSlots(w http.ResponseWriter, r *http.Request) {
	roundIDStr := r.URL.Query().Get("round_id")
	roundID, err := strconv.ParseInt(roundIDStr, 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT s.id, s.location_id, l.name, s.start_time, s.duration_min,
		       s.capacity, s.filled_count, s.created_by, COALESCE(u.name, u.campus_email, '')
		FROM slots s
		LEFT JOIN users u ON u.id = s.created_by
		JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1
		ORDER BY s.start_time
	`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type slotView struct {
		ID           int64     `json:"id"`
		LocationID   int64     `json:"location_id"`
		LocationName string    `json:"location_name"`
		StartTime    time.Time `json:"start_time"`
		DurationMin  int       `json:"duration_min"`
		Capacity     int       `json:"capacity"`
		FilledCount  int       `json:"filled_count"`
		HostedByID   *int64    `json:"hosted_by_id"`
		HostedByName string    `json:"hosted_by_name"`
	}

	slots := []slotView{}
	for rows.Next() {
		var s slotView
		if err := rows.Scan(&s.ID, &s.LocationID, &s.LocationName, &s.StartTime,
			&s.DurationMin, &s.Capacity, &s.FilledCount, &s.HostedByID, &s.HostedByName); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		slots = append(slots, s)
	}

	json.NewEncoder(w).Encode(slots)
}

func (h *AdminHandler) DeleteSlot(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "slotID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `DELETE FROM slots WHERE id = $1 AND filled_count = 0`, id)
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "slot not found or already has candidates assigned", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) AssignJudgeStation(w http.ResponseWriter, r *http.Request) {
	var req assignStationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	var role string
	err := h.Pool.QueryRow(r.Context(), `SELECT role FROM users WHERE id = $1`, req.JudgeID).Scan(&role)
	if err != nil {
		http.Error(w, "judge user not found", http.StatusNotFound)
		return
	}
	if role != "judge" && role != "admin" {
		http.Error(w, "user is not a judge", http.StatusBadRequest)
		return
	}

	_, err = h.Pool.Exec(r.Context(), `
		INSERT INTO judge_stations (judge_id, location_id, round_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (judge_id, round_id) DO UPDATE SET location_id = EXCLUDED.location_id
	`, req.JudgeID, req.LocationID, req.RoundID)
	if err != nil {
		http.Error(w, "failed to assign station: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) ListJudgeStations(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT js.judge_id, u.campus_email, js.location_id, l.name
		FROM judge_stations js
		JOIN users u ON u.id = js.judge_id
		JOIN locations l ON l.id = js.location_id
		WHERE js.round_id = $1
		ORDER BY l.name
	`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type stationView struct {
		JudgeID      int64  `json:"judge_id"`
		JudgeEmail   string `json:"judge_email"`
		LocationID   int64  `json:"location_id"`
		LocationName string `json:"location_name"`
	}

	stations := []stationView{}
	for rows.Next() {
		var s stationView
		if err := rows.Scan(&s.JudgeID, &s.JudgeEmail, &s.LocationID, &s.LocationName); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		stations = append(stations, s)
	}

	json.NewEncoder(w).Encode(stations)
}

func (h *AdminHandler) PromoteToJudge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE users SET role = 'judge' WHERE campus_email = $1
	`, req.Email)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "user not found - they need to log in at least once first", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type moveAssignmentRequest struct {
	NewSlotID int64 `json:"new_slot_id"`
}

func (h *AdminHandler) MoveAssignment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "assignmentID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid assignment id", http.StatusBadRequest)
		return
	}
	var req moveAssignmentRequest
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

	if err := reassignAssignment(ctx, tx, id, req.NewSlotID); err != nil {
		http.Error(w, "move failed: "+err.Error(), http.StatusConflict)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}

	if h.Sheets != nil {
		go h.syncAssignmentToSheet(context.Background(), id)
	}
	w.WriteHeader(http.StatusNoContent)
}

type swapAssignmentsRequest struct {
	AssignmentAID int64 `json:"assignment_a_id"`
	AssignmentBID int64 `json:"assignment_b_id"`
}

func (h *AdminHandler) SwapAssignments(w http.ResponseWriter, r *http.Request) {
	var req swapAssignmentsRequest
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

	if err := swapAssignments(ctx, tx, req.AssignmentAID, req.AssignmentBID); err != nil {
		http.Error(w, "swap failed: "+err.Error(), http.StatusConflict)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}

	if h.Sheets != nil {
		go h.syncAssignmentToSheet(context.Background(), req.AssignmentAID)
		go h.syncAssignmentToSheet(context.Background(), req.AssignmentBID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) ListAssignments(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT a.id, a.candidate_id, COALESCE(u.name,''), COALESCE(u.bits_id,''), u.campus_email, a.slot_id, s.start_time,
		       l.name, a.status, a.team
		FROM assignments a
		JOIN users u ON u.id = a.candidate_id
		JOIN slots s ON s.id = a.slot_id
		JOIN locations l ON l.id = s.location_id
		WHERE s.round_id = $1 AND u.is_active = true
		ORDER BY s.start_time
	`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type assignmentBoardView struct {
		AssignmentID    int64     `json:"assignment_id"`
		CandidateID     int64     `json:"candidate_id"`
		CandidateName   string    `json:"candidate_name"`
		CandidateBitsID string    `json:"candidate_bits_id"`
		CandidateEmail  string    `json:"candidate_email"`
		SlotID          int64     `json:"slot_id"`
		SlotStart       time.Time `json:"slot_start"`
		LocationName    string    `json:"location_name"`
		Status          string    `json:"status"`
		Team            *string   `json:"team,omitempty"`
	}

	results := []assignmentBoardView{}
	for rows.Next() {
		var v assignmentBoardView
		if err := rows.Scan(&v.AssignmentID, &v.CandidateID, &v.CandidateName, &v.CandidateBitsID, &v.CandidateEmail, &v.SlotID,
			&v.SlotStart, &v.LocationName, &v.Status, &v.Team); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}

	json.NewEncoder(w).Encode(results)
}

func (h *AdminHandler) syncAssignmentToSheet(ctx context.Context, assignmentID int64) {
	var name, email, locationName, status string
	var startTime time.Time
	var roundNumber int16

	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name, ''), u.campus_email, l.name, s.start_time, ro.number, a.status
		FROM assignments a
		JOIN users u ON u.id = a.candidate_id
		JOIN slots s ON s.id = a.slot_id
		JOIN locations l ON l.id = s.location_id
		JOIN rounds ro ON ro.id = s.round_id
		WHERE a.id = $1
	`, assignmentID).Scan(&name, &email, &locationName, &startTime, &roundNumber, &status)
	if err != nil {
		return
	}

	tab := fmt.Sprintf("Round%d", roundNumber)
	row := []interface{}{name, email, locationName, sheets.FormatSheetTime(startTime), status, "", "", sheets.FormatSheetTime(time.Now())}
	h.Sheets.UpsertRowAtColumn(ctx, tab, "B", email, row)
}

func (h *AdminHandler) ImportFromSheet(w http.ResponseWriter, r *http.Request) {
	if h.Sheets == nil {
		http.Error(w, "sheets client not configured", http.StatusServiceUnavailable)
		return
	}

	rows, err := h.Sheets.ReadRows(r.Context(), "Sheet1")
	if err != nil {
		http.Error(w, "failed to read Sheet1: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(rows) < 2 {
		http.Error(w, "no data rows found in Sheet1", http.StatusBadRequest)
		return
	}

	header := rows[0]
	nameCol, emailCol, phoneCol, whatsappCol := -1, -1, -1, -1
	for i, c := range header {
		h := normalizeSheetHeader(fmt.Sprint(c))
		switch {
		case h == "email" || h == "campusemail" || h == "emailaddress":
			emailCol = i
		case h == "phone" || h == "phonenumber" || h == "mobilenumber" || h == "mobile":
			phoneCol = i
		case h == "whatsapp" || h == "whatsappnumber" || h == "whatsappno" || h == "whatsappmobilenumber":
			whatsappCol = i
		case strings.Contains(h, "name"):
			nameCol = i
		}
	}
	if emailCol == -1 {
		http.Error(w, "no email column found in Sheet1 header row", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	inserted, updated, skipped, deactivated := 0, 0, 0, 0
	var errs []string

	activeEmails := make([]string, 0, len(rows)-1)
	for i, row := range rows[1:] {
		if emailCol >= len(row) {
			continue
		}
		email := strings.TrimSpace(fmt.Sprint(row[emailCol]))
		if email == "" {
			continue
		}
		if err := auth.ValidateDomain(email, h.AllowedDomain); err != nil {
			errs = append(errs, fmt.Sprintf("row %d: %s - %s", i+2, email, err))
			skipped++
			continue
		}
		activeEmails = append(activeEmails, email)

		name, phone, whatsapp := "", "", ""
		if nameCol != -1 && nameCol < len(row) {
			name = strings.TrimSpace(fmt.Sprint(row[nameCol]))
		}
		if phoneCol != -1 && phoneCol < len(row) {
			phone = fmt.Sprint(row[phoneCol])
		}
		if whatsappCol != -1 && whatsappCol < len(row) {
			whatsapp = fmt.Sprint(row[whatsappCol])
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO users (campus_email, name, phone, whatsapp, role)
			VALUES ($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), 'candidate')
			ON CONFLICT (campus_email) DO NOTHING
		`, email, name, phone, whatsapp)
		if err != nil {
			errs = append(errs, fmt.Sprintf("row %d: db error - %s", i+2, err.Error()))
			continue
		}
		if tag.RowsAffected() > 0 {
			inserted++
			continue
		}

		if _, err := tx.Exec(ctx, `
			UPDATE users
			SET name = COALESCE(NULLIF($2,''), name),
			    phone = COALESCE(NULLIF($3,''), phone),
			    whatsapp = COALESCE(NULLIF($4,''), whatsapp),
			    is_active = true
			WHERE campus_email = $1
		`, email, name, phone, whatsapp); err != nil {
			errs = append(errs, fmt.Sprintf("row %d: db update error - %s", i+2, err.Error()))
			continue
		}
		updated++
	}

	deactivated, err = h.deactivateMissingCandidates(ctx, tx, activeEmails)
	if err != nil {
		http.Error(w, "failed to deactivate removed candidates: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit import", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"inserted":          inserted,
		"updated":           updated,
		"skipped":           skipped,
		"deactivated":       deactivated,
		"name_column_found": nameCol != -1,
		"errors":            errs,
	})
}

func (h *AdminHandler) deactivateMissingCandidates(ctx context.Context, tx pgx.Tx, activeEmails []string) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id
		FROM users
		WHERE role = 'candidate' AND is_active = true AND NOT (campus_email = ANY($1::text[]))
	`, activeEmails)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	candidateIDs := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		candidateIDs = append(candidateIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(candidateIDs) == 0 {
		return 0, nil
	}

	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = ANY($1::bigint[])`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE queries
		SET swapped_with_assignment_id = NULL
		WHERE swapped_with_assignment_id IN (
			SELECT id FROM assignments WHERE candidate_id = ANY($1::bigint[])
		)
	`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE slots s
		SET filled_count = GREATEST(0, s.filled_count - x.cnt)
		FROM (
			SELECT slot_id, COUNT(*)::int AS cnt
			FROM assignments
			WHERE candidate_id = ANY($1::bigint[])
			GROUP BY slot_id
		) x
		WHERE s.id = x.slot_id
	`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM evaluations WHERE candidate_id = ANY($1::bigint[])`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM debate_participants WHERE candidate_id = ANY($1::bigint[])`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM candidate_unavailability WHERE candidate_id = ANY($1::bigint[])`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM assignments WHERE candidate_id = ANY($1::bigint[])`, candidateIDs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET is_active = false WHERE id = ANY($1::bigint[])`, candidateIDs); err != nil {
		return 0, err
	}

	return len(candidateIDs), nil
}

func normalizeSheetHeader(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer(" ", "", "_", "", "-", "", "(", "", ")", "", ":", "", ".", "", "/", "")
	return replacer.Replace(s)
}

type dayConfig struct {
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	BreakStart string `json:"break_start,omitempty"`
	BreakEnd   string `json:"break_end,omitempty"`
}

type generateScheduleRequest struct {
	RoundID     int64     `json:"round_id"`
	LocationID  int64     `json:"location_id"`
	StartDate   string    `json:"start_date"` // "2026-08-15"
	EndDate     string    `json:"end_date"`   // inclusive
	Weekday     dayConfig `json:"weekday"`
	Weekend     dayConfig `json:"weekend"`
	DurationMin int       `json:"duration_min"`
	Capacity    int       `json:"capacity"` // per-slot capacity, e.g. 1 for R1, 6 for R2
	Timezone    string    `json:"timezone,omitempty"`
}

type generateScheduleResult struct {
	RequiredCapacity int      `json:"required_capacity"`
	Created          int      `json:"slots_created"`
	Days             []string `json:"days"`
}

func (h *AdminHandler) GenerateSchedule(w http.ResponseWriter, r *http.Request) {
	var req generateScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.DurationMin < 1 || req.Capacity < 1 {
		http.Error(w, "duration_min and capacity must be positive", http.StatusBadRequest)
		return
	}

	tz := req.Timezone
	if tz == "" {
		tz = "Asia/Kolkata"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		http.Error(w, "invalid timezone", http.StatusBadRequest)
		return
	}

	dateLayout := "2006-01-02"
	startDate, err := time.ParseInLocation(dateLayout, req.StartDate, loc)
	if err != nil {
		http.Error(w, "invalid start_date", http.StatusBadRequest)
		return
	}
	endDate, err := time.ParseInLocation(dateLayout, req.EndDate, loc)
	if err != nil {
		http.Error(w, "invalid end_date", http.StatusBadRequest)
		return
	}
	if endDate.Before(startDate) {
		http.Error(w, "end_date must be on or after start_date", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	var roundNumber int16
	if err := h.Pool.QueryRow(ctx, `SELECT number FROM rounds WHERE id = $1`, req.RoundID).Scan(&roundNumber); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}

	requiredCapacity, err := h.countEligiblePool(ctx, req.RoundID, roundNumber)
	if err != nil {
		http.Error(w, "failed to compute eligible pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if requiredCapacity == 0 {
		http.Error(w, "no eligible candidates found for this round - cannot size the schedule", http.StatusBadRequest)
		return
	}

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	result := generateScheduleResult{RequiredCapacity: requiredCapacity}
	timeLayout := "2006-01-02 15:04"

	for d := startDate; !d.After(endDate); d = d.AddDate(0, 0, 1) {
		isWeekend := d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
		cfg := req.Weekday
		if isWeekend {
			cfg = req.Weekend
		}
		if cfg.StartTime == "" || cfg.EndTime == "" {
			continue // no hours configured for this day type, skip entirely
		}

		dateStr := d.Format(dateLayout)

		start, err := time.ParseInLocation(timeLayout, dateStr+" "+cfg.StartTime, loc)
		if err != nil {
			http.Error(w, "invalid start_time on "+dateStr, http.StatusBadRequest)
			return
		}
		end, err := time.ParseInLocation(timeLayout, dateStr+" "+cfg.EndTime, loc)
		if err != nil {
			http.Error(w, "invalid end_time on "+dateStr, http.StatusBadRequest)
			return
		}
		if !end.After(start) {
			end = end.Add(24 * time.Hour)
		}

		var breakStart, breakEnd time.Time
		hasBreak := cfg.BreakStart != "" && cfg.BreakEnd != ""
		if hasBreak {
			breakStart, _ = time.ParseInLocation(timeLayout, dateStr+" "+cfg.BreakStart, loc)
			breakEnd, _ = time.ParseInLocation(timeLayout, dateStr+" "+cfg.BreakEnd, loc)
		}

		step := time.Duration(req.DurationMin) * time.Minute
		dayHadSlots := false

		for t := start; t.Add(step).Compare(end) <= 0; t = t.Add(step) {
			slotEnd := t.Add(step)
			if hasBreak && t.Before(breakEnd) && slotEnd.After(breakStart) {
				continue
			}

			_, err := tx.Exec(ctx, `
				INSERT INTO slots (round_id, location_id, start_time, duration_min, capacity)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (round_id, location_id, start_time) DO NOTHING
			`, req.RoundID, req.LocationID, t, req.DurationMin, req.Capacity)
			if err != nil {
				http.Error(w, "failed to create slot at "+t.Format(time.RFC3339)+": "+err.Error(), http.StatusInternalServerError)
				return
			}

			dayHadSlots = true
			result.Created++
		}

		if dayHadSlots {
			result.Days = append(result.Days, dateStr)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(result)
}

func (h *AdminHandler) countEligiblePool(ctx context.Context, roundID int64, roundNumber int16) (int, error) {
	var query string
	switch roundNumber {
	case 1:
		query = `
			SELECT count(*) FROM users
			WHERE role = 'candidate' AND is_active = true
			AND id NOT IN (
				SELECT a.candidate_id FROM assignments a JOIN slots s ON s.id = a.slot_id WHERE s.round_id = $1
			)`
	case 2:
		query = `
			SELECT count(*) FROM users
			WHERE role = 'candidate' AND is_active = true AND round1_result = 'advanced'
			AND id NOT IN (
				SELECT a.candidate_id FROM assignments a JOIN slots s ON s.id = a.slot_id WHERE s.round_id = $1
			)`
	case 3:
		query = `
			SELECT count(*) FROM users
			WHERE role = 'candidate' AND is_active = true AND round2_result = 'advanced'
			AND id NOT IN (
				SELECT a.candidate_id FROM assignments a JOIN slots s ON s.id = a.slot_id WHERE s.round_id = $1
			)`
	default:
		return 0, fmt.Errorf("unknown round number %d", roundNumber)
	}

	var count int
	if err := h.Pool.QueryRow(ctx, query, roundID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (h *AdminHandler) ListUnavailability(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT COALESCE(u.name,''), COALESCE(u.bits_id,''), u.campus_email, cu.unavailable_dates::text[], COALESCE(cu.note, ''), COALESCE(cu.reason,''), cu.submitted_at
		FROM candidate_unavailability cu
		JOIN users u ON u.id = cu.candidate_id
		WHERE cu.round_id = $1
		ORDER BY u.campus_email
	`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type view struct {
		CandidateName   string    `json:"candidate_name"`
		CandidateBitsID string    `json:"candidate_bits_id"`
		CandidateEmail  string    `json:"candidate_email"`
		Dates           []string  `json:"unavailable_dates"`
		Note            string    `json:"note"`
		Reason          string    `json:"reason"`
		SubmittedAt     time.Time `json:"submitted_at"`
	}

	results := []view{}
	for rows.Next() {
		var v view
		if err := rows.Scan(&v.CandidateName, &v.CandidateBitsID, &v.CandidateEmail, &v.Dates, &v.Note, &v.SubmittedAt, &v.Reason); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}

	json.NewEncoder(w).Encode(results)
}

func (h *AdminHandler) ListRounds(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Pool.Query(r.Context(), `SELECT id, number, name, slot_creation_open, is_active FROM rounds ORDER BY number`)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type roundView struct {
		ID               int64  `json:"id"`
		Number           int16  `json:"number"`
		Name             string `json:"name"`
		SlotCreationOpen bool   `json:"slot_creation_open"`
		IsActive         bool   `json:"is_active"`
	}
	rounds := []roundView{}
	for rows.Next() {
		var rv roundView
		if err := rows.Scan(&rv.ID, &rv.Number, &rv.Name, &rv.SlotCreationOpen, &rv.IsActive); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		rounds = append(rounds, rv)
	}
	json.NewEncoder(w).Encode(rounds)
}

func (h *AdminHandler) ListLocations(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}
	rows, err := h.Pool.Query(r.Context(), `SELECT id, name FROM locations WHERE round_id = $1 ORDER BY name`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type locView struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	locs := []locView{}
	for rows.Next() {
		var l locView
		if err := rows.Scan(&l.ID, &l.Name); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		locs = append(locs, l)
	}
	json.NewEncoder(w).Encode(locs)
}

func (h *AdminHandler) CreateLocation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoundID int64  `json:"round_id"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	row := h.Pool.QueryRow(r.Context(), `
		INSERT INTO locations (round_id, name) VALUES ($1, $2) RETURNING id
	`, req.RoundID, req.Name)
	var id int64
	if err := row.Scan(&id); err != nil {
		http.Error(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

func (h *AdminHandler) UnassignCandidate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "assignmentID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid assignment id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var candidateID, slotID int64
	if err := tx.QueryRow(ctx, `SELECT candidate_id, slot_id FROM assignments WHERE id = $1`, id).Scan(&candidateID, &slotID); err != nil {
		http.Error(w, "assignment not found", http.StatusNotFound)
		return
	}

	if _, err := tx.Exec(ctx, `DELETE FROM assignments WHERE id = $1`, id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE slots SET filled_count = filled_count - 1 WHERE id = $1`, slotID); err != nil {
		http.Error(w, "slot update failed", http.StatusInternalServerError)
		return
	}
	// clean up round-specific rows so the queue/scoring views don't show a ghost entry
	tx.Exec(ctx, `DELETE FROM evaluations WHERE candidate_id = $1 AND slot_id = $2`, candidateID, slotID)
	tx.Exec(ctx, `DELETE FROM debate_participants WHERE candidate_id = $1 AND slot_id = $2`, candidateID, slotID)

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) AddCandidateToSlot(w http.ResponseWriter, r *http.Request) {
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	var req struct {
		CandidateID int64 `json:"candidate_id"`
	}
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

	var roundID int64
	var roundNumber int16
	var slotStart time.Time
	err = tx.QueryRow(ctx, `
		SELECT s.round_id, ro.number, s.start_time FROM slots s JOIN rounds ro ON ro.id = s.round_id WHERE s.id = $1
	`, slotID).Scan(&roundID, &roundNumber, &slotStart)
	if err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}

	// check unavailability, but only WARN - never block a manual add, since
	// admin might have a good reason to override (candidate's plans changed, etc.)
	var conflict bool
	err = tx.QueryRow(ctx, `
		SELECT $1 = ANY(unavailable_dates) FROM candidate_unavailability
		WHERE candidate_id = $2 AND round_id = $3
	`, slotStart.Format("2006-01-02"), req.CandidateID, roundID).Scan(&conflict)
	// no row found just means no unavailability submitted - not an error
	if err != nil && err != pgx.ErrNoRows {
		conflict = false
	}

	tag, err := tx.Exec(ctx, `UPDATE slots SET filled_count = filled_count + 1 WHERE id = $1 AND filled_count < capacity`, slotID)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "slot is full", http.StatusConflict)
		return
	}

	if _, err := tx.Exec(ctx, `INSERT INTO assignments (candidate_id, slot_id, status) VALUES ($1, $2, 'confirmed')`, req.CandidateID, slotID); err != nil {
		http.Error(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if roundNumber == 2 {
		var countA, countB int
		tx.QueryRow(ctx, `SELECT count(*) FROM debate_participants WHERE slot_id = $1 AND team = 'A'`, slotID).Scan(&countA)
		tx.QueryRow(ctx, `SELECT count(*) FROM debate_participants WHERE slot_id = $1 AND team = 'B'`, slotID).Scan(&countB)
		team := "A"
		if countA > countB {
			team = "B"
		}
		tx.Exec(ctx, `INSERT INTO debate_participants (candidate_id, slot_id, team) VALUES ($1, $2, $3)`, req.CandidateID, slotID, team)
	} else {
		tx.Exec(ctx, `INSERT INTO evaluations (candidate_id, slot_id, status) VALUES ($1, $2, 'not_arrived')`, req.CandidateID, slotID)
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"unavailability_conflict": conflict})
}

func (h *AdminHandler) ListUnassignedCandidates(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}
	var roundNumber int16
	if err := h.Pool.QueryRow(r.Context(), `SELECT number FROM rounds WHERE id = $1`, roundID).Scan(&roundNumber); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}

	var query string
	switch roundNumber {
	case 1:
		query = `SELECT id, COALESCE(name,''), campus_email FROM users WHERE role='candidate' AND is_active = true AND id NOT IN (SELECT a.candidate_id FROM assignments a JOIN slots s ON s.id=a.slot_id WHERE s.round_id=$1)`
	case 2:
		query = `SELECT id, COALESCE(name,''), campus_email FROM users WHERE role='candidate' AND is_active = true AND round1_result='advanced' AND id NOT IN (SELECT a.candidate_id FROM assignments a JOIN slots s ON s.id=a.slot_id WHERE s.round_id=$1)`
	case 3:
		query = `SELECT id, COALESCE(name,''), campus_email FROM users WHERE role='candidate' AND is_active = true AND round2_result='advanced' AND id NOT IN (SELECT a.candidate_id FROM assignments a JOIN slots s ON s.id=a.slot_id WHERE s.round_id=$1)`
	}

	rows, err := h.Pool.Query(r.Context(), query, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type c struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	results := []c{}
	for rows.Next() {
		var v c
		if err := rows.Scan(&v.ID, &v.Name, &v.Email); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}
	json.NewEncoder(w).Encode(results)
}

func (h *AdminHandler) ListCandidates(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize := 25
	offset := (page - 1) * pageSize

	roundFilter := r.URL.Query().Get("round") // "", "1", "2", "3"

	whereClause := "WHERE role = 'candidate'"
	switch roundFilter {
	case "2":
		whereClause += " AND round1_result = 'advanced'"
	case "3":
		whereClause += " AND round1_result = 'advanced' AND round2_result = 'advanced'"
	}

	var total int
	h.Pool.QueryRow(r.Context(), "SELECT count(*) FROM users "+whereClause).Scan(&total)

	rows, err := h.Pool.Query(r.Context(), `
		SELECT id, COALESCE(name,''), campus_email, COALESCE(bits_id, ''), COALESCE(phone,''), COALESCE(whatsapp,''),
		       COALESCE(round1_result::text,''), COALESCE(round2_result::text,'')
		FROM users `+whereClause+`
		ORDER BY campus_email
		LIMIT $1 OFFSET $2
	`, pageSize, offset)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type cv struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Email        string `json:"email"`
		BitsID       string `json:"bits_id"`
		Phone        string `json:"phone"`
		WhatsApp     string `json:"whatsapp"`
		Round1Result string `json:"round1_result"`
		Round2Result string `json:"round2_result"`
	}
	list := []cv{}
	for rows.Next() {
		var v cv
		if err := rows.Scan(&v.ID, &v.Name, &v.Email, &v.BitsID, &v.Phone, &v.WhatsApp, &v.Round1Result, &v.Round2Result); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		list = append(list, v)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"candidates": list, "total": total, "page": page, "page_size": pageSize,
	})
}

func (h *AdminHandler) ActivateRound(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `UPDATE rounds SET is_active = false`); err != nil {
		http.Error(w, "failed to deactivate rounds", http.StatusInternalServerError)
		return
	}
	tag, err := tx.Exec(ctx, `UPDATE rounds SET is_active = true WHERE id = $1`, roundID)
	if err != nil {
		http.Error(w, "failed to activate round", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) DeactivateAllRounds(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Pool.Exec(r.Context(), `UPDATE rounds SET is_active = false`); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) ActiveRound(w http.ResponseWriter, r *http.Request) {
	var rv struct {
		ID     int64  `json:"id"`
		Number int16  `json:"number"`
		Name   string `json:"name"`
	}
	err := h.Pool.QueryRow(r.Context(), `SELECT id, number, name FROM rounds WHERE is_active = true LIMIT 1`).
		Scan(&rv.ID, &rv.Number, &rv.Name)
	if err != nil {
		http.Error(w, "no active round", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(rv)
}

func (h *AdminHandler) SearchUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	role := r.URL.Query().Get("role")

	query := `SELECT id, COALESCE(name,''), campus_email, role, COALESCE(round1_result::text,''), COALESCE(round2_result::text,'') FROM users WHERE 1=1`
	args := []interface{}{}
	idx := 1
	if q != "" {
		query += fmt.Sprintf(" AND (campus_email ILIKE $%d OR COALESCE(name,'') ILIKE $%d OR COALESCE(bits_id,'') ILIKE $%d)", idx, idx, idx)
		args = append(args, "%"+q+"%")
		idx++
	}
	if role != "" {
		query += fmt.Sprintf(" AND role = $%d", idx)
		args = append(args, role)
		idx++
	}
	query += " ORDER BY campus_email LIMIT 20"

	rows, err := h.Pool.Query(r.Context(), query, args...)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type u struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Email        string `json:"email"`
		Role         string `json:"role"`
		Round1Result string `json:"round1_result"`
		Round2Result string `json:"round2_result"`
	}
	results := []u{}
	for rows.Next() {
		var v u
		if err := rows.Scan(&v.ID, &v.Name, &v.Email, &v.Role, &v.Round1Result, &v.Round2Result); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, v)
	}
	json.NewEncoder(w).Encode(results)
}

type updateSlotCapacityRequest struct {
	Capacity int `json:"capacity"`
}

func (h *AdminHandler) UpdateSlotCapacity(w http.ResponseWriter, r *http.Request) {
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	var req updateSlotCapacityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Capacity < 1 {
		http.Error(w, "capacity must be at least 1", http.StatusBadRequest)
		return
	}

	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE slots SET capacity = $1 WHERE id = $2 AND $1 >= filled_count
	`, req.Capacity, slotID)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "slot not found, or new capacity is below current filled count", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *AdminHandler) SyncRoundResultsFromSheet(w http.ResponseWriter, r *http.Request) {
	if h.Sheets == nil {
		http.Error(w, "sheets client not configured", http.StatusServiceUnavailable)
		return
	}
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}

	var roundNumber int16
	if err := h.Pool.QueryRow(r.Context(), `SELECT number FROM rounds WHERE id = $1`, roundID).Scan(&roundNumber); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}
	if roundNumber != 1 && roundNumber != 2 {
		http.Error(w, "results sync only applies to round 1 or round 2 (feeding into round 2/3 eligibility)", http.StatusBadRequest)
		return
	}

	tabName := fmt.Sprintf("Round%d", roundNumber)
	rows, err := h.Sheets.ReadRows(r.Context(), tabName)
	if err != nil {
		http.Error(w, "failed to read sheet: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(rows) < 2 {
		http.Error(w, "no data rows in "+tabName, http.StatusBadRequest)
		return
	}

	header := rows[0]
	emailCol, statusCol := -1, -1
	for i, c := range header {
		h := strings.ToLower(strings.TrimSpace(fmt.Sprint(c)))
		if h == "email" {
			emailCol = i
		}
		// round1's column is literally "Status"; round2's is "Attendance",
		// but for R2->R3 eligibility we treat "present" as advanced-eligible
		// and require the admin to additionally mark real advancement via
		// a "Status" column they add themselves if scoring alone isn't enough.
		if h == "status" {
			statusCol = i
		}
	}
	if emailCol == -1 {
		http.Error(w, "no 'Email' column found in "+tabName, http.StatusBadRequest)
		return
	}
	if statusCol == -1 {
		http.Error(w, "no 'Status' column found in "+tabName+" - add one with 'advanced'/'eliminated' values per row", http.StatusBadRequest)
		return
	}

	resultCol := "round1_result"
	if roundNumber == 2 {
		resultCol = "round2_result"
	}

	ctx := r.Context()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	updated, skipped := 0, 0
	for _, row := range rows[1:] {
		if emailCol >= len(row) || statusCol >= len(row) {
			continue
		}
		email := strings.TrimSpace(fmt.Sprint(row[emailCol]))
		status := strings.ToLower(strings.TrimSpace(fmt.Sprint(row[statusCol])))
		if email == "" || (status != "advanced" && status != "eliminated") {
			skipped++
			continue
		}

		query := fmt.Sprintf(`UPDATE users SET %s = $1 WHERE campus_email = $2`, resultCol)
		tag, err := tx.Exec(ctx, query, status, email)
		if err != nil {
			http.Error(w, "update failed for "+email+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() > 0 {
			updated++
		} else {
			skipped++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]int{"updated": updated, "skipped": skipped})
}

type updateSlotLocationRequest struct {
	LocationID int64 `json:"location_id"`
}

func (h *AdminHandler) UpdateSlotLocation(w http.ResponseWriter, r *http.Request) {
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	var req updateSlotLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `UPDATE slots SET location_id = $1 WHERE id = $2`, req.LocationID, slotID)
	if err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reassignJudgeRequest struct {
	OldJudgeID int64 `json:"old_judge_id"`
	NewJudgeID int64 `json:"new_judge_id"`
}

func (h *AdminHandler) ReassignSlotJudge(w http.ResponseWriter, r *http.Request) {
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	var req reassignJudgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var hostID int64
	if err := h.Pool.QueryRow(ctx, `SELECT created_by FROM slots WHERE id = $1`, slotID).Scan(&hostID); err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}

	if hostID == req.OldJudgeID {
		_, err = h.Pool.Exec(ctx, `UPDATE slots SET created_by = $1 WHERE id = $2`, req.NewJudgeID, slotID)
		if err == nil {
			_, err = h.Pool.Exec(ctx, `
				UPDATE slot_co_judges SET host_marked_present = false, co_judge_marked_host_present = false
				WHERE slot_id = $1
			`, slotID)
		}
	} else {
		_, err = h.Pool.Exec(ctx, `
			UPDATE slot_co_judges
			SET judge_id = $1, host_marked_present = false, co_judge_marked_host_present = false
			WHERE slot_id = $2 AND judge_id = $3
		`, req.NewJudgeID, slotID, req.OldJudgeID)
	}
	if err != nil {
		http.Error(w, "reassign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type slotJudgeInfo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (h *AdminHandler) SlotJudges(w http.ResponseWriter, r *http.Request) {
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}

	var host slotJudgeInfo
	if err := h.Pool.QueryRow(r.Context(), `
		SELECT s.created_by, COALESCE(u.name, u.campus_email, '')
		FROM slots s JOIN users u ON u.id = s.created_by WHERE s.id = $1
	`, slotID).Scan(&host.ID, &host.Name); err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}

	rows, err := h.Pool.Query(r.Context(), `
		SELECT cj.judge_id, COALESCE(u.name, u.campus_email, '')
		FROM slot_co_judges cj JOIN users u ON u.id = cj.judge_id
		WHERE cj.slot_id = $1
	`, slotID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	coJudges := []slotJudgeInfo{}
	for rows.Next() {
		var j slotJudgeInfo
		if err := rows.Scan(&j.ID, &j.Name); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		coJudges = append(coJudges, j)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"host": host, "co_judges": coJudges})
}

func (h *AdminHandler) MarkR3JudgePresent(w http.ResponseWriter, r *http.Request) {
	slotID, err := strconv.ParseInt(chi.URLParam(r, "slotID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid slot id", http.StatusBadRequest)
		return
	}
	judgeIDParam, err := strconv.ParseInt(chi.URLParam(r, "judgeID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid judge id", http.StatusBadRequest)
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE slot_co_judges SET host_marked_present = true WHERE slot_id = $1 AND judge_id = $2
	`, slotID, judgeIDParam)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, "judge not found on this slot", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
