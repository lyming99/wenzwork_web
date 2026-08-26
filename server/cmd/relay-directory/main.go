package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"github.com/wenzwork/wenzwork-web/server/internal/relaydirectory"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gorm.io/gorm"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config.LoadDevelopmentEnv()
	environment := valueOrDefault("APP_ENV", "development")
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		log.Error("Relay Directory database startup failed", "error", err)
		os.Exit(1)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	store, err := relaymanagement.NewStore(db, nil)
	if err != nil {
		log.Error("Relay Directory store startup failed", "error", err)
		os.Exit(1)
	}
	service, err := relaydirectory.NewGRPCService(store, log)
	if err != nil {
		log.Error("Relay Directory gRPC startup failed", "error", err)
		os.Exit(1)
	}

	serverCertificate, nodeCAPEM, err := directoryTLS(environment)
	if err != nil {
		log.Error("Relay Directory TLS startup failed", "error", err)
		os.Exit(1)
	}
	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(nodeCAPEM) {
		log.Error("Relay node CA certificate is invalid")
		os.Exit(1)
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientRoots,
	}
	grpcServer, err := relaydirectory.NewGRPCServer(service, credentials.NewTLS(tlsConfig))
	if err != nil {
		log.Error("Relay Directory gRPC startup failed", "error", err)
		os.Exit(1)
	}
	directoryAddress := valueOrDefault("RELAY_DIRECTORY_ADDR", ":9443")
	listener, err := net.Listen("tcp", directoryAddress)
	if err != nil {
		log.Error("Relay Directory listener startup failed", "error", err)
		os.Exit(1)
	}

	healthAddress := valueOrDefault("RELAY_DIRECTORY_HEALTH_ADDR", "127.0.0.1:9460")
	if !isLoopbackAddress(healthAddress) {
		log.Error("RELAY_DIRECTORY_HEALTH_ADDR must use a loopback address")
		os.Exit(2)
	}
	healthServer := &http.Server{
		Addr: healthAddress, Handler: directoryHealthHandler(db),
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 2)
	go func() { serverErrors <- grpcServer.Serve(listener) }()
	go func() { serverErrors <- healthServer.ListenAndServe() }()
	log.Info("Relay Directory listening", "address", directoryAddress, "protocol", "grpc", "mtls", true, "health_address", healthAddress)
	select {
	case <-ctx.Done():
		log.Info("Relay Directory shutdown requested")
	case serveErr := <-serverErrors:
		if !errors.Is(serveErr, grpc.ErrServerStopped) && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("Relay Directory failed", "error", serveErr)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = healthServer.Shutdown(shutdownCtx)
	grpcStopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(grpcStopped)
	}()
	select {
	case <-grpcStopped:
	case <-shutdownCtx.Done():
		grpcServer.Stop()
	}
}

func directoryHealthHandler(db *gorm.DB) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		if request.URL.Path != "/health/live" && request.URL.Path != "/health/ready" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Path == "/health/ready" {
			readyContext, cancel := context.WithTimeout(request.Context(), 2*time.Second)
			defer cancel()
			if database.Ready(readyContext, db) != nil {
				http.Error(writer, "not ready", http.StatusServiceUnavailable)
				return
			}
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})
}

func directoryTLS(environment string) (tls.Certificate, []byte, error) {
	listenerCertificatePath := strings.TrimSpace(os.Getenv("RELAY_DIRECTORY_TLS_CERTIFICATE_FILE"))
	listenerKeyPath := strings.TrimSpace(os.Getenv("RELAY_DIRECTORY_TLS_PRIVATE_KEY_FILE"))
	nodeCAPath := strings.TrimSpace(os.Getenv("RELAY_NODE_CA_CERTIFICATE_FILE"))
	if listenerCertificatePath != "" || listenerKeyPath != "" {
		if listenerCertificatePath == "" || listenerKeyPath == "" || nodeCAPath == "" {
			return tls.Certificate{}, nil, errors.New("Directory TLS certificate/key and Relay node CA certificate must be configured together")
		}
		certificate, err := tls.LoadX509KeyPair(listenerCertificatePath, listenerKeyPath)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		nodeCAPEM, err := os.ReadFile(nodeCAPath)
		return certificate, nodeCAPEM, err
	}
	if environment == "production" {
		return tls.Certificate{}, nil, errors.New("managed Relay Directory TLS files are required in production")
	}
	caDirectory := valueOrDefault("RELAY_DEVELOPMENT_CA_DIR", "cache/relay-ca")
	authority, err := relayidentity.LoadOrCreateDevelopmentCA(caDirectory, time.Now().UTC())
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	issued, err := authority.IssueServer([]string{"localhost", "127.0.0.1", "::1"}, time.Now().UTC(), 24*time.Hour)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certificate, err := tls.X509KeyPair(issued.CertificatePEM, issued.PrivateKeyPEM)
	return certificate, authority.CAPEM(), err
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func isLoopbackAddress(value string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	address := net.ParseIP(host)
	return host == "localhost" || (address != nil && address.IsLoopback())
}
