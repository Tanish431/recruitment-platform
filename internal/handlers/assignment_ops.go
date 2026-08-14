package handlers

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var errFullSlot = errors.New("target slot has no free capacity")

func swapAssignments(ctx context.Context, tx pgx.Tx, assignmentA, assignmentB int64) error {
	var slotA, slotB, candidateA, candidateB int64
	if err := tx.QueryRow(ctx, `SELECT slot_id, candidate_id FROM assignments WHERE id = $1`, assignmentA).Scan(&slotA, &candidateA); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT slot_id, candidate_id FROM assignments WHERE id = $1`, assignmentB).Scan(&slotB, &candidateB); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE assignments SET slot_id = $1, status = 'confirmed' WHERE id = $2`, slotB, assignmentA); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE assignments SET slot_id = $1, status = 'confirmed' WHERE id = $2`, slotA, assignmentB); err != nil {
		return err
	}
	if err := syncEvalRecordAfterSlotChange(ctx, tx, candidateA, slotA, slotB); err != nil {
		return err
	}
	return syncEvalRecordAfterSlotChange(ctx, tx, candidateB, slotB, slotA)
}

func reassignAssignment(ctx context.Context, tx pgx.Tx, assignmentID, newSlotID int64) error {
	var oldSlotID, candidateID int64
	if err := tx.QueryRow(ctx, `SELECT slot_id, candidate_id FROM assignments WHERE id = $1`, assignmentID).Scan(&oldSlotID, &candidateID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE slots SET filled_count = filled_count + 1 WHERE id = $1 AND filled_count < capacity`, newSlotID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errFullSlot
	}
	if _, err := tx.Exec(ctx, `UPDATE slots SET filled_count = filled_count - 1 WHERE id = $1`, oldSlotID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE assignments SET slot_id = $1, status = 'confirmed' WHERE id = $2`, newSlotID, assignmentID); err != nil {
		return err
	}
	return syncEvalRecordAfterSlotChange(ctx, tx, candidateID, oldSlotID, newSlotID)
}

func syncEvalRecordAfterSlotChange(ctx context.Context, tx pgx.Tx, candidateID, oldSlotID, newSlotID int64) error {
	if oldSlotID == newSlotID {
		return nil
	}
	var roundNumber int16
	if err := tx.QueryRow(ctx, `
		SELECT ro.number FROM slots s JOIN rounds ro ON ro.id = s.round_id WHERE s.id = $1
	`, newSlotID).Scan(&roundNumber); err != nil {
		return err
	}

	if roundNumber == 2 {
		_, err := tx.Exec(ctx, `
			UPDATE debate_participants SET slot_id = $1 WHERE candidate_id = $2 AND slot_id = $3
		`, newSlotID, candidateID, oldSlotID)
		return err
	}

	_, err := tx.Exec(ctx, `
		UPDATE evaluations SET slot_id = $1 WHERE candidate_id = $2 AND slot_id = $3
	`, newSlotID, candidateID, oldSlotID)
	return err
}
