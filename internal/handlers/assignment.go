package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

type AssignmentHandler struct {
	Pool   *pgxpool.Pool
	Sheets *sheets.Client // nil-safe: sync is skipped if not configured yet
}

func NewAssignmentHandler(pool *pgxpool.Pool, sheetsClient *sheets.Client) *AssignmentHandler {
	return &AssignmentHandler{Pool: pool, Sheets: sheetsClient}
}

type assignRequest struct {
	GroupSize int `json:"group_size"`
}

type assignResult struct {
	PoolSize     int      `json:"pool_size"`
	GroupsFormed int      `json:"groups_formed"`
	SlotsFilled  int      `json:"slots_filled"`
	Unplaced     int      `json:"unplaced"`
	Warnings     []string `json:"warnings,omitempty"`
}

type openSlot struct {
	ID        int64
	Remaining int
	StartTime time.Time
}

func (h *AssignmentHandler) fetchOpenSlots(ctx context.Context, roundID int64, roundNumber int16) ([]openSlot, int, error) {
	query := `
		SELECT id, capacity - filled_count AS remaining, start_time
		FROM slots
		WHERE round_id = $1 AND capacity > filled_count
	`
	switch roundNumber {
	case 2:
		query += ` AND (SELECT count(*) FROM slot_co_judges cj WHERE cj.slot_id = slots.id) >= 1`
	case 3:
		query += ` AND (SELECT count(*) FROM slot_co_judges cj WHERE cj.slot_id = slots.id) >= 2`
	}
	query += ` ORDER BY start_time`

	rows, err := h.Pool.Query(ctx, query, roundID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var slots []openSlot
	total := 0
	for rows.Next() {
		var s openSlot
		if err := rows.Scan(&s.ID, &s.Remaining, &s.StartTime); err != nil {
			return nil, 0, err
		}
		slots = append(slots, s)
		total += s.Remaining
	}
	return slots, total, rows.Err()
}

func (h *AssignmentHandler) fetchUnavailability(ctx context.Context, roundID int64) (map[int64]map[string]bool, error) {
	rows, err := h.Pool.Query(ctx, `
		SELECT candidate_id, unavailable_dates::text[] FROM candidate_unavailability WHERE round_id = $1
	`, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]map[string]bool)
	for rows.Next() {
		var candidateID int64
		var dates []string
		if err := rows.Scan(&candidateID, &dates); err != nil {
			return nil, err
		}
		set := make(map[string]bool, len(dates))
		for _, d := range dates {
			set[d] = true
		}
		result[candidateID] = set
	}
	return result, rows.Err()
}

// RunAssignment shuffles the eligible candidate pool for a round, assigns
// group labels in chunks of GroupSize, and greedily fills open slots in
// start_time order. For round 2, it also splits each slot's occupants into
// Team A / Team B afterward.
func (h *AssignmentHandler) RunAssignment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	roundID, err := strconv.ParseInt(chi.URLParam(r, "roundID"), 10, 64)
	if err != nil {
		http.Error(w, "invalid round id", http.StatusBadRequest)
		return
	}

	var req assignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.GroupSize < 1 {
		http.Error(w, "group_size must be at least 1", http.StatusBadRequest)
		return
	}

	var roundNumber int16
	if err := h.Pool.QueryRow(ctx, `SELECT number FROM rounds WHERE id = $1`, roundID).Scan(&roundNumber); err != nil {
		http.Error(w, "round not found", http.StatusNotFound)
		return
	}

	reconciledCount, reconWarnings, err := h.reconcileUnavailability(ctx, roundID)
	if err != nil {
		http.Error(w, "failed to reconcile unavailability: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pool, err := h.fetchEligiblePool(ctx, roundID, roundNumber)
	if err != nil {
		http.Error(w, "failed to fetch candidate pool: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(pool) == 0 {
		warnings := append([]string{"No new candidates to place — everyone eligible is already assigned."}, reconWarnings...)
		json.NewEncoder(w).Encode(assignResult{
			PoolSize: 0, GroupsFormed: 0, SlotsFilled: 0, Unplaced: 0, Warnings: warnings,
		})
		return
	}

	slots, totalCapacity, err := h.fetchOpenSlots(ctx, roundID, roundNumber)
	if err != nil {
		http.Error(w, "failed to fetch open slots: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if totalCapacity < len(pool) {
		http.Error(w, fmt.Sprintf(
			"insufficient slot capacity: %d candidates but only %d capacity available (need %d more)",
			len(pool), totalCapacity, len(pool)-totalCapacity,
		), http.StatusConflict)
		return
	}

	unavail, err := h.fetchUnavailability(ctx, roundID)
	if err != nil {
		http.Error(w, "failed to fetch unavailability: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	slotFillMap := make(map[int64][]int64)
	unplacedDueToConflict := []int64{}
	groupCounter := 0
	totalPlaced := 0

	// queue of remaining candidates to place, processed slot by slot
	queue := append([]int64{}, pool...)

	for _, slot := range slots {
		slotDate := slot.StartTime.Format("2006-01-02")
		placedThisSlot := 0
		var stillQueued []int64 // candidates skipped for this slot due to unavailability, retried against later slots

		for _, candidateID := range queue {
			if placedThisSlot >= slot.Remaining {
				stillQueued = append(stillQueued, candidateID)
				continue
			}
			if unavail[candidateID][slotDate] {
				stillQueued = append(stillQueued, candidateID) // try this candidate again at a later slot/date
				continue
			}

			label := fmt.Sprintf("R%d-G%d", roundNumber, (groupCounter/req.GroupSize)+1)
			if _, err := tx.Exec(ctx, `
				INSERT INTO assignments (candidate_id, slot_id, status) VALUES ($1, $2, 'confirmed')
			`, candidateID, slot.ID); err != nil {
				http.Error(w, "failed to insert assignment: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET group_label = $1 WHERE id = $2`, label, candidateID); err != nil {
				http.Error(w, "failed to set group label: "+err.Error(), http.StatusInternalServerError)
				return
			}

			slotFillMap[slot.ID] = append(slotFillMap[slot.ID], candidateID)
			placedThisSlot++
			groupCounter++
			totalPlaced++
		}

		if placedThisSlot > 0 {
			if _, err := tx.Exec(ctx, `UPDATE slots SET filled_count = filled_count + $1 WHERE id = $2`, placedThisSlot, slot.ID); err != nil {
				http.Error(w, "failed to update slot fill count: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		queue = stillQueued
	}

	// anyone still in the queue after every slot has been tried couldn't be
	// placed anywhere without violating their stated unavailability
	unplacedDueToConflict = queue

	if roundNumber == 2 {
		for slotID, candidateIDs := range slotFillMap {
			shuffled := make([]int64, len(candidateIDs))
			copy(shuffled, candidateIDs)
			rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			half := len(shuffled) / 2
			for i, candidateID := range shuffled {
				team := "A"
				if i >= half {
					team = "B"
				}
				if _, err := tx.Exec(ctx, `UPDATE assignments SET team = $1 WHERE candidate_id = $2 AND slot_id = $3`, team, candidateID, slotID); err != nil {
					http.Error(w, "failed to assign team: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if _, err := tx.Exec(ctx, `INSERT INTO debate_participants (candidate_id, slot_id, team) VALUES ($1, $2, $3)`, candidateID, slotID, team); err != nil {
					http.Error(w, "failed to create debate participant row: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
	}

	if roundNumber == 1 || roundNumber == 3 {
		for slotID, candidateIDs := range slotFillMap {
			for _, candidateID := range candidateIDs {
				if _, err := tx.Exec(ctx, `INSERT INTO evaluations (candidate_id, slot_id, status) VALUES ($1, $2, 'not_arrived')`, candidateID, slotID); err != nil {
					http.Error(w, "failed to create evaluation row: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "failed to commit assignment: "+err.Error(), http.StatusInternalServerError)
		return
	}

	result := assignResult{
		PoolSize:     len(pool),
		GroupsFormed: (groupCounter + req.GroupSize - 1) / req.GroupSize,
		SlotsFilled:  len(slotFillMap),
		Unplaced:     len(unplacedDueToConflict),
	}
	if reconciledCount > 0 || len(reconWarnings) > 0 {
		if reconciledCount > 0 {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%d existing placement(s) were moved to resolve date conflicts.",
				reconciledCount,
			))
		}
		result.Warnings = append(result.Warnings, reconWarnings...)
	}
	if len(unplacedDueToConflict) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%d candidate(s) could not be placed anywhere without violating their stated unavailability - needs manual placement.",
			len(unplacedDueToConflict),
		))
	}

	if h.Sheets != nil {
		go h.syncAssignmentsToSheets(context.Background(), roundNumber, slotFillMap)
	}

	json.NewEncoder(w).Encode(result)
}

func (h *AssignmentHandler) fetchEligiblePool(ctx context.Context, roundID int64, roundNumber int16) ([]int64, error) {
	var query string
	switch roundNumber {
	case 1:
		query = `
			SELECT id FROM users
			WHERE role = 'candidate' AND is_active = true
			AND id NOT IN (
				SELECT a.candidate_id FROM assignments a
				JOIN slots s ON s.id = a.slot_id
				WHERE s.round_id = $1
			)`
	case 2:
		query = `
			SELECT id FROM users
			WHERE role = 'candidate' AND is_active = true AND round1_result = 'advanced'
			AND id NOT IN (
				SELECT a.candidate_id FROM assignments a
				JOIN slots s ON s.id = a.slot_id
				WHERE s.round_id = $1
			)`
	case 3:
		query = `
			SELECT id FROM users
			WHERE role = 'candidate' AND is_active = true AND round2_result = 'advanced'
			AND id NOT IN (
				SELECT a.candidate_id FROM assignments a
				JOIN slots s ON s.id = a.slot_id
				WHERE s.round_id = $1
			)`
	default:
		return nil, fmt.Errorf("unknown round number %d", roundNumber)
	}

	rows, err := h.Pool.Query(ctx, query, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (h *AssignmentHandler) syncAssignmentsToSheets(ctx context.Context, roundNumber int16, slotFillMap map[int64][]int64) {
	tab := fmt.Sprintf("Round%d", roundNumber)

	for slotID, candidateIDs := range slotFillMap {
		var locationName string
		var startTime time.Time
		h.Pool.QueryRow(ctx, `
			SELECT l.name, s.start_time FROM slots s JOIN locations l ON l.id = s.location_id WHERE s.id = $1
		`, slotID).Scan(&locationName, &startTime)

		for _, candidateID := range candidateIDs {
			var name, email string
			h.Pool.QueryRow(ctx, `SELECT COALESCE(name, ''), campus_email FROM users WHERE id = $1`, candidateID).Scan(&name, &email)

			row := []interface{}{
				name, email, locationName, startTime.Format(time.RFC3339),
				"not_arrived", "", "", time.Now().Format(time.RFC3339),
			}
			if err := h.Sheets.UpsertRowAtColumn(ctx, tab, "B", email, row); err != nil {
				fmt.Printf("sheets upsert failed for %s: %v\n", email, err)
			}
		}
	}
}

func (h *AssignmentHandler) reconcileUnavailability(ctx context.Context, roundID int64) (int, []string, error) {
	unavail, err := h.fetchUnavailability(ctx, roundID)
	if err != nil {
		return 0, nil, err
	}

	rows, err := h.Pool.Query(ctx, `
		SELECT a.id, a.candidate_id, a.slot_id, s.start_time
		FROM assignments a JOIN slots s ON s.id = a.slot_id
		WHERE s.round_id = $1
	`, roundID)
	if err != nil {
		return 0, nil, err
	}
	type placement struct {
		AssignmentID, CandidateID, SlotID int64
		SlotDate                          string
	}
	var placements []placement
	for rows.Next() {
		var p placement
		var st time.Time
		if err := rows.Scan(&p.AssignmentID, &p.CandidateID, &p.SlotID, &st); err != nil {
			rows.Close()
			return 0, nil, err
		}
		p.SlotDate = st.Format("2006-01-02")
		placements = append(placements, p)
	}
	rows.Close()

	var conflicted []placement
	for _, p := range placements {
		if unavail[p.CandidateID][p.SlotDate] {
			conflicted = append(conflicted, p)
		}
	}
	if len(conflicted) == 0 {
		return 0, nil, nil
	}

	openRows, err := h.Pool.Query(ctx, `
		SELECT id, start_time, capacity - filled_count FROM slots
		WHERE round_id = $1 AND is_buffer = false AND capacity > filled_count
		ORDER BY start_time
	`, roundID)
	if err != nil {
		return 0, nil, err
	}
	type openS struct {
		ID        int64
		Date      string
		Remaining int
	}
	var openSlots []openS
	for openRows.Next() {
		var s openS
		var st time.Time
		if err := openRows.Scan(&s.ID, &st, &s.Remaining); err != nil {
			openRows.Close()
			return 0, nil, err
		}
		s.Date = st.Format("2006-01-02")
		openSlots = append(openSlots, s)
	}
	openRows.Close()

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)

	moved := 0
	var warnings []string

	for _, p := range conflicted {
		placedNew := false
		for i := range openSlots {
			s := &openSlots[i]
			if s.Remaining <= 0 || s.ID == p.SlotID || unavail[p.CandidateID][s.Date] {
				continue
			}
			if err := reassignAssignment(ctx, tx, p.AssignmentID, s.ID); err != nil {
				continue
			}
			s.Remaining--
			moved++
			placedNew = true
			break
		}
		if !placedNew {
			warnings = append(warnings, fmt.Sprintf(
				"candidate %d is on a now-unavailable date and no open slot could take them — needs manual attention",
				p.CandidateID,
			))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return moved, warnings, nil
}
