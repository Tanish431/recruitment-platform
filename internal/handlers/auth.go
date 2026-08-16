package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/Tanish431/recruitment-platform/internal/auth"
	appmiddleware "github.com/Tanish431/recruitment-platform/internal/middleware"
	"github.com/Tanish431/recruitment-platform/internal/models"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

const (
	sessionCookieName = "session_id"
	stateCookieName   = "oauth_state"
	sessionTTL        = 7 * 24 * time.Hour
)

type AuthHandler struct {
	Pool          *pgxpool.Pool
	OAuthCfg      *oauth2.Config
	AllowedDomain string
	FrontendURL   string
	Sheets        *sheets.Client
}

func NewAuthHandler(pool *pgxpool.Pool, oauthCfg *oauth2.Config, allowedDomain, frontendURL string, sheets *sheets.Client) *AuthHandler {
	return &AuthHandler{
		Pool:          pool,
		OAuthCfg:      oauthCfg,
		AllowedDomain: allowedDomain,
		FrontendURL:   frontendURL,
		Sheets:        sheets,
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Login redirects the user to Google's consent screen.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	state, err := randomToken()
	if err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300, // 5 min, enough time to complete the consent flow
	})

	// hd hints Google to pre-filter to the campus domain in the picker -
	// this is a UX nicety only, NOT a security check. The real check
	// happens server-side in Callback via auth.ValidateDomain.
	url := h.OAuthCfg.AuthCodeURL(state, oauth2.SetAuthURLParam("hd", h.AllowedDomain), oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// Callback handles Google's redirect back after consent.
func (h *AuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") != stateCookie.Value {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	token, err := h.OAuthCfg.Exchange(ctx, code)
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusUnauthorized)
		return
	}

	info, err := auth.FetchUserInfo(ctx, h.OAuthCfg, token)
	if err != nil {
		http.Error(w, "failed to fetch user info", http.StatusInternalServerError)
		return
	}
	if !info.EmailVerified {
		http.Error(w, "email not verified", http.StatusForbidden)
		return
	}
	if err := auth.ValidateDomain(info.Email, h.AllowedDomain); err != nil {
		http.Error(w, "unauthorized domain: "+err.Error(), http.StatusForbidden)
		return
	}

	user, err := h.upsertUser(ctx, info.Email, info.Name)
	if err != nil {
		http.Error(w, "failed to create/find user", http.StatusInternalServerError)
		return
	}
	if user.Role == "candidate" && !user.IsActive {
		http.Error(w, "candidate is inactive", http.StatusForbidden)
		return
	}

	sessionID, err := h.createSession(ctx, user.ID)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})

	// clear the now-consumed state cookie
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/", MaxAge: -1})

	dest := h.FrontendURL + "/dashboard"
	if user.Phone == "" || user.WhatsApp == "" {
		dest = h.FrontendURL + "/onboarding"
	} else if user.Role == "judge" || user.Role == "admin" {
		dest = h.FrontendURL + "/judge"
	}
	http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
}

func (h *AuthHandler) upsertUser(ctx context.Context, email, name string) (*models.User, error) {
	row := h.Pool.QueryRow(ctx, `
		INSERT INTO users (campus_email, name, role)
		VALUES ($1, $2, 'candidate')
		ON CONFLICT (campus_email) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, campus_email, COALESCE(name,''), COALESCE(phone, ''), COALESCE(whatsapp, ''), role, is_active, created_at
	`, email, name)

	var u models.User
	if err := row.Scan(&u.ID, &u.CampusEmail, &u.Name, &u.Phone, &u.WhatsApp, &u.Role, &u.IsActive, &u.CreatedAt); err != nil {
		return nil, err
	}
	if h.Sheets != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Sheets.UpsertRowAtColumn(ctx, "Sheet1", "B", u.CampusEmail,
			[]interface{}{u.Name, u.CampusEmail, u.Phone, u.WhatsApp}); err != nil {
			log.Printf("warning: failed to sync candidate sheet row on login for %s: %v", u.CampusEmail, err)
		}
	}
	return &u, nil
}

func (h *AuthHandler) createSession(ctx context.Context, userID int64) (string, error) {
	sessionID, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = h.Pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, expires_at)
		VALUES ($1, $2, $3)
	`, sessionID, userID, time.Now().Add(sessionTTL))
	if err != nil {
		return "", err
	}
	return sessionID, nil
}

// Me returns the currently authenticated user.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(appmiddleware.UserIDKey).(int64)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	row := h.Pool.QueryRow(r.Context(), `
		SELECT id, campus_email, COALESCE(name,''), COALESCE(phone,''), COALESCE(whatsapp,''), role, round1_result::text, round2_result::text, round1_result_seen, round2_result_seen, is_active, created_at
		FROM users WHERE id = $1
	`, userID)
	var u models.User
	if err := row.Scan(&u.ID, &u.CampusEmail, &u.Name, &u.Phone, &u.WhatsApp, &u.Role, &u.Round1Result, &u.Round2Result, &u.Round1ResultSeen, &u.Round2ResultSeen, &u.IsActive, &u.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(u)
}

// UpdateProfile lets a candidate submit phone/whatsapp after first login.
func (h *AuthHandler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value(appmiddleware.UserIDKey).(int64)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var body struct {
		Phone    string `json:"phone"`
		WhatsApp string `json:"whatsapp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Phone == "" || body.WhatsApp == "" {
		http.Error(w, "phone and whatsapp are both required", http.StatusBadRequest)
		return
	}

	_, err := h.Pool.Exec(r.Context(), `
		UPDATE users SET phone = $1, whatsapp = $2 WHERE id = $3
	`, body.Phone, body.WhatsApp, userID)
	if err != nil {
		http.Error(w, "failed to update profile", http.StatusInternalServerError)
		return
	}

	if h.Sheets != nil {
		var name, email string
		if err := h.Pool.QueryRow(r.Context(), `SELECT COALESCE(name, ''), campus_email FROM users WHERE id = $1`, userID).Scan(&name, &email); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.Sheets.UpsertRowAtColumn(ctx, "Sheet1", "B", email,
				[]interface{}{name, email, body.Phone, body.WhatsApp}); err != nil {
				log.Printf("warning: failed to sync candidate sheet row on profile update for %s: %v", email, err)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Logout deletes the session server-side and clears the cookie.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		h.Pool.Exec(r.Context(), `DELETE FROM sessions WHERE id = $1`, cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}
