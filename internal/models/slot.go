package models

import "time"

type Slot struct {
	ID          int64      `json:"id"`
	RoundID     int64      `json:"round_id"`
	LocationID  int64      `json:"location_id"`
	StartTime   time.Time  `json:"start_time"`
	DurationMin int        `json:"duration_min"`
	Capacity    int        `json:"capacity"`
	FilledCount int        `json:"filled_count"`
	CreatedBy   *int64     `json:"created_by,omitempty"` // panelist who claimed it (round 2)
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
}
