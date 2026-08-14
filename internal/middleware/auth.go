package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type contextKey string

const UserIDKey contextKey = "user_id"
const UserRoleKey contextKey = "user_role"

type AuthMiddleware struct {
	Pool *pgxpool.Pool
}

func NewAuthMiddleware(pool *pgxpool.Pool) *AuthMiddleware {
	return &AuthMiddleware{Pool: pool}
}

func (m *AuthMiddleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_id")
		if err != nil || cookie.Value == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var userID int64
		var role string
		var isActive bool
		var expiresAt time.Time

		row := m.Pool.QueryRow(r.Context(), `
			SELECT s.user_id, u.role, u.is_active, s.expires_at
			FROM sessions s
			JOIN users u ON u.id = s.user_id
			WHERE s.id = $1
		`, cookie.Value)

		if err := row.Scan(&userID, &role, &isActive, &expiresAt); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if role == "candidate" && !isActive {
			http.Error(w, "candidate is inactive", http.StatusUnauthorized)
			return
		}
		if time.Now().After(expiresAt) {
			http.Error(w, "session expired", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), UserIDKey, userID)
		ctx = context.WithValue(ctx, UserRoleKey, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole restricts access to any of the given allowed roles. Must be
// applied after RequireAuth, since it reads the role set into context there.
func RequireRole(next http.Handler, allowedRoles ...string) http.Handler {
	allowed := make(map[string]bool, len(allowedRoles))
	for _, r := range allowedRoles {
		allowed[r] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userRole, ok := r.Context().Value(UserRoleKey).(string)
		if !ok || !allowed[userRole] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
