package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config.LoadDevelopmentEnv()
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		log.Error("Relay Scheduler database startup failed", "error", err)
		os.Exit(1)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	healthAddress := valueOrDefault("RELAY_SCHEDULER_HEALTH_ADDR", "127.0.0.1:9462")
	if !isLoopbackAddress(healthAddress) {
		log.Error("RELAY_SCHEDULER_HEALTH_ADDR must use a loopback address")
		os.Exit(2)
	}
	server := &http.Server{
		Addr: healthAddress,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("X-Content-Type-Options", "nosniff")
			if request.URL.Path != "/health/ready" && request.URL.Path != "/health/live" {
				http.NotFound(writer, request)
				return
			}
			readyContext, cancel := context.WithTimeout(request.Context(), 2*time.Second)
			defer cancel()
			if request.URL.Path == "/health/ready" && database.Ready(readyContext, db) != nil {
				http.Error(writer, "not ready", http.StatusServiceUnavailable)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"ok"}`))
		}),
		ReadHeaderTimeout: 3 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	log.Info("Relay Scheduler started", "health_address", server.Addr)
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Error("Relay Scheduler shutdown failed", "error", err)
			os.Exit(1)
		}
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("Relay Scheduler failed", "error", err)
			os.Exit(1)
		}
	}
}

func isLoopbackAddress(value string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	address := net.ParseIP(host)
	return host == "localhost" || (address != nil && address.IsLoopback())
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
