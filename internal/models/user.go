package models

import "time"

type Role string

const (
	RoleCandidate Role = "candidate"
	RolePanelist  Role = "panelist"
	RoleJudge     Role = "judge"
	RoleAdmin     Role = "admin"
)

type User struct {
	ID               int64     `json:"id"`
	CampusEmail      string    `json:"campus_email"`
	Name             string    `json:"name"`
	BitsID           string    `json:"bits_id"`
	Phone            string    `json:"phone"`
	WhatsApp         string    `json:"whatsapp"`
	Role             Role      `json:"role"`
	IsActive         bool      `json:"is_active"`
	Round1Result     *string   `json:"round1_result,omitempty"` // "advanced" | "eliminated"
	Round2Result     *string   `json:"round2_result,omitempty"` // "advanced" | "eliminated"
	Round1ResultSeen bool      `json:"round1_result_seen"`
	Round2ResultSeen bool      `json:"round2_result_seen"`
	CreatedAt        time.Time `json:"created_at"`
}
