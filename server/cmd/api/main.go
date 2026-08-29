package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/analytics"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/emailsettings"
	"github.com/wenzwork/wenzwork-web/server/internal/feedback"
	"github.com/wenzwork/wenzwork-web/server/internal/helpdocs"
	"github.com/wenzwork/wenzwork-web/server/internal/httpapi"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
	"github.com/wenzwork/wenzwork-web/server/internal/relayallocation"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrelease"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
	"github.com/wenzwork/wenzwork-web/server/internal/releaseassets"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
	"github.com/wenzwork/wenzwork-web/server/internal/systemsetup"
	"github.com/wenzwork/wenzwork-web/server/internal/webui"
	"gorm.io/gorm"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	config.LoadDevelopmentEnv()
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	disableAdminMFA := !cfg.AdminMFARequired
	if disableAdminMFA {
		log.Info("administrator MFA enforcement is disabled by configuration")
	}
	relayBootstrapAssetsDir := cfg.RelayBootstrapAssetsDir
	if err := relayrelease.ValidateBootstrapAssets(relayBootstrapAssetsDir, cfg.Environment == "production"); err != nil {
		if cfg.Environment != "production" {
			log.Error("Relay bootstrap asset startup failed", "error", err)
			os.Exit(1)
		}
		// Relay bootstrap delivery is an optional supply-chain surface. Keep the
		// Host available, but fail that surface closed when a portable package
		// only contains the repository test key or incomplete verifier assets.
		log.Warn("Relay online bootstrap disabled because trusted assets are unavailable", "error", err)
		relayBootstrapAssetsDir = ""
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Error("database handle failed", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	administratorState, err := ensureDefaultAdministrator(ctx, databaseAdministratorBootstrapper{db: db}, defaultAdministratorConfig{
		Email:       os.Getenv("SYSTEM_ADMIN_EMAIL"),
		Password:    os.Getenv("SYSTEM_ADMIN_PASSWORD"),
		DisplayName: os.Getenv("SYSTEM_ADMIN_DISPLAY_NAME"),
		PasswordParams: auth.Argon2Params{
			MemoryKiB: cfg.Argon2MemoryKiB, Iterations: cfg.Argon2Iterations, Parallelism: cfg.Argon2Parallelism,
			SaltLength: 16, KeyLength: 32,
		},
	})
	if err != nil {
		log.Error("default administrator startup failed", "error", err)
		os.Exit(1)
	}
	if administratorState.Created {
		log.Info("default administrator created from Host configuration", "email", administratorState.Email)
	} else {
		log.Info("Host administrator is already initialized", "email", administratorState.Email)
	}
	var relayCA *relayidentity.CertificateAuthority
	if cfg.RelayCACertificateFile != "" {
		relayCA, err = relayidentity.LoadCertificateAuthority(cfg.RelayCACertificateFile, cfg.RelayCAPrivateKeyFile)
	} else {
		relayCA, err = relayidentity.LoadOrCreateCertificateAuthority(cfg.RelayDevelopmentCADir, time.Now().UTC())
	}
	if err != nil {
		log.Error("Relay certificate authority startup failed", "error", err)
		os.Exit(1)
	}
	relayStore, err := relaymanagement.NewStore(db, relayCA)
	if err != nil {
		log.Error("Relay management startup failed", "error", err)
		os.Exit(1)
	}
	go runRelayMaintenance(ctx, relayStore, log)
	locationResolvers := make([]analytics.LocationResolver, 0, 2)
	if cfg.GeoIPCityDatabasePath != "" {
		geoResolver, err := analytics.NewGeoIPResolver(cfg.GeoIPCityDatabasePath)
		if err != nil {
			log.Error("GeoIP City database startup failed", "error", err, "path", cfg.GeoIPCityDatabasePath)
			os.Exit(1)
		}
		defer geoResolver.Close()
		locationResolvers = append(locationResolvers, geoResolver)
		log.Info("GeoIP City database loaded", "path", cfg.GeoIPCityDatabasePath)
	}
	if cfg.IPGeolocationAPIEnabled {
		locationResolvers = append(locationResolvers, analytics.NewAPILocationResolver())
		log.Info("on-demand IP geolocation APIs enabled", "providers", "toolshu,ip-api", "cache", "database")
	} else if len(locationResolvers) == 0 {
		log.Warn("IP geolocation is disabled; public IP regions will be shown as unknown")
	}
	locationResolver := analytics.NewFallbackLocationResolver(locationResolvers...)
	analyticsStore, err := analytics.NewStore(db, locationResolver)
	if err != nil {
		log.Error("analytics startup failed", "error", err)
		os.Exit(1)
	}
	catalogStore, err := catalog.NewStore(db, catalog.WithReleaseSourceTokenEncryptionKey(cfg.MFAEncryptionKey))
	if err != nil {
		log.Error("catalog startup failed", "error", err)
		os.Exit(1)
	}
	for _, source := range []struct {
		project, repository, legacyToken string
	}{
		{catalog.ReleaseProjectWeb, cfg.GitHubReleaseRepository, cfg.GitHubReleaseToken},
		{catalog.ReleaseProjectDesktop, cfg.DesktopGitHubReleaseRepository, ""},
		{catalog.ReleaseProjectMobile, cfg.MobileGitHubReleaseRepository, ""},
	} {
		if err := catalogStore.EnsureReleaseProjectSourceSettings(ctx, source.project, source.repository, source.legacyToken); err != nil {
			log.Error("release source settings startup failed", "project", source.project, "error", err)
			os.Exit(1)
		}
	}
	if err := catalogStore.EnsureReleaseAccessKey(ctx, cfg.ReleaseAccessKey); err != nil {
		log.Error("release access key startup failed", "error", err)
		os.Exit(1)
	}
	var releaseUploader httpapi.ReleaseAssetUploadService
	var releaseCache *objectstore.ReleaseAssetCache
	releasePushAssets, err := objectstore.NewLocalReleaseAssetStore(cfg.ReleasePushStorageDir)
	if err != nil {
		log.Error("local release push storage startup failed", "error", err)
		os.Exit(1)
	}
	if cfg.S3Endpoint != "" || cfg.S3Region != "" || cfg.S3Bucket != "" ||
		cfg.S3AccessKeyID != "" || cfg.S3SecretAccessKey != "" || cfg.DownloadCDNBaseURL != "" {
		releaseUploader, err = objectstore.NewReleaseAssetUploader(objectstore.S3Config{
			Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
			AccessKeyID: cfg.S3AccessKeyID, SecretAccessKey: cfg.S3SecretAccessKey,
			SessionToken: cfg.S3SessionToken, AddressingStyle: cfg.S3AddressingStyle,
		}, cfg.DownloadCDNBaseURL)
		if err != nil {
			log.Error("release asset storage startup failed", "error", err)
			os.Exit(1)
		}
		releaseCache, err = objectstore.NewReleaseAssetCache(objectstore.S3Config{
			Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket,
			AccessKeyID: cfg.S3AccessKeyID, SecretAccessKey: cfg.S3SecretAccessKey,
			SessionToken: cfg.S3SessionToken, AddressingStyle: cfg.S3AddressingStyle,
		}, cfg.ReleaseAssetCacheDir)
		if err != nil {
			log.Error("release asset cache startup failed", "error", err)
			os.Exit(1)
		}
	} else {
		releaseCache, err = objectstore.NewLocalReleaseAssetCache(cfg.ReleaseAssetCacheDir)
		if err != nil {
			log.Error("release asset cache startup failed", "error", err)
			os.Exit(1)
		}
		log.Warn("release asset S3 storage is disabled; uploads and S3-backed downloads are unavailable, but GitHub, mirror, and locally pushed downloads remain enabled")
	}
	releaseSources := releaseassets.NewService(releaseUploader)
	releaseDownloads := releaseassets.NewRepositoryDeliveryService(releaseCache, func(ctx context.Context, repository string) (string, error) {
		credentials, err := catalogStore.GetReleaseSourceCredentialsByRepository(ctx, repository)
		return credentials.GitHubToken, err
	}).WithLocalStore(releasePushAssets)
	mailSender, err := emailsettings.NewStore(db, emailsettings.Config{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUser, Password: cfg.SMTPPassword,
		From: cfg.MailFrom, RequireTLS: cfg.Environment == "production", Timeout: 10 * time.Second,
	}, cfg.MFAEncryptionKey)
	if err != nil {
		log.Error("system email settings startup failed", "error", err)
		os.Exit(1)
	}
	authConfig := auth.DefaultServiceConfig()
	authConfig.RegistrationEnabled = cfg.RegistrationEnabled
	authConfig.PublicBaseURL = cfg.PublicBaseURL
	authConfig.MFAEncryptionKey = cfg.MFAEncryptionKey
	authConfig.PasswordParams = auth.Argon2Params{
		MemoryKiB: cfg.Argon2MemoryKiB, Iterations: cfg.Argon2Iterations, Parallelism: cfg.Argon2Parallelism,
		SaltLength: 16, KeyLength: 32,
	}
	authService, err := auth.NewService(db, mailSender, authConfig)
	if err != nil {
		log.Error("auth startup failed", "error", err)
		os.Exit(1)
	}
	systemSetupService := systemsetup.NewService(
		cfg,
		os.Getenv("SYSTEM_ADMIN_EMAIL"),
		os.Getenv("SYSTEM_ADMIN_PASSWORD"),
		os.Getenv("SYSTEM_ADMIN_DISPLAY_NAME"),
	)
	redemptionCodec, err := membership.NewCodeCodec([]byte(cfg.RedemptionCodeHMACKey))
	if err != nil {
		log.Error("membership code codec startup failed", "error", err)
		os.Exit(1)
	}
	membershipStore, err := membership.NewStore(db, redemptionCodec)
	if err != nil {
		log.Error("membership store startup failed", "error", err)
		os.Exit(1)
	}
	remoteAccessPolicy, err := remoteaccesspolicy.NewStore(db)
	if err != nil {
		log.Error("remote access policy startup failed", "error", err)
		os.Exit(1)
	}
	promotionService, err := membership.NewBetaPromotionService(db, redemptionCodec, mailSender, cfg.MFAEncryptionKey)
	if err != nil {
		log.Error("beta promotion startup failed", "error", err)
		os.Exit(1)
	}
	trialPromotionService, err := membership.NewTrialPromotionService(
		db,
		redemptionCodec,
		mailSender,
		cfg.MFAEncryptionKey,
	)
	if err != nil {
		log.Error("trial promotion startup failed", "error", err)
		os.Exit(1)
	}
	lifetimeCodeDeliveryService, err := membership.NewLifetimeCodeDeliveryService(
		db, redemptionCodec, mailSender, cfg.MFAEncryptionKey,
	)
	if err != nil {
		log.Error("lifetime code delivery startup failed", "error", err)
		os.Exit(1)
	}
	helpStore, err := helpdocs.NewStore(db)
	if err != nil {
		log.Error("help document startup failed", "error", err)
		os.Exit(1)
	}
	feedbackStore, err := feedback.NewStore(db)
	if err != nil {
		log.Error("feedback startup failed", "error", err)
		os.Exit(1)
	}
	var remote *remoteRuntime
	if remoteConfigErr := cfg.RemoteMVPConfigurationError(); remoteConfigErr != nil {
		log.Warn("remote MVP capability is fail-closed", "error", remoteConfigErr)
	} else {
		remote, err = newRemoteRuntime(cfg, db, relayStore, remoteAccessPolicy, log)
		if err != nil {
			log.Error("remote MVP capability startup failed; Device API remains fail-closed", "error", err)
			remote = nil
		} else {
			if err := configureRelayAgentRuntime(relayStore, cfg, remote); err != nil {
				remote.Close()
				log.Error("Relay agent configuration startup failed; Device API remains fail-closed", "error", err)
				remote = nil
			} else {
				defer remote.Close()
				log.Info("remote MVP capability enabled", "region", cfg.RemoteMVPRegion, "pool", cfg.RemoteMVPPool, "cell", cfg.RemoteMVPCell, "ticket_ttl", cfg.RelayTicketTTL)
			}
		}
	}
	var remoteDeviceService httpapi.RemoteDeviceService
	var remoteAllocationService httpapi.RemoteAllocationService
	remoteReadiness := func(context.Context) error { return errors.New("remote MVP capability is not configured") }
	if remote != nil {
		remoteDeviceService = remote.devices
		remoteAllocationService = remote.allocations
		remoteReadiness = remote.Ready
	}
	var browserDeviceLinkIssuer remotecontrol.DeviceLinkIssuer
	var browserDeviceLinkRevoker remotecontrol.DeviceLinkGrantRevoker
	if remote != nil {
		browserDeviceLinkIssuer = remote.browserDeviceLinks
		browserDeviceLinkRevoker = remote.deviceLinkGrants
	}
	remoteControlService, err := remotecontrol.NewService(remotecontrol.ServiceConfig{
		Database: db, CursorKey: []byte(cfg.MFAEncryptionKey), DeviceLinkIssuer: browserDeviceLinkIssuer, DeviceLinkRevoker: browserDeviceLinkRevoker,
		RouteResolver: func() remotecontrol.DeviceRouteResolver {
			if remote == nil {
				return nil
			}
			return remote.registry
		}(),
	})
	if err != nil {
		log.Error("remote control startup failed", "error", err)
		os.Exit(1)
	}

	router := httpapi.NewRouter(httpapi.Dependencies{
		Logger:                  log,
		Catalog:                 catalogStore,
		CatalogAdmin:            catalogStore,
		ReleaseUploads:          releaseUploader,
		ReleaseSources:          releaseSources,
		ReleaseDownloads:        releaseDownloads,
		ReleasePush:             catalogStore,
		ReleasePushAssets:       releasePushAssets,
		ReleaseAccessKeys:       catalogStore,
		PricingAdmin:            catalogStore,
		Auth:                    authService,
		AppAuth:                 authService,
		UserAdmin:               authService,
		SystemSetup:             systemSetupService,
		SystemEmail:             mailSender,
		Membership:              membershipStore,
		MembershipAdmin:         membershipStore,
		LifetimeCodeAdmin:       lifetimeCodeDeliveryService,
		Promotion:               promotionService,
		PromotionAdmin:          promotionService,
		TrialPromotion:          trialPromotionService,
		TrialAdmin:              trialPromotionService,
		Help:                    helpStore,
		HelpAdmin:               helpStore,
		Feedback:                feedbackStore,
		Analytics:               analyticsStore,
		TrustedProxies:          cfg.TrustedProxyCIDRs,
		Relay:                   relayStore,
		RelayDefaultRegion:      cfg.RemoteMVPRegion,
		RelayDefaultPool:        cfg.RemoteMVPPool,
		RelayDefaultCell:        cfg.RemoteMVPCell,
		RemoteDevice:            remoteDeviceService,
		RemoteAllocation:        remoteAllocationService,
		RemoteAccessPolicy:      remoteAccessPolicy,
		RemoteControl:           remoteControlService,
		PublicBaseURL:           cfg.PublicBaseURL,
		RelayDirectoryURL:       cfg.RelayDirectoryURL,
		RelayArtifactBaseURL:    cfg.DownloadCDNBaseURL,
		RelayBootstrapAssetsDir: relayBootstrapAssetsDir,
		AuthHTTP: httpapi.AuthHTTPConfig{
			CookieSecure: cfg.CookieSecure, AllowedOrigins: cfg.AllowedOrigins,
			DisableAdminMFA: disableAdminMFA,
		},
		Readiness: func(ctx context.Context) error {
			return database.Ready(ctx, db)
		},
		RemoteReadiness: remoteReadiness,
	})
	handler, err := webui.NewHandler(router, cfg.WebRoot)
	if err != nil {
		log.Error("web application startup failed", "error", err, "web_root", cfg.WebRoot)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		log.Info("server listening", "address", cfg.HTTPAddr, "environment", cfg.Environment, "web_root", cfg.WebRoot)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "error", err)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}

type remoteRuntime struct {
	db                 *gorm.DB
	registry           *relayrouter.NegotiatedRegistry
	issuer             remoteauth.Issuer
	devices            *remotedevice.Service
	allocations        *relayallocation.Service
	deviceLinkGrants   *relayserver.RedisV2GrantUseStore
	browserDeviceLinks *remotecontrol.BrowserDeviceLinkGrantIssuer
	deviceLinkGrantKey ed25519.PublicKey
}

func configureRelayAgentRuntime(store *relaymanagement.Store, cfg config.Config, runtime *remoteRuntime) error {
	if store == nil || runtime == nil {
		return errors.New("Relay agent runtime dependencies are required")
	}
	if len(runtime.deviceLinkGrantKey) != ed25519.PublicKeySize || cfg.RelayDeviceLinkGrantKeyID == "" {
		return errors.New("remote/v2 device link grant key is required")
	}
	connectionPublicKey, ok := runtime.issuer.PrivateKey.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("Relay Ticket public key is invalid")
	}
	encodedConnectionKey, err := relayidentity.EncodePublicKey(connectionPublicKey)
	if err != nil {
		return err
	}
	encodedDeviceLinkKey, err := relayidentity.EncodePublicKey(runtime.deviceLinkGrantKey)
	if err != nil {
		return err
	}
	agentConfig := relaymanagement.AgentRuntimeConfiguration{
		ProtocolVersion: 2, ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
		BrowserOriginPatterns: relayBrowserOriginPatterns(cfg.AllowedOrigins, cfg.PublicBaseURL),
		RedisURL:              cfg.RedisURL, TicketIssuer: cfg.RelayTicketIssuer,
		TicketPublicKeys:          map[string]string{cfg.RelayTicketKeyID: encodedConnectionKey},
		DeviceLinkGrantPublicKeys: map[string]string{cfg.RelayDeviceLinkGrantKeyID: encodedDeviceLinkKey},
		HandshakeConcurrency:      128,
	}
	store.SetAgentRuntimeConfiguration(agentConfig)
	return nil
}

func relayBrowserOriginPatterns(allowedOrigins []string, publicBaseURL string) []string {
	values := append(append([]string(nil), allowedOrigins...), publicBaseURL)
	patterns := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		parsed, err := url.Parse(strings.TrimSpace(value))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			continue
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		patterns = append(patterns, origin)
	}
	return patterns
}

func newRemoteRuntime(cfg config.Config, db *gorm.DB, relayStore *relaymanagement.Store, accessPolicy *remoteaccesspolicy.Store, log *slog.Logger) (*remoteRuntime, error) {
	privateKey, _, err := relayidentity.LoadOrCreatePrivateKey(cfg.RelayTicketPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load or create Relay Ticket private key: %w", err)
	}
	var deviceLinkPrivateKey ed25519.PrivateKey
	var deviceLinkGrantKey ed25519.PublicKey
	if cfg.RelayDeviceLinkGrantKeyID == "" || cfg.RelayDeviceLinkGrantPrivateKeyFile == "" {
		return nil, errors.New("Relay v2 device link grant key ID and private key file are required")
	}
	deviceLinkPrivateKey, _, keyErr := relayidentity.LoadOrCreatePrivateKey(cfg.RelayDeviceLinkGrantPrivateKeyFile)
	if keyErr != nil {
		return nil, fmt.Errorf("load or create Relay v2 device link grant private key: %w", keyErr)
	}
	if deviceLinkPrivateKey.Equal(privateKey) {
		return nil, errors.New("Relay v2 device link grant private key must be independent from the connection Ticket key")
	}
	var keyOK bool
	deviceLinkGrantKey, keyOK = deviceLinkPrivateKey.Public().(ed25519.PublicKey)
	if !keyOK || len(deviceLinkGrantKey) != ed25519.PublicKeySize {
		return nil, errors.New("Relay v2 device link grant public key is invalid")
	}
	registry, err := relayrouter.NewNegotiatedRedisRegistryFromURL(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	deviceStore, err := remotedevice.NewStore(db,
		remotedevice.WithAccessKeyIdempotencyEncryptionKey(cfg.MFAEncryptionKey),
		remotedevice.WithAccessPolicy(accessPolicy),
	)
	if err != nil {
		_ = registry.Close()
		return nil, err
	}
	deviceService, err := remotedevice.NewService(deviceStore)
	if err != nil {
		_ = registry.Close()
		return nil, err
	}
	issuer := remoteauth.Issuer{Issuer: cfg.RelayTicketIssuer, KeyID: cfg.RelayTicketKeyID, PrivateKey: privateKey}
	allocationService, err := relayallocation.NewService(relayallocation.ServiceConfig{
		Database: db, Issuer: issuer,
		AccessPolicy: accessPolicy,
		Region:       cfg.RemoteMVPRegion, Pool: cfg.RemoteMVPPool, Cell: cfg.RemoteMVPCell, TicketTTL: cfg.RelayTicketTTL,
		DeviceLinkGrantIssuer:     optionalDeviceLinkGrantIssuer(cfg.RelayTicketIssuer, cfg.RelayDeviceLinkGrantKeyID, deviceLinkGrantKey),
		DeviceLinkGrantPublicKeys: optionalDeviceLinkGrantPublicKeys(cfg.RelayDeviceLinkGrantKeyID, deviceLinkGrantKey),
	})
	if err != nil {
		_ = registry.Close()
		return nil, err
	}
	deviceLinkGrants, err := relayserver.NewRedisV2GrantUseStoreFromURL(cfg.RedisURL)
	if err != nil {
		_ = registry.Close()
		return nil, err
	}
	var browserDeviceLinkIssuer *remotecontrol.BrowserDeviceLinkGrantIssuer
	if len(deviceLinkPrivateKey) == ed25519.PrivateKeySize {
		browserDeviceLinkIssuer, err = remotecontrol.NewBrowserDeviceLinkGrantIssuer(remotecontrol.DeviceLinkGrantIssuerConfig{
			Database: db, Routes: registry,
			Signer:   remoteauth.DeviceLinkGrantIssuer{Issuer: cfg.RelayTicketIssuer, KeyID: cfg.RelayDeviceLinkGrantKeyID, PrivateKey: deviceLinkPrivateKey},
			GrantTTL: cfg.RelayDeviceLinkGrantTTL,
		})
		if err != nil {
			_ = deviceLinkGrants.Close()
			_ = registry.Close()
			return nil, err
		}
	}
	relayStore.SetHeartbeatRoutePublisher(heartbeatRoutePublisher{registry: registry})
	return &remoteRuntime{
		db: db, registry: registry, issuer: issuer,
		devices: deviceService, allocations: allocationService, deviceLinkGrants: deviceLinkGrants,
		browserDeviceLinks: browserDeviceLinkIssuer,
		deviceLinkGrantKey: append(ed25519.PublicKey(nil), deviceLinkGrantKey...),
	}, nil
}

func optionalDeviceLinkGrantPublicKeys(keyID string, publicKey ed25519.PublicKey) map[string]ed25519.PublicKey {
	if keyID == "" || len(publicKey) != ed25519.PublicKeySize {
		return nil
	}
	return map[string]ed25519.PublicKey{keyID: append(ed25519.PublicKey(nil), publicKey...)}
}

func optionalDeviceLinkGrantIssuer(issuer, keyID string, publicKey ed25519.PublicKey) string {
	if keyID == "" || len(publicKey) != ed25519.PublicKeySize {
		return ""
	}
	return issuer
}

type heartbeatRoutePublisher struct {
	registry *relayrouter.NegotiatedRegistry
}

func (publisher heartbeatRoutePublisher) PublishRelayRoutes(ctx context.Context, nodeID, cellID uuid.UUID, routes []relaymanagement.HeartbeatRoute, ttl time.Duration, now time.Time) error {
	if publisher.registry == nil || nodeID == uuid.Nil || cellID == uuid.Nil {
		return errors.New("Relay heartbeat route publisher is unavailable")
	}
	negotiated := make([]relayrouter.Route, 0, len(routes))
	for _, route := range routes {
		negotiated = append(negotiated, relayrouter.Route{
			DeviceID: route.DeviceID, UserID: route.UserID, CellID: cellID.String(), NodeID: nodeID.String(),
			ConnectionID: route.ConnectionID, ConnectionEpoch: route.ConnectionEpoch,
			AssignmentVersion: route.AssignmentVersion, GrantVersion: route.GrantVersion, ProtocolVersion: route.ProtocolVersion,
		})
	}
	return publisher.registry.Publish(ctx, nodeID.String(), negotiated, ttl, now)
}

func (runtime *remoteRuntime) Close() {
	if runtime == nil {
		return
	}
	if runtime.registry != nil {
		_ = runtime.registry.Close()
	}
	if runtime.deviceLinkGrants != nil {
		_ = runtime.deviceLinkGrants.Close()
	}
}

func (runtime *remoteRuntime) Ready(ctx context.Context) error {
	if runtime == nil || runtime.db == nil || runtime.registry == nil ||
		runtime.deviceLinkGrants == nil || runtime.issuer.Issuer == "" || runtime.issuer.KeyID == "" || len(runtime.issuer.PrivateKey) == 0 {
		return errors.New("remote MVP capability is not configured")
	}
	return errors.Join(database.Ready(ctx, runtime.db), runtime.registry.Ping(ctx), runtime.deviceLinkGrants.Ping(ctx))
}
