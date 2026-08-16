package handlers

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
)

// blockIfRoundInactive writes a 403 and returns true if a non-admin caller
// is targeting a round that isn't the currently admin-activated one.
// Admins bypass this entirely - they need access to every round to set
// things up ahead of time.
func blockIfRoundInactive(ctx context.Context, pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, roundID int64) bool {
	role, _ := r.Context().Value(appmiddleware.UserRoleKey).(string)
	if role == "admin" {
		return false
	}
	var isActive bool
	err := pool.QueryRow(ctx, `SELECT is_active FROM rounds WHERE id = $1`, roundID).Scan(&isActive)
	if err != nil || !isActive {
		http.Error(w, "this round is not currently active", http.StatusForbidden)
		return true
	}
	return false
}
