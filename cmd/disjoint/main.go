package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"disjoint/internal/database"
	"disjoint/internal/handlers"
	"disjoint/internal/worker"
)

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func main() {
	host := getEnv("HOST", "0.0.0.0")
	port := getEnv("PORT", "8080")
	dbPath := getEnv("DATABASE_PATH", "disjoint.db")
	uploadDir := getEnv("UPLOAD_DIR", "uploads")
	sessionSecret := getEnv("SESSION_SECRET", "disjoint-default-secret-key-please-change-in-production")

	if sessionSecret == "disjoint-default-secret-key-please-change-in-production" {
		log.Println("[disjoint] Notice: SESSION_SECRET not set, using default key. Set SESSION_SECRET in production.")
	}

	// Ensure upload directory exists
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatalf("[disjoint] Failed to create upload directory %s: %v", uploadDir, err)
	}

	// Open database and run migrations
	db, queries, err := database.OpenDB(dbPath)
	if err != nil {
		log.Fatalf("[disjoint] Database initialization failed: %v", err)
	}
	defer db.Close()
	log.Printf("[disjoint] Database connected successfully at %s", dbPath)

	// Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start background cleanup worker for expiring threads
	cleanupWorker := worker.New(queries, uploadDir, 1*time.Minute)
	cleanupWorker.Start(ctx)

	// Initialize App and routes
	app, err := handlers.NewApp(db, queries, uploadDir, sessionSecret)
	if err != nil {
		log.Fatalf("[disjoint] App initialization failed: %v", err)
	}

	server := &http.Server{
		Addr:         host + ":" + port,
		Handler:      app.Routes(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Signal handling for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[disjoint] Server listening on http://%s:%s", host, port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[disjoint] HTTP server error: %v", err)
		}
	}()

	<-stop
	log.Println("[disjoint] Shutting down gracefully...")

	// Cancel background workers
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[disjoint] Error during server shutdown: %v", err)
	}

	fmt.Println("[disjoint] Server exited clean.")
}
