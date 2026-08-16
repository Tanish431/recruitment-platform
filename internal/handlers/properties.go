package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PropertyHandler struct {
	Pool *pgxpool.Pool
}

func NewPropertyHandler(pool *pgxpool.Pool) *PropertyHandler {
	return &PropertyHandler{Pool: pool}
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
		http.Error(w, "query failed", http.StatusInternalServerError)
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
		http.Error(w, "query failed", http.StatusInternalServerError)
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
		http.Error(w, "query failed", http.StatusInternalServerError)
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
