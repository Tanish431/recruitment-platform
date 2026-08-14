package models

import "time"

type Attendance string

const (
	AttendancePending Attendance = "pending"
	AttendancePresent Attendance = "present"
	AttendanceNoShow  Attendance = "no_show"
)

type Team string

const (
	TeamA Team = "A"
	TeamB Team = "B"
)

// Round 2 evaluation (attendance + score combined, single judge per slot)
type DebateParticipant struct {
	ID          int64      `json:"id"`
	CandidateID int64      `json:"candidate_id"`
	SlotID      int64      `json:"slot_id"`
	Team        Team       `json:"team"`
	Attendance  Attendance `json:"attendance"`
	Score       *int       `json:"score,omitempty"`
	Comments    *string    `json:"comments,omitempty"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
}
