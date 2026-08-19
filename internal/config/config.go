package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                  string
	DatabaseURL           string
	GoogleClientID        string
	GoogleClientSecret    string
	GoogleRedirectURL     string
	AllowedEmailDomain    string
	SessionSecret         string
	SheetsCredentialsPath string
	GoogleSheetID         string
	FrontendURL           string
	CookieDomain          string
	CookieSecure          bool
	CORSOrigin            string
}

func Load() (*Config, error) {
	_ = godotenv.Load() // ok if .env missing in prod, env vars may be set directly

	cfg := &Config{
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		GoogleClientID:        os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:    os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:     os.Getenv("GOOGLE_REDIRECT_URL"),
		AllowedEmailDomain:    os.Getenv("ALLOWED_EMAIL_DOMAIN"),
		SessionSecret:         os.Getenv("SESSION_SECRET"),
		SheetsCredentialsPath: os.Getenv("GOOGLE_SHEETS_CREDENTIALS_PATH"),
		GoogleSheetID:         os.Getenv("GOOGLE_SHEET_ID"),
		FrontendURL:           os.Getenv("FRONTEND_URL"),
		CookieDomain:          getEnv("COOKIE_DOMAIN", ""),
		CookieSecure:          getEnv("COOKIE_SECURE", "false") == "true",
		CORSOrigin:            getEnv("CORS_ORIGIN", "http://localhost:3000"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.GoogleClientID == "" || cfg.GoogleClientSecret == "" {
		return nil, fmt.Errorf("google oauth credentials are required")
	}
	if cfg.AllowedEmailDomain == "" {
		return nil, fmt.Errorf("ALLOWED_EMAIL_DOMAIN is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
