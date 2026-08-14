package models

import "time"

type EvalStatus string

const (
	EvalCheckedIn  EvalStatus = "checked_in"
	EvalInProgress EvalStatus = "in_progress"
	EvalCompleted  EvalStatus = "completed"
	EvalNoShow     EvalStatus = "no_show"
	EvalSkipped    EvalStatus = "skipped"
)

// Round 1 evaluation
type Evaluation struct {
	ID              int64      `json:"id"`
	CandidateID     int64      `json:"candidate_id"`
	SlotID          int64      `json:"slot_id"`
	JudgeID         *int64     `json:"judge_id,omitempty"`
	Status          EvalStatus `json:"status"`
	CheckedInAt     *time.Time `json:"checked_in_at,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	Score           *int       `json:"score,omitempty"`
	Comments        *string    `json:"comments,omitempty"`
	DurationSeconds *int       `json:"duration_seconds,omitempty"`
	SkipCount       int        `json:"skip_count"`
}
