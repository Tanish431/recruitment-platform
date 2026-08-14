package models

import "time"

type Round struct {
	ID               int64     `json:"id"`
	Number           int16     `json:"number"`
	Name             string    `json:"name"`
	SlotCreationOpen bool      `json:"slot_creation_open"`
	CreatedAt        time.Time `json:"created_at"`
}

type Location struct {
	ID      int64  `json:"id"`
	RoundID int64  `json:"round_id"`
	Name    string `json:"name"`
}
