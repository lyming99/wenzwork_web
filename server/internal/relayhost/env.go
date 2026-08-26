package relayhost

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

const DefaultEnvFile = "/etc/wenzwork-relay/relay.env"

type EnvironmentConfig struct {
	ManagementURL string
	AccessKey     string
	Version       string
}

func LoadEnvironment() (EnvironmentConfig, error) {
	if path := strings.TrimSpace(os.Getenv("RELAY_ENV_FILE")); path != "" {
		if err := godotenv.Overload(path); err != nil {
			return EnvironmentConfig{}, fmt.Errorf("load RELAY_ENV_FILE: %w", err)
		}
	} else if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(".env"); err != nil {
			return EnvironmentConfig{}, fmt.Errorf("load Relay .env: %w", err)
		}
	} else if _, err := os.Stat(DefaultEnvFile); err == nil {
		if err := godotenv.Load(DefaultEnvFile); err != nil {
			return EnvironmentConfig{}, fmt.Errorf("load default Relay environment: %w", err)
		}
	}
	result := EnvironmentConfig{
		ManagementURL: strings.TrimSpace(os.Getenv("RELAY_MANAGEMENT_URL")),
		AccessKey:     strings.TrimSpace(os.Getenv("RELAY_ACCESS_KEY")),
		Version:       envDefault("RELAY_VERSION", "0.0.0"),
	}
	if result.AccessKey == "" {
		return EnvironmentConfig{}, errors.New("RELAY_ACCESS_KEY is required")
	}
	return result, nil
}

func (config EnvironmentConfig) RuntimeConfig(binding relaymanagement.AccessKeyBinding) Config {
	runtime := binding.Configuration
	return Config{
		AccessKeyMode: true, ConfigurationVersion: binding.ConfigurationVersion,
		InstallationID: binding.InstallationID, CellID: binding.CellID,
		Version: config.Version, ProtocolVersion: runtime.ProtocolVersion, DirectoryURL: config.ManagementURL,
		PublicEndpoint: runtime.PublicEndpoint, BrowserOriginPatterns: append([]string(nil), runtime.BrowserOriginPatterns...),
		ListenAddress: runtime.ListenAddress, HealthAddress: runtime.HealthAddress,
		RedisURL: runtime.RedisURL, TicketIssuer: runtime.TicketIssuer,
		TicketPublicKeys:          runtime.TicketPublicKeys,
		DeviceLinkGrantPublicKeys: runtime.DeviceLinkGrantPublicKeys,
		ConnectionHardLimit:       runtime.ConnectionHardLimit, HandshakeConcurrency: runtime.HandshakeConcurrency,
	}
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
