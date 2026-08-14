package models

import "time"

type AssignmentStatus string

const (
	AssignmentConfirmed    AssignmentStatus = "confirmed"
	AssignmentPendingQuery AssignmentStatus = "pending_query"
	AssignmentReassigned   AssignmentStatus = "reassigned"
)

type Assignment struct {
	ID          int64            `json:"id"`
	CandidateID int64            `json:"candidate_id"`
	SlotID      int64            `json:"slot_id"`
	Status      AssignmentStatus `json:"status"`
	CreatedAt   time.Time        `json:"created_at"`
}
