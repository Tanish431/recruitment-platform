package models

import "time"

type QueryStatus string

const (
	QueryPending  QueryStatus = "pending"
	QueryResolved QueryStatus = "resolved"
)

type ResolutionType string

const (
	ResolutionSwap     ResolutionType = "swap"
	ResolutionReassign ResolutionType = "reassign"
)

type CandidateQuery struct {
	ID                      int64           `json:"id"`
	AssignmentID            int64           `json:"assignment_id"`
	Reason                  string          `json:"reason"`
	Status                  QueryStatus     `json:"status"`
	ResolutionType          *ResolutionType `json:"resolution_type,omitempty"`
	SwappedWithAssignmentID *int64          `json:"swapped_with_assignment_id,omitempty"`
	ResolvedBy              *int64          `json:"resolved_by,omitempty"`
	CreatedAt               time.Time       `json:"created_at"`
	ResolvedAt              *time.Time      `json:"resolved_at,omitempty"`
}
