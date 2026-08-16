package handlers

import (
	"context"
	"database/sql"
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

type Round2Handler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client
}

func NewRound2Handler(pool *pgxpool.Pool, sheetsClient *sheets.Client) *Round2Handler {
	return &Round2Handler{Pool: pool, Sheets: sheetsClient}
}

func maxCoJudges(roundNumber int16) int {
	if roundNumber == 3 {
		return 2
	}
	return 1
}

// AvailableSlots lists open (location, time) combos a judge can still claim
// for round 2 - only shown while the round's slot_creation_open flag is true.
func (h *Round2Handler) AvailableSlots(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(r.URL.Query().Get("round_id"), 10, 64)
	if err != nil {
		http.Error(w, "round_id query param required", http.StatusBadRequest)
		return
	}
	if blockIfRoundInactive(r.Context(), h.Pool, w, r, roundID) { // or roundID, matching whichever var name is local
		return
	}

	var exists bool
	if err := h.Pool.QueryRow(r.Context(), `SELECT true FROM rounds WHERE id = $1`, roundID).Scan(&exists); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}
	if !exists {
		http.Error(w, "round not found", http.StatusNotFound)
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
// enforcement - this handler just surfaces a clean 409 when it fires.
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
		req.DurationMin = 75
	}

	ctx := r.Context()
	var exists bool
	if err := h.Pool.QueryRow(ctx, `SELECT true FROM rounds WHERE id = $1`, req.RoundID).Scan(&exists); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}

	// Slot creation cap: only (eligible candidates / 12) slots can exist at
	// once — forces judges to double up on existing slots as co-judges
	// rather than everyone spinning up their own near-empty debate.
	var eligibleCount int
	h.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role = 'candidate' AND round1_result = 'advanced'`).Scan(&eligibleCount)
	maxSlots := eligibleCount / 12
	if maxSlots < 1 {
		maxSlots = 1
	}
	var currentSlots int
	h.Pool.QueryRow(ctx, `SELECT count(*) FROM slots WHERE round_id = $1 AND created_by IS NOT NULL`, req.RoundID).Scan(&currentSlots)
	if currentSlots >= maxSlots {
		http.Error(w, "slot creation limit reached — join an existing open slot as co-judge instead", http.StatusConflict)
		return
	}

	startTime, err := time.Parse(time.RFC3339, req.StartTime)
	if err != nil {
		http.Error(w, "start_time must be RFC3339", http.StatusBadRequest)
		return
	}

	row := h.Pool.QueryRow(ctx, `
		INSERT INTO slots (round_id, location_id, start_time, duration_min, capacity, created_by, claimed_at)
		VALUES ($1, $2, $3, $4, 6, $5, now())
		RETURNING id
	`, req.RoundID, req.LocationID, startTime, req.DurationMin, uid)

	var id int64
	if err := row.Scan(&id); err != nil {
		http.Error(w, "slot already claimed by another judge", http.StatusConflict)
		return
	}
	json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

// OpenSlotsToJoin lists slots that have a host but no co-judge yet — what
// a judge sees when deciding to join rather than create.
func (h *Round2Handler) OpenSlotsToJoin(w http.ResponseWriter, r *http.Request) {
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

	var roundNumber int16
	h.Pool.QueryRow(r.Context(), `SELECT number FROM rounds WHERE id = $1`, roundID).Scan(&roundNumber)
	cap := maxCoJudges(roundNumber)

	rows, err := h.Pool.Query(r.Context(), `
		SELECT s.id, l.name, s.start_time, COALESCE(hu.name, hu.campus_email, ''),
		       (SELECT count(*) FROM slot_co_judges cj WHERE cj.slot_id = s.id)
		FROM slots s
		JOIN locations l ON l.id = s.location_id
		JOIN users hu ON hu.id = s.created_by
		WHERE s.round_id = $1 AND s.created_by IS NOT NULL AND s.created_by != $2
		AND (SELECT count(*) FROM slot_co_judges cj WHERE cj.slot_id = s.id) < $3
		ORDER BY s.start_time
	`, roundID, uid, cap)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type openView struct {
		ID           int64     `json:"id"`
		Location     string    `json:"location_name"`
		Start        time.Time `json:"start_time"`
		HostName     string    `json:"host_name"`
		JudgesJoined int       `json:"judges_joined"`
		JudgesNeeded int       `json:"judges_needed"`
	}
	results := []openView{}
	for rows.Next() {
		var v openView
		if err := rows.Scan(&v.ID, &v.Location, &v.Start, &v.HostName, &v.JudgesJoined); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		v.JudgesNeeded = cap
		results = append(results, v)
	}
	json.NewEncoder(w).Encode(results)
}

// JoinSlot lets a judge become the co-judge on someone else's claimed slot.
func (h *Round2Handler) JoinSlot(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()
	var hostID int64
	var roundID int64
	if err := h.Pool.QueryRow(ctx, `SELECT created_by, round_id FROM slots WHERE id = $1`, slotID).Scan(&hostID, &roundID); err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}
	if hostID == uid {
		http.Error(w, "you already host this slot", http.StatusBadRequest)
		return
	}

	var roundNumber int16
	h.Pool.QueryRow(ctx, `SELECT number FROM rounds WHERE id = $1`, roundID).Scan(&roundNumber)
	cap := maxCoJudges(roundNumber)

	var coJudgeCount int
	h.Pool.QueryRow(ctx, `SELECT count(*) FROM slot_co_judges WHERE slot_id = $1`, slotID).Scan(&coJudgeCount)
	if coJudgeCount >= cap {
		http.Error(w, "this slot already has enough co-judges", http.StatusConflict)
		return
	}

	if _, err := h.Pool.Exec(ctx, `INSERT INTO slot_co_judges (slot_id, judge_id) VALUES ($1, $2)`, slotID, uid); err != nil {
		http.Error(w, "failed to join: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type coJudgeStatusView struct {
	SlotID                   int64   `json:"slot_id"`
	HostID                   int64   `json:"host_id"`
	HostName                 string  `json:"host_name"`
	CoJudgeID                *int64  `json:"co_judge_id,omitempty"`
	CoJudgeName              *string `json:"co_judge_name,omitempty"`
	HostMarkedCoJudgePresent bool    `json:"host_marked_co_judge_present"`
	CoJudgeMarkedHostPresent bool    `json:"co_judge_marked_host_present"`
	YouAreHost               bool    `json:"you_are_host"`
	YouAreCoJudge            bool    `json:"you_are_co_judge"`
	ScoringUnlocked          bool    `json:"scoring_unlocked"`
	TeamAPrep                string  `json:"team_a_prep"`
	TeamBPrep                string  `json:"team_b_prep"`
}

// SlotCoJudgeStatus is the combined view a judge's UI polls to know who's
// hosting/co-judging and whether scoring is unlocked yet.
func (h *Round2Handler) SlotCoJudgeStatus(w http.ResponseWriter, r *http.Request) {
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

	var v coJudgeStatusView
	err = h.Pool.QueryRow(r.Context(), `
		SELECT s.id, s.created_by, COALESCE(hu.name, hu.campus_email, ''),
		       cj.judge_id, cu.name, cu.campus_email,
		       COALESCE(cj.host_marked_present, false), COALESCE(cj.co_judge_marked_host_present, false),
		       COALESCE(s.team_a_prep, ''), COALESCE(s.team_b_prep, '')
		FROM slots s
		JOIN users hu ON hu.id = s.created_by
		LEFT JOIN slot_co_judges cj ON cj.slot_id = s.id
		LEFT JOIN users cu ON cu.id = cj.judge_id
		WHERE s.id = $1
	`, slotID).Scan(&v.SlotID, &v.HostID, &v.HostName, &v.CoJudgeID, new(sql.NullString), new(sql.NullString),
		&v.HostMarkedCoJudgePresent, &v.CoJudgeMarkedHostPresent, &v.TeamAPrep, &v.TeamBPrep)
	if err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}

	// re-query co-judge name cleanly (nullable name/email combo needs its own scan)
	if v.CoJudgeID != nil {
		var name string
		h.Pool.QueryRow(r.Context(), `SELECT COALESCE(name, campus_email, '') FROM users WHERE id = $1`, *v.CoJudgeID).Scan(&name)
		v.CoJudgeName = &name
	}

	v.YouAreHost = v.HostID == uid
	v.YouAreCoJudge = v.CoJudgeID != nil && *v.CoJudgeID == uid
	v.ScoringUnlocked = v.CoJudgeID != nil && v.HostMarkedCoJudgePresent && v.CoJudgeMarkedHostPresent

	json.NewEncoder(w).Encode(v)
}

// MarkCoJudgePresent: host confirms the co-judge showed up.
func (h *Round2Handler) MarkCoJudgePresent(w http.ResponseWriter, r *http.Request) {
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
	h.Pool.QueryRow(r.Context(), `SELECT created_by FROM slots WHERE id = $1`, slotID).Scan(&hostID)
	if hostID != uid {
		http.Error(w, "only the host can mark the co-judge present", http.StatusForbidden)
		return
	}
	tag, err := h.Pool.Exec(r.Context(), `UPDATE slot_co_judges SET host_marked_present = true WHERE slot_id = $1`, slotID)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, "no co-judge has joined this slot yet", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MarkHostPresent: the co-judge confirms the host showed up.
func (h *Round2Handler) MarkHostPresent(w http.ResponseWriter, r *http.Request) {
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
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE slot_co_judges SET co_judge_marked_host_present = true WHERE slot_id = $1 AND judge_id = $2
	`, slotID, uid)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, "you are not the co-judge for this slot", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type prepRequest struct {
	Team string `json:"team"` // "A" | "B"
	Text string `json:"text"`
}

// SetTeamPrep: host-only, records pre-debate prep notes per team.
func (h *Round2Handler) SetTeamPrep(w http.ResponseWriter, r *http.Request) {
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
	var req prepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Team != "A" && req.Team != "B") {
		http.Error(w, "team must be A or B", http.StatusBadRequest)
		return
	}
	col := "team_a_prep"
	if req.Team == "B" {
		col = "team_b_prep"
	}
	// safe: col is one of two hardcoded literals above, never derived from request input
	query := fmt.Sprintf(`UPDATE slots SET %s = $1 WHERE id = $2 AND created_by = $3`, col)
	tag, err := h.Pool.Exec(r.Context(), query, req.Text, slotID, uid)
	if err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "not your slot", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Round2Handler) coJudgeConfirmed(ctx context.Context, slotID int64) bool {
	var total, confirmed int
	h.Pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE host_marked_present AND co_judge_marked_host_present)
		FROM slot_co_judges WHERE slot_id = $1
	`, slotID).Scan(&total, &confirmed)
	return total >= 1 && total == confirmed
}

// Participants lists the 6 candidates in a slot the judge is hosting,
// with current attendance/score state - the judge's scoring screen.
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
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
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

	var slotID int64
	h.Pool.QueryRow(r.Context(), `SELECT slot_id FROM debate_participants WHERE id = $1`, id).Scan(&slotID)
	if !h.coJudgeConfirmed(r.Context(), slotID) {
		http.Error(w, "confirm co-judge attendance before entering scores", http.StatusForbidden)
		return
	}

	var req scoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.Score < 1 || req.Score > 5 {
		http.Error(w, "score must be between 1 and 5", http.StatusBadRequest)
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

	ratingsStr := h.formatPropertyRatings(ctx, "participant_property_ratings", "evaluation_id", participantID)

	row := []interface{}{
		name, email, locationName, startTime.Format(time.RFC3339), team,
		attendance, scoreStr, ratingsStr, commentsStr, time.Now().Format(time.RFC3339),
	}
	h.Sheets.UpsertRowAtColumn(ctx, "Round2", "B", email, row)
}

func (h *Round2Handler) formatPropertyRatings(ctx context.Context, table, fkCol string, id int64) string {
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

type myClaimedSlot struct {
	ID           int64     `json:"id"`
	LocationName string    `json:"location_name"`
	StartTime    time.Time `json:"start_time"`
	FilledCount  int       `json:"filled_count"`
	Capacity     int       `json:"capacity"`
}

// MyClaimedSlots lists slots this judge has personally claimed for the
// round - lets them jump back into scoring without re-claiming.
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
		WHERE s.round_id = $1 AND s.created_by = $2 AND s.closed_at IS NULL
		ORDER BY s.start_time DESC
	`, roundID, uid)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
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

func (h *Round2Handler) CloseSlot(w http.ResponseWriter, r *http.Request) {
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
	tag, err := h.Pool.Exec(r.Context(), `
		UPDATE slots SET closed_at = now() WHERE id = $1 AND created_by = $2
	`, slotID, uid)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "slot not found or not yours", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
