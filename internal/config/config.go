package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	GRPCAddress             string
	DatabaseURL             string
	AuthorizationGRPCTarget string
	SecretsGRPCTarget       string
	NotificationsGRPCTarget string

	// How often an image's repository is polled. RefreshImage covers the
	// latency this would otherwise add.
	DiscoveryInterval time.Duration
	// Budget for one image's discovery pass, upstream calls included.
	DiscoveryTimeout time.Duration
	// How many images one pass claims. Bounds concurrent outbound registry
	// calls from the control plane.
	DiscoveryBatchSize int
}

func FromEnv() (Config, error) {
	cfg := Config{}
	cfg.GRPCAddress = os.Getenv("GRPC_ADDRESS")
	if cfg.GRPCAddress == "" {
		cfg.GRPCAddress = ":50051"
	}
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL must be set")
	}
	cfg.AuthorizationGRPCTarget = os.Getenv("AUTHORIZATION_GRPC_TARGET")
	if cfg.AuthorizationGRPCTarget == "" {
		return Config{}, fmt.Errorf("AUTHORIZATION_GRPC_TARGET must be set")
	}
	cfg.SecretsGRPCTarget = os.Getenv("SECRETS_GRPC_TARGET")
	if cfg.SecretsGRPCTarget == "" {
		return Config{}, fmt.Errorf("SECRETS_GRPC_TARGET must be set")
	}
	cfg.NotificationsGRPCTarget = os.Getenv("NOTIFICATIONS_GRPC_TARGET")

	var err error
	if cfg.DiscoveryInterval, err = durationFromEnv("DISCOVERY_INTERVAL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.DiscoveryTimeout, err = durationFromEnv("DISCOVERY_TIMEOUT", 60*time.Second); err != nil {
		return Config{}, err
	}
	cfg.DiscoveryBatchSize = 10
	return cfg, nil
}

func durationFromEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}
