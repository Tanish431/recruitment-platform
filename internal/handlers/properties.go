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

	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

type PropertyHandler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client
}

func NewPropertyHandler(pool *pgxpool.Pool, sheetsClient *sheets.Client) *PropertyHandler {
	return &PropertyHandler{Pool: pool, Sheets: sheetsClient}
}

type propertyView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Position int16  `json:"position"`
}

// ListProperties is available to any authenticated role - judges need it
// to render the scoring form, admins need it to manage the list.
func (h *PropertyHandler) ListProperties(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}
	rows, err := h.Pool.Query(r.Context(), `
		SELECT id, name, position FROM round_scoring_properties
		WHERE round_id = $1 AND is_active = true
		ORDER BY position, id
	`, roundID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	results := []propertyView{}
	for rows.Next() {
		var p propertyView
		if err := rows.Scan(&p.ID, &p.Name, &p.Position); err != nil {
			http.Error(w, "scan failed", http.StatusInternalServerError)
			return
		}
		results = append(results, p)
	}
	json.NewEncoder(w).Encode(results)
}

func (h *PropertyHandler) CreateProperty(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	var maxPos int16
	h.Pool.QueryRow(r.Context(), `SELECT COALESCE(MAX(position), -1) FROM round_scoring_properties WHERE round_id = $1`, roundID).Scan(&maxPos)

	var id int64
	err = h.Pool.QueryRow(r.Context(), `
		INSERT INTO round_scoring_properties (round_id, name, position) VALUES ($1, $2, $3) RETURNING id
	`, roundID, req.Name, maxPos+1).Scan(&id)
	if err != nil {
		http.Error(w, "insert failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]int64{"id": id})
}

func (h *PropertyHandler) DeleteProperty(w http.ResponseWriter, r *http.Request) {
	propertyID, err := strconv.ParseInt(chi.URLParam(r, "propertyID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid property id", http.StatusBadRequest)
		return
	}
	_, err = h.Pool.Exec(r.Context(), `UPDATE round_scoring_properties SET is_active = false WHERE id = $1`, propertyID)
	if err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Ratings ---

type rateRequest struct {
	Rating string `json:"rating"` // "bad" | "meh" | "good"
}

func validRating(r string) bool {
	return r == "bad" || r == "meh" || r == "good"
}

func (h *PropertyHandler) RateEvaluationProperty(w http.ResponseWriter, r *http.Request) {
	evaluationID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}
	propertyID, err := strconv.ParseInt(chi.URLParam(r, "propertyID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid property id", http.StatusBadRequest)
		return
	}
	var req rateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validRating(req.Rating) {
		http.Error(w, "rating must be bad, meh, or good", http.StatusBadRequest)
		return
	}
	_, err = h.Pool.Exec(r.Context(), `
		INSERT INTO evaluation_property_ratings (evaluation_id, property_id, rating)
		VALUES ($1, $2, $3)
		ON CONFLICT (evaluation_id, property_id) DO UPDATE SET rating = EXCLUDED.rating, rated_at = now()
	`, evaluationID, propertyID, req.Rating)
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Sheets != nil {
		go h.syncEvaluationToSheet(context.Background(), evaluationID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *PropertyHandler) EvaluationRatings(w http.ResponseWriter, r *http.Request) {
	evaluationID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid evaluation id", http.StatusBadRequest)
		return
	}
	rows, err := h.Pool.Query(r.Context(), `
		SELECT property_id, rating::text FROM evaluation_property_ratings WHERE evaluation_id = $1
	`, evaluationID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	result := map[int64]string{}
	for rows.Next() {
		var pid int64
		var rating string
		rows.Scan(&pid, &rating)
		result[pid] = rating
	}
	json.NewEncoder(w).Encode(result)
}

func (h *PropertyHandler) RateParticipantProperty(w http.ResponseWriter, r *http.Request) {
	participantID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid participant id", http.StatusBadRequest)
		return
	}
	propertyID, err := strconv.ParseInt(chi.URLParam(r, "propertyID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid property id", http.StatusBadRequest)
		return
	}
	var req rateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validRating(req.Rating) {
		http.Error(w, "rating must be bad, meh, or good", http.StatusBadRequest)
		return
	}
	_, err = h.Pool.Exec(r.Context(), `
		INSERT INTO participant_property_ratings (participant_id, property_id, rating)
		VALUES ($1, $2, $3)
		ON CONFLICT (participant_id, property_id) DO UPDATE SET rating = EXCLUDED.rating, rated_at = now()
	`, participantID, propertyID, req.Rating)
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Sheets != nil {
		go h.syncParticipantToSheet(context.Background(), participantID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *PropertyHandler) ParticipantRatings(w http.ResponseWriter, r *http.Request) {
	participantID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid participant id", http.StatusBadRequest)
		return
	}
	rows, err := h.Pool.Query(r.Context(), `
		SELECT property_id, rating::text FROM participant_property_ratings WHERE participant_id = $1
	`, participantID)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	result := map[int64]string{}
	for rows.Next() {
		var pid int64
		var rating string
		rows.Scan(&pid, &rating)
		result[pid] = rating
	}
	json.NewEncoder(w).Encode(result)
}

func (h *PropertyHandler) formatPropertyRatings(ctx context.Context, table, fkCol string, id int64) string {
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

func (h *PropertyHandler) syncEvaluationToSheet(ctx context.Context, evaluationID int64) {
	if h.Sheets == nil {
		return
	}

	var email, name, locationName, status, judgeName, coJudgeName, speakerNotes, finalNotes string
	var startTime time.Time
	var score *int
	var roundNumber int16

	err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(u.name,''), u.campus_email, l.name, s.start_time, ro.number, e.status, e.score,
		       COALESCE(e.speaker_notes,''), COALESCE(e.final_notes,''), COALESCE(e.co_judge_name,''),
		       COALESCE(ju.name, ju.campus_email, '')
		FROM evaluations e
		JOIN users u ON u.id = e.candidate_id
		JOIN slots s ON s.id = e.slot_id
		JOIN locations l ON l.id = s.location_id
		JOIN rounds ro ON ro.id = s.round_id
		LEFT JOIN users ju ON ju.id = e.judge_id
		WHERE e.id = $1
	`, evaluationID).Scan(&name, &email, &locationName, &startTime, &roundNumber, &status, &score,
		&speakerNotes, &finalNotes, &coJudgeName, &judgeName)
	if err != nil {
		return
	}

	scoreStr := ""
	if score != nil {
		scoreStr = fmt.Sprint(*score)
	}
	ratingsStr := h.formatPropertyRatings(ctx, "evaluation_property_ratings", "evaluation_id", evaluationID)

	tab := fmt.Sprintf("Round%d", roundNumber)
	row := []interface{}{
		name, email, locationName, sheets.FormatSheetTime(startTime),
		status, scoreStr, h.getMotion(ctx, evaluationID), judgeName, coJudgeName, ratingsStr, speakerNotes, finalNotes,
		sheets.FormatSheetTime(time.Now()),
	}
	h.Sheets.UpsertRowAtColumn(ctx, tab, "B", email, row)
}

func (h *PropertyHandler) syncParticipantToSheet(ctx context.Context, participantID int64) {
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
		       COALESCE(s.team_a_prep, ''), COALESCE(s.team_b_prep, ''), COALESCE(dp.speaker_notes,''), COALESCE(dp.final_notes,''), dp.slot_id
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

func (h *PropertyHandler) getMotion(ctx context.Context, evaluationID int64) string {
	var motion string
	_ = h.Pool.QueryRow(ctx, `SELECT COALESCE(motion,'') FROM evaluations WHERE id = $1`, evaluationID).Scan(&motion)
	return motion
}
