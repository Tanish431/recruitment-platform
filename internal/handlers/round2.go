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
	ScorerJudgeID            int64   `json:"scorer_judge_id"`
	ScorerName               string  `json:"scorer_name"`
	ScorerChosen             bool    `json:"scorer_chosen"`
	YouAreScorer             bool    `json:"you_are_scorer"`
	TeamAPrep                string  `json:"team_a_prep"`
	TeamBPrep                string  `json:"team_b_prep"`
	Motion                   string  `json:"motion"`
}

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
	var scorerJudgeID *int64
	err = h.Pool.QueryRow(r.Context(), `
		SELECT s.id, s.created_by, COALESCE(hu.name, hu.campus_email, ''),
		       cj.judge_id, COALESCE(cj.host_marked_present, false), COALESCE(cj.co_judge_marked_host_present, false),
		       COALESCE(s.team_a_prep, ''), COALESCE(s.team_b_prep, ''), COALESCE(s.motion, ''), s.scorer_judge_id
		FROM slots s
		JOIN users hu ON hu.id = s.created_by
		LEFT JOIN slot_co_judges cj ON cj.slot_id = s.id
		WHERE s.id = $1
	`, slotID).Scan(&v.SlotID, &v.HostID, &v.HostName, &v.CoJudgeID,
		&v.HostMarkedCoJudgePresent, &v.CoJudgeMarkedHostPresent,
		&v.TeamAPrep, &v.TeamBPrep, &v.Motion, &scorerJudgeID)
	if err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}

	if v.CoJudgeID != nil {
		var name string
		h.Pool.QueryRow(r.Context(), `SELECT COALESCE(name, campus_email, '') FROM users WHERE id = $1`, *v.CoJudgeID).Scan(&name)
		v.CoJudgeName = &name
	}

	v.YouAreHost = v.HostID == uid
	v.YouAreCoJudge = v.CoJudgeID != nil && *v.CoJudgeID == uid
	v.ScoringUnlocked = v.CoJudgeID != nil && v.HostMarkedCoJudgePresent && v.CoJudgeMarkedHostPresent

	effectiveScorer := v.HostID
	if scorerJudgeID != nil {
		effectiveScorer = *scorerJudgeID
		v.ScorerChosen = true
	}
	v.ScorerJudgeID = effectiveScorer
	v.YouAreScorer = effectiveScorer == uid
	if effectiveScorer == v.HostID {
		v.ScorerName = v.HostName
	} else if v.CoJudgeName != nil {
		v.ScorerName = *v.CoJudgeName
	}

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
	Team string `json:"team"`
	Text string `json:"text"`
}

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

	ctx := r.Context()
	scorer, err := h.effectiveScorer(ctx, slotID)
	if err != nil || scorer != uid {
		http.Error(w, "you are not the designated scorer for this slot", http.StatusForbidden)
		return
	}

	col := "team_a_prep"
	if req.Team == "B" {
		col = "team_b_prep"
	}
	// safe: col is one of two hardcoded literals above, never derived from request input
	query := fmt.Sprintf(`UPDATE slots SET %s = $1 WHERE id = $2`, col)
	if _, err := h.Pool.Exec(ctx, query, req.Text, slotID); err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if h.Sheets != nil {
		go h.syncSlotToSheet(context.Background(), slotID)
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

	ctx := r.Context()
	scorer, err := h.effectiveScorer(ctx, slotID)
	if err != nil {
		http.Error(w, "slot not found", http.StatusNotFound)
		return
	}
	if scorer != uid {
		http.Error(w, "you are not the designated scorer for this slot", http.StatusForbidden)
		return
	}

	rows, err := h.Pool.Query(ctx, `
		SELECT dp.id, dp.candidate_id, COALESCE(u.name,''), COALESCE(u.bits_id,''), u.campus_email, dp.team, dp.attendance, dp.score, dp.comments
		FROM debate_participants dp
		JOIN users u ON u.id = dp.candidate_id
		WHERE dp.slot_id = $1
		ORDER BY dp.team, u.campus_email
	`, slotID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type participantView struct {
		ID              int64   `json:"id"`
		CandidateID     int64   `json:"candidate_id"`
		CandidateName   string  `json:"candidate_name"`
		CandidateBitsID string  `json:"candidate_bits_id"`
		CandidateEmail  string  `json:"candidate_email"`
		Team            string  `json:"team"`
		Attendance      string  `json:"attendance"`
		Score           *int    `json:"score,omitempty"`
		Comments        *string `json:"comments,omitempty"`
	}

	participants := []participantView{}
	for rows.Next() {
		var p participantView
		if err := rows.Scan(&p.ID, &p.CandidateID, &p.CandidateName, &p.CandidateBitsID, &p.CandidateEmail, &p.Team, &p.Attendance, &p.Score, &p.Comments); err != nil {
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

	ctx := r.Context()
	var slotID int64
	h.Pool.QueryRow(ctx, `SELECT slot_id FROM debate_participants WHERE id = $1`, id).Scan(&slotID)
	scorer, err := h.effectiveScorer(ctx, slotID)
	if err != nil || scorer != uid {
		http.Error(w, "you are not the designated scorer for this slot", http.StatusForbidden)
		return
	}

	tag, err := h.Pool.Exec(ctx, `UPDATE debate_participants SET attendance = $1 WHERE id = $2`, req.Attendance, id)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "participant not found", http.StatusNotFound)
		return
	}
	if h.Sheets != nil {
		go h.syncParticipantToSheet(context.Background(), id)
	}
	w.WriteHeader(http.StatusNoContent)
}

type scoreRequest struct {
	Score        int    `json:"score"`
	SpeakerNotes string `json:"speaker_notes"`
	FinalNotes   string `json:"final_notes"`
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
	if req.Score < 1 || req.Score > 5 {
		http.Error(w, "score must be between 1 and 5", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	var slotID int64
	h.Pool.QueryRow(ctx, `SELECT slot_id FROM debate_participants WHERE id = $1`, id).Scan(&slotID)
	scorer, err := h.effectiveScorer(ctx, slotID)
	if err != nil || scorer != uid {
		http.Error(w, "you are not the designated scorer for this slot", http.StatusForbidden)
		return
	}

	tag, err := h.Pool.Exec(ctx, `
		UPDATE debate_participants SET score = $1, speaker_notes = $2, final_notes = $3, submitted_at = now()
		WHERE id = $4 AND attendance = 'present'
	`, req.Score, req.SpeakerNotes, req.FinalNotes, id)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "participant not found or not marked present", http.StatusForbidden)
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

	var email, name, locationName, team, attendance, speakerNotes, finalNotes string
	var startTime time.Time
	var score *int
	var slotID int64
	var teamAPrep, teamBPrep string

	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name,''), u.campus_email, l.name, s.start_time, dp.team, dp.attendance, dp.score,
		       COALESCE(s.team_a_prep, ''), COALESCE(s.team_b_prep, '') ,COALESCE(dp.speaker_notes,''), COALESCE(dp.final_notes,''), dp.slot_id
		FROM debate_participants dp
		JOIN users u ON u.id = dp.candidate_id
		JOIN slots s ON s.id = dp.slot_id
		JOIN locations l ON l.id = s.location_id
		WHERE dp.id = $1
	`, participantID).Scan(&name, &email, &locationName, &startTime, &team, &attendance, &score, &teamAPrep, &teamBPrep,
		&speakerNotes, &finalNotes, &slotID)
	if err != nil {
		return
	}

	var hostName, coJudgeName, motion string
	h.Pool.QueryRow(ctx, `
		SELECT COALESCE(hu.name, hu.campus_email, ''), COALESCE(s.motion,'')
		FROM slots s JOIN users hu ON hu.id = s.created_by WHERE s.id = $1
	`, slotID).Scan(&hostName, &motion)
	h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name, u.campus_email, '') FROM slot_co_judges cj JOIN users u ON u.id = cj.judge_id WHERE cj.slot_id = $1
	`, slotID).Scan(&coJudgeName)

	scoreStr := ""
	if score != nil {
		scoreStr = fmt.Sprint(*score)
	}

	ratingsStr := h.formatPropertyRatings(ctx, "participant_property_ratings", "participant_id", participantID)

	visibleTeamAPrep := ""
	visibleTeamBPrep := ""
	if team == "A" {
		visibleTeamAPrep = teamAPrep
	} else if team == "B" {
		visibleTeamBPrep = teamBPrep
	}

	row := []interface{}{
		name, email, locationName, sheets.FormatSheetTime(startTime), team,
		visibleTeamAPrep, visibleTeamBPrep, motion, attendance, scoreStr, ratingsStr, hostName, coJudgeName, speakerNotes, finalNotes,
		sheets.FormatSheetTime(time.Now()),
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
		WHERE s.round_id = $1 AND s.closed_at IS NULL
		AND (s.created_by = $2 OR s.id IN (SELECT slot_id FROM slot_co_judges WHERE judge_id = $2))
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

// effectiveScorer returns who's allowed to mark attendance/score for a
// slot: whoever the host explicitly delegated to (scorer_judge_id), or the
// host themself by default if no delegation has happened yet.
func (h *Round2Handler) effectiveScorer(ctx context.Context, slotID int64) (int64, error) {
	var hostID int64
	var scorerID *int64
	err := h.Pool.QueryRow(ctx, `SELECT created_by, scorer_judge_id FROM slots WHERE id = $1`, slotID).Scan(&hostID, &scorerID)
	if err != nil {
		return 0, err
	}
	if scorerID != nil {
		return *scorerID, nil
	}
	return hostID, nil
}

type setScorerRequest struct {
	JudgeID int64 `json:"judge_id"`
}

// SetScorer lets the host decide, after attendance is confirmed, whether
// they or the co-judge actually enters scores.
func (h *Round2Handler) SetScorer(w http.ResponseWriter, r *http.Request) {
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
	var req setScorerRequest
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
	if hostID != uid {
		http.Error(w, "only the host can choose who scores", http.StatusForbidden)
		return
	}

	// req.JudgeID must be the host or one of the co-judges on this slot
	if req.JudgeID != hostID {
		var isCoJudge bool
		h.Pool.QueryRow(ctx, `SELECT true FROM slot_co_judges WHERE slot_id = $1 AND judge_id = $2`, slotID, req.JudgeID).Scan(&isCoJudge)
		if !isCoJudge {
			http.Error(w, "judge_id must be the host or a co-judge on this slot", http.StatusBadRequest)
			return
		}
	}

	if _, err := h.Pool.Exec(ctx, `UPDATE slots SET scorer_judge_id = $1 WHERE id = $2`, req.JudgeID, slotID); err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type motionRequest struct {
	Motion string `json:"motion"`
}

func (h *Round2Handler) SetMotion(w http.ResponseWriter, r *http.Request) {
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
	var req motionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	scorer, err := h.effectiveScorer(ctx, slotID)
	if err != nil || scorer != uid {
		http.Error(w, "you are not the designated scorer for this slot", http.StatusForbidden)
		return
	}
	if _, err := h.Pool.Exec(ctx, `UPDATE slots SET motion = $1 WHERE id = $2`, req.Motion, slotID); err != nil {
		http.Error(w, "update failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if h.Sheets != nil {
		go h.syncSlotToSheet(context.Background(), slotID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Round2Handler) syncSlotToSheet(ctx context.Context, slotID int64) {
	rows, err := h.Pool.Query(ctx, `SELECT id FROM debate_participants WHERE slot_id = $1`, slotID)
	if err != nil {
		return
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		h.syncParticipantToSheet(ctx, id)
	}
}
