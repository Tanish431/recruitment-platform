package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/Tanish431/recruitment-platform/internal/config"
	"github.com/Tanish431/recruitment-platform/internal/db"
	"github.com/Tanish431/recruitment-platform/internal/router"
	"github.com/Tanish431/recruitment-platform/internal/sheets"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
	}
	defer pool.Close()

	var sheetsClient *sheets.Client
	if cfg.SheetsCredentialsPath != "" && cfg.GoogleSheetID != "" {
		sheetsClient, err = sheets.New(ctx, cfg.SheetsCredentialsPath, cfg.GoogleSheetID)
		if err != nil {
			log.Printf("warning: sheets client failed to init, sync disabled: %v", err)
			sheetsClient = nil
		} else {
			log.Println("sheets sync enabled")
		}
	} else {
		log.Println("sheets credentials not configured, sync disabled")
	}

	r := router.New(cfg, pool, sheetsClient)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 20 * time.Second,
	}

	go func() {
		log.Printf("server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
	log.Println("server stopped cleanly")
}
