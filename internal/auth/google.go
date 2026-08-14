package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type GoogleUserInfo struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func NewOAuthConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint:     google.Endpoint,
	}
}

func FetchUserInfo(ctx context.Context, cfg *oauth2.Config, token *oauth2.Token) (*GoogleUserInfo, error) {
	client := cfg.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v3/userinfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var info GoogleUserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ValidateDomain re-checks the email domain server-side. Never trust the
// client-side `hd` param alone — it can be spoofed before it reaches you.
func ValidateDomain(email, allowedDomain string) error {
	if allowedDomain == "*" {
		return nil
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return errors.New("malformed email")
	}
	if !strings.EqualFold(parts[1], allowedDomain) {
		return fmt.Errorf("email domain %q not allowed", parts[1])
	}
	return nil
}
