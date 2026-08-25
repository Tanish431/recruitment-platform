package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ResultsHandler struct {
	Pool *pgxpool.Pool
}

func NewResultsHandler(pool *pgxpool.Pool) *ResultsHandler {
	return &ResultsHandler{Pool: pool}
}

type resultsTableRow struct {
	CandidateID int64            `json:"candidate_id"`
	Name        string           `json:"name"`
	BitsID      string           `json:"bits_id"`
	Phone       string           `json:"phone"`
	Overall     *int             `json:"overall,omitempty"`
	Properties  map[int64]string `json:"properties"`
}

type resultsTableView struct {
	Properties []propertyView    `json:"properties"`
	Rows       []resultsTableRow `json:"rows"`
}

// CandidateResultsTable builds the grid: one row per candidate placed in
// this round, one column per active property, plus their overall score.
// Works for both R1/R3 (evaluations) and R2 (debate_participants) shapes.
func (h *ResultsHandler) CandidateResultsTable(w http.ResponseWriter, r *http.Request) {
	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	var roundNumber int16
	if err := h.Pool.QueryRow(ctx, `SELECT number FROM rounds WHERE id = $1`, roundID).Scan(&roundNumber); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}

	propRows, err := h.Pool.Query(ctx, `
		SELECT id, name, position FROM round_scoring_properties
		WHERE round_id = $1 AND is_active = true ORDER BY position, id
	`, roundID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	properties := []propertyView{}
	for propRows.Next() {
		var p propertyView
		if err := propRows.Scan(&p.ID, &p.Name, &p.Position); err == nil {
			properties = append(properties, p)
		}
	}
	propRows.Close()

	rowsMap := map[int64]*resultsTableRow{}
	var order []int64

	if roundNumber == 2 {
		dpRows, err := h.Pool.Query(ctx, `
			SELECT dp.id, dp.candidate_id, COALESCE(u.name,''), COALESCE(u.bits_id,''), COALESCE(u.phone,''), dp.score
			FROM debate_participants dp
			JOIN users u ON u.id = dp.candidate_id
			JOIN slots s ON s.id = dp.slot_id
			WHERE s.round_id = $1
			ORDER BY u.campus_email
		`, roundID)
		if err != nil {
			http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		type dpRow struct{ ParticipantID, CandidateID int64 }
		var dpList []dpRow
		for dpRows.Next() {
			var candidateID int64
			var name, bitsID, phone string
			var score *int
			var pid int64
			if err := dpRows.Scan(&pid, &candidateID, &name, &bitsID, &phone, &score); err != nil {
				continue
			}
			rowsMap[candidateID] = &resultsTableRow{CandidateID: candidateID, Name: name, BitsID: bitsID, Phone: phone, Overall: score, Properties: map[int64]string{}}
			order = append(order, candidateID)
			dpList = append(dpList, dpRow{pid, candidateID})
		}
		dpRows.Close()

		for _, d := range dpList {
			ratingRows, err := h.Pool.Query(ctx, `SELECT property_id, rating::text FROM participant_property_ratings WHERE participant_id = $1`, d.ParticipantID)
			if err != nil {
				continue
			}
			for ratingRows.Next() {
				var pid int64
				var rating string
				ratingRows.Scan(&pid, &rating)
				if row, ok := rowsMap[d.CandidateID]; ok {
					row.Properties[pid] = rating
				}
			}
			ratingRows.Close()
		}
	} else {
		evRows, err := h.Pool.Query(ctx, `
			SELECT e.id, e.candidate_id, COALESCE(u.name,''), COALESCE(u.bits_id,''), COALESCE(u.phone,''), e.score
			FROM evaluations e
			JOIN users u ON u.id = e.candidate_id
			JOIN slots s ON s.id = e.slot_id
			WHERE s.round_id = $1
			ORDER BY u.campus_email
		`, roundID)
		if err != nil {
			http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		type evRow struct{ EvaluationID, CandidateID int64 }
		var evList []evRow
		for evRows.Next() {
			var candidateID int64
			var name, bitsID, phone string
			var score *int
			var eid int64
			if err := evRows.Scan(&eid, &candidateID, &name, &bitsID, &phone, &score); err != nil {
				continue
			}
			rowsMap[candidateID] = &resultsTableRow{CandidateID: candidateID, Name: name, BitsID: bitsID, Phone: phone, Overall: score, Properties: map[int64]string{}}
			order = append(order, candidateID)
			evList = append(evList, evRow{eid, candidateID})
		}
		evRows.Close()

		for _, e := range evList {
			ratingRows, err := h.Pool.Query(ctx, `SELECT property_id, rating::text FROM evaluation_property_ratings WHERE evaluation_id = $1`, e.EvaluationID)
			if err != nil {
				continue
			}
			for ratingRows.Next() {
				var pid int64
				var rating string
				ratingRows.Scan(&pid, &rating)
				if row, ok := rowsMap[e.CandidateID]; ok {
					row.Properties[pid] = rating
				}
			}
			ratingRows.Close()
		}
	}

	rows := make([]resultsTableRow, 0, len(order))
	for _, cid := range order {
		rows = append(rows, *rowsMap[cid])
	}

	json.NewEncoder(w).Encode(resultsTableView{Properties: properties, Rows: rows})
}

func (h *ResultsHandler) CandidateSummary(w http.ResponseWriter, r *http.Request) {
	candidateID, err := strconv.ParseInt(chi.URLParam(r, "candidateID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid candidate id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	var name, bitsID, phone string
	if err := h.Pool.QueryRow(ctx, `
		SELECT COALESCE(name,''), COALESCE(bits_id,''), COALESCE(phone,'') FROM users WHERE id = $1
	`, candidateID).Scan(&name, &bitsID, &phone); err != nil {
		http.Error(w, "candidate not found", http.StatusNotFound)
		return
	}

	type roundSummary struct {
		RoundNumber  int16             `json:"round_number"`
		Status       string            `json:"status"`
		Overall      *int              `json:"overall,omitempty"`
		SpeakerNotes string            `json:"speaker_notes,omitempty"`
		FinalNotes   string            `json:"final_notes,omitempty"`
		JudgeName    string            `json:"judge_name,omitempty"`
		CoJudgeName  string            `json:"co_judge_name,omitempty"`
		Motion       string            `json:"motion,omitempty"`
		Team         string            `json:"team,omitempty"`
		TeamAPrep    string            `json:"team_a_prep,omitempty"`
		TeamBPrep    string            `json:"team_b_prep,omitempty"`
		Properties   map[string]string `json:"properties"`
	}
	var rounds []roundSummary

	evRows, err := h.Pool.Query(ctx, `
		SELECT e.id, ro.number, e.status, e.score,
		       COALESCE(e.speaker_notes,''), COALESCE(e.final_notes,''), COALESCE(e.co_judge_name,''),
		       COALESCE(e.motion,''), COALESCE(ju.name, ju.campus_email, '')
		FROM evaluations e
		JOIN slots s ON s.id = e.slot_id
		JOIN rounds ro ON ro.id = s.round_id
		LEFT JOIN users ju ON ju.id = e.judge_id
		WHERE e.candidate_id = $1
	`, candidateID)
	if err == nil {
		for evRows.Next() {
			var rs roundSummary
			var eid int64
			if err := evRows.Scan(&eid, &rs.RoundNumber, &rs.Status, &rs.Overall, &rs.SpeakerNotes, &rs.FinalNotes, &rs.CoJudgeName, &rs.Motion, &rs.JudgeName); err != nil {
				continue
			}
			rs.Properties = map[string]string{}
			pRows, _ := h.Pool.Query(ctx, `
				SELECT rp.name, epr.rating::text FROM evaluation_property_ratings epr
				JOIN round_scoring_properties rp ON rp.id = epr.property_id
				WHERE epr.evaluation_id = $1
			`, eid)
			for pRows.Next() {
				var pname, rating string
				pRows.Scan(&pname, &rating)
				rs.Properties[pname] = rating
			}
			pRows.Close()
			rounds = append(rounds, rs)
		}
		evRows.Close()
	}

	dpRows, err := h.Pool.Query(ctx, `
		SELECT dp.id, dp.team, dp.attendance, dp.score,
		       COALESCE(dp.speaker_notes,''), COALESCE(dp.final_notes,''),
		       COALESCE(s.motion,''), COALESCE(s.team_a_prep,''), COALESCE(s.team_b_prep,''),
		       COALESCE(hu.name, hu.campus_email, ''), COALESCE(cj.name, cj.campus_email, '')
		FROM debate_participants dp
		JOIN slots s ON s.id = dp.slot_id
		JOIN users hu ON hu.id = s.created_by
		LEFT JOIN slot_co_judges scj ON scj.slot_id = s.id
		LEFT JOIN users cj ON cj.id = scj.judge_id
		WHERE dp.candidate_id = $1
	`, candidateID)
	if err == nil {
		for dpRows.Next() {
			var rs roundSummary
			var pid int64
			rs.RoundNumber = 2
			if err := dpRows.Scan(&pid, &rs.Team, &rs.Status, &rs.Overall, &rs.SpeakerNotes, &rs.FinalNotes, &rs.Motion, &rs.TeamAPrep, &rs.TeamBPrep, &rs.JudgeName, &rs.CoJudgeName); err != nil {
				continue
			}
			rs.Properties = map[string]string{}
			pRows, _ := h.Pool.Query(ctx, `
				SELECT rp.name, ppr.rating::text FROM participant_property_ratings ppr
				JOIN round_scoring_properties rp ON rp.id = ppr.property_id
				WHERE ppr.participant_id = $1
			`, pid)
			for pRows.Next() {
				var pname, rating string
				pRows.Scan(&pname, &rating)
				rs.Properties[pname] = rating
			}
			pRows.Close()
			rounds = append(rounds, rs)
		}
		dpRows.Close()
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"candidate_id": candidateID, "name": name, "bits_id": bitsID, "phone": phone, "rounds": rounds,
	})
}
