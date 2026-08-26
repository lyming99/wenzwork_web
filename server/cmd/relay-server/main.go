package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relaydirectory"
	"github.com/wenzwork/wenzwork-web/server/internal/relayhost"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/relayruntime"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

type relayDirectoryConnection interface {
	relayruntime.DirectoryClient
	Close() error
}

// defaultManagementURL is embedded in official Relay builds. Private or local
// deployments can still override it with RELAY_MANAGEMENT_URL or -ldflags.
var defaultManagementURL = "https://wenzwork.com"

// version is injected by the Release build. Development builds intentionally
// retain 0.0.0 unless RELAY_VERSION or the legacy config supplies a value.
var version = "0.0.0"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if handled, err := runAsWindowsServiceIfNeeded(); handled {
		if err != nil {
			log.Error("Relay Windows Service stopped", "error", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	environmentConfig, environmentErr := relayhost.LoadEnvironment()
	environmentMode := environmentErr == nil || strings.TrimSpace(os.Getenv("RELAY_ENV_FILE")) != "" ||
		strings.TrimSpace(os.Getenv("RELAY_MANAGEMENT_URL")) != "" || strings.TrimSpace(os.Getenv("RELAY_ACCESS_KEY")) != ""
	var config relayhost.Config
	var directory relayDirectoryConnection
	var identityPrivateKey ed25519.PrivateKey
	if environmentMode {
		if environmentErr != nil {
			log.Error("Relay environment startup failed", "error", environmentErr)
			os.Exit(78)
		}
		if strings.TrimSpace(environmentConfig.ManagementURL) == "" {
			environmentConfig.ManagementURL = defaultManagementURL
		}
		environmentConfig.Version = effectiveRelayVersion(environmentConfig.Version)
		accessClient, err := relaydirectory.NewAccessKeyClient(environmentConfig.ManagementURL, environmentConfig.AccessKey)
		if err != nil {
			log.Error("Relay Access Key client startup failed", "error", err)
			os.Exit(78)
		}
		binding, err := resolveAccessKeyBinding(ctx, accessClient, environmentConfig, log)
		if err != nil {
			_ = accessClient.Close()
			log.Error("Relay Access Key startup failed", "error", err)
			if errors.Is(err, relaymanagement.ErrAccessKeyInvalid) || errors.Is(err, relaymanagement.ErrInstallationRevoked) {
				os.Exit(78)
			}
			os.Exit(1)
		}
		config = environmentConfig.RuntimeConfig(binding)
		directory = accessClient
		log.Info("Relay environment loaded", "management_url", environmentConfig.ManagementURL, "installation_id", binding.InstallationID, "cell_id", binding.CellID)
	} else {
		configPath := strings.TrimSpace(os.Getenv("WENZWORK_RELAY_CONFIG"))
		if configPath == "" {
			configPath = relayhost.DefaultConfigFile
		}
		legacyConfig, err := relayhost.Load(configPath)
		if err != nil {
			log.Error("Relay configuration startup failed", "error", err)
			os.Exit(1)
		}
		privateKeyPEM, err := os.ReadFile(filepath.Clean(legacyConfig.IdentityPrivateKeyFile))
		if err != nil {
			log.Error("Relay identity startup failed", "error", err)
			os.Exit(1)
		}
		identityPrivateKey, err = relayidentity.ParsePrivateKeyPEM(privateKeyPEM)
		if err != nil {
			log.Error("Relay identity startup failed", "error", err)
			os.Exit(1)
		}
		certificatePEM, err := os.ReadFile(filepath.Clean(legacyConfig.CertificateFile))
		if err != nil {
			log.Error("Relay certificate startup failed", "error", err)
			os.Exit(1)
		}
		caPEM, err := os.ReadFile(filepath.Clean(legacyConfig.CACertificateFile))
		if err != nil {
			log.Error("Relay CA startup failed", "error", err)
			os.Exit(1)
		}
		legacyDirectory, err := relaydirectory.NewClient(legacyConfig.DirectoryURL, certificatePEM, privateKeyPEM, caPEM)
		if err != nil {
			log.Error("Relay Directory client startup failed", "error", err)
			os.Exit(1)
		}
		legacyConfig.Version = effectiveRelayVersion(legacyConfig.Version)
		config, directory = legacyConfig, legacyDirectory
	}
	defer directory.Close()
	instanceID := uuid.New()
	options := []relayruntime.Option{relayruntime.WithInstanceID(instanceID)}
	if len(identityPrivateKey) == ed25519.PrivateKeySize {
		options = append(options, relayruntime.WithIdentityPrivateKey(identityPrivateKey))
	}
	if config.DataPlaneConfigured() {
		if err := config.ValidateDataPlane(); err != nil {
			log.Error("Relay data-plane configuration startup failed", "error", err)
			os.Exit(2)
		}
		ticketKeys, keyErr := loadTicketPublicKeys(config.TicketPublicKeyFiles, config.TicketPublicKeys)
		if keyErr != nil {
			log.Error("Relay connection Ticket verification key startup failed", "error", keyErr)
			os.Exit(1)
		}
		deviceLinkGrantKeys, deviceLinkKeyErr := loadTicketPublicKeys(config.DeviceLinkGrantPublicKeyFiles, config.DeviceLinkGrantPublicKeys)
		if deviceLinkKeyErr != nil {
			log.Error("Relay v2 device link grant verification key startup failed", "error", deviceLinkKeyErr)
			os.Exit(1)
		}
		if keyErr := validateIndependentTicketPublicKeys(ticketKeys, deviceLinkGrantKeys); keyErr != nil {
			log.Error("Relay Ticket verification key isolation failed", "error", keyErr)
			os.Exit(1)
		}
		routeStore, registryErr := relayrouter.NewNegotiatedRedisRegistryFromURL(config.RedisURL)
		if registryErr != nil {
			log.Error("Relay negotiated route store startup failed", "error", registryErr)
			os.Exit(1)
		}
		defer routeStore.Close()
		verifier := remoteauth.Verifier{Issuer: config.TicketIssuer, Keys: ticketKeys, Leeway: 5 * time.Second}
		grantUses, grantUseErr := relayserver.NewRedisV2GrantUseStoreFromURL(config.RedisURL)
		if grantUseErr != nil {
			log.Error("Relay v2 grant replay store startup failed", "error", grantUseErr)
			os.Exit(1)
		}
		defer grantUses.Close()
		v2Hub := relayserver.NewV2Hub()
		v2Handler := &relayserver.V2Handler{
			CellID: config.CellID.String(), NodeID: instanceID.String(), BrowserOriginPatterns: config.BrowserOriginPatterns,
			ClientGrantVerifier: remoteauth.DeviceLinkGrantVerifier{Issuer: config.TicketIssuer, Keys: deviceLinkGrantKeys, Leeway: 5 * time.Second},
			DeviceAuthenticator: relayserver.V2TicketDeviceAuthenticator{Verifier: verifier, CellID: config.CellID.String(), NodeID: instanceID.String()},
			GrantUses:           grantUses, Hub: v2Hub,
			ProtocolFailure: func(failure relayserver.V2ProtocolFailure) {
				log.Warn("Relay v2 protocol failure", "stage", failure.Stage, "reason", failure.Reason, "role", failure.Role,
					"protocol_major", failure.ProtocolMajor, "frame_size_bucket", v2FrameSizeBucket(failure.FrameSizeBytes))
			},
		}
		options = append(options, relayruntime.WithDataPlaneV2(v2Handler, v2Hub, routeStore))
	} else {
		log.Warn("Relay data plane is not configured; WSS and routing readiness remain fail-closed")
	}
	relay, err := relayruntime.New(config, directory, log, options...)
	if err != nil {
		log.Error("Relay runtime startup failed", "error", err)
		os.Exit(1)
	}
	if err := relay.Run(ctx); err != nil {
		log.Error("Relay runtime stopped", "error", err)
		if errors.Is(err, relayruntime.ErrRevoked) || errors.Is(err, relaymanagement.ErrAccessKeyInvalid) {
			os.Exit(78)
		}
		os.Exit(1)
	}
}

func effectiveRelayVersion(configured string) string {
	if configured == "0.0.0" && version != "0.0.0" {
		return version
	}
	return configured
}

func resolveAccessKeyBinding(ctx context.Context, client *relaydirectory.AccessKeyClient, environment relayhost.EnvironmentConfig, log *slog.Logger) (relaymanagement.AccessKeyBinding, error) {
	backoff := time.Second
	for {
		requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		binding, err := client.Resolve(requestContext)
		cancel()
		if err == nil {
			candidate := environment.RuntimeConfig(binding)
			if validationErr := candidate.Validate(); validationErr != nil {
				err = fmt.Errorf("Relay management configuration is incomplete: %w", validationErr)
			} else if !candidate.DataPlaneConfigured() {
				err = errors.New("Relay management data-plane configuration is incomplete")
			} else if validationErr := candidate.ValidateDataPlane(); validationErr != nil {
				err = fmt.Errorf("Relay management data-plane configuration is invalid: %w", validationErr)
			} else {
				return binding, nil
			}
		}
		if errors.Is(err, relaymanagement.ErrAccessKeyInvalid) || errors.Is(err, relaymanagement.ErrInstallationRevoked) {
			return relaymanagement.AccessKeyBinding{}, err
		}
		log.Warn("Relay management connection failed", "error", err, "retry_after", backoff.String())
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return relaymanagement.AccessKeyBinding{}, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func loadTicketPublicKeys(files, encodedKeys map[string]string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey, len(files)+len(encodedKeys))
	for keyID, path := range files {
		contents, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("read verification key %q: %w", keyID, err)
		}
		publicKey, err := relayidentity.ParsePublicKeyPEM(contents)
		if err != nil {
			return nil, fmt.Errorf("parse verification key %q: %w", keyID, err)
		}
		keys[keyID] = publicKey
	}
	for keyID, encoded := range encodedKeys {
		if _, exists := keys[keyID]; exists {
			return nil, fmt.Errorf("verification key ID %q is duplicated", keyID)
		}
		publicKey, err := relayidentity.DecodePublicKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("parse downloaded verification key %q: %w", keyID, err)
		}
		keys[keyID] = publicKey
	}
	return keys, nil
}

func validateIndependentTicketPublicKeys(keySets ...map[string]ed25519.PublicKey) error {
	for leftIndex, left := range keySets {
		for rightIndex := leftIndex + 1; rightIndex < len(keySets); rightIndex++ {
			for leftKeyID, leftKey := range left {
				for rightKeyID, rightKey := range keySets[rightIndex] {
					if leftKeyID == rightKeyID || leftKey.Equal(rightKey) {
						return fmt.Errorf("Relay verification keys %q and %q must be independent", leftKeyID, rightKeyID)
					}
				}
			}
		}
	}
	return nil
}

func v2FrameSizeBucket(size int) string {
	switch {
	case size <= 0:
		return "0"
	case size <= 1<<10:
		return "1KiB"
	case size <= 64<<10:
		return "64KiB"
	case size <= 1<<20:
		return "1MiB"
	default:
		return "over_1MiB"
	}
}
