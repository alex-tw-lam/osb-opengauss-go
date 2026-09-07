// config.go is the only file that knows environment variable names.
// It parses them once into a Config and nothing else.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds every deployment-specific value. All fields come from
// environment variables with development defaults.
type Config struct {
	// Admin connection to the openGauss/GaussDB instance.
	DBHost           string
	DBPort           int
	DBUser           string
	DBPassword       string
	DBAdminName      string
	DBSSLMode        string
	DBConnTimeout    int
	StorageMode      string // "role_quota" or "tablespace"
	Tablespaces      []string
	PlansFile        string
	TablespacePrefix string

	// Basic auth the platform must use to talk to this broker.
	BrokerUsername string
	BrokerPassword string

	// State storage: SQLite file by default, or STATE_DSN for a
	// PostgreSQL-compatible (or gaussdb://) server.
	StatePath string
	StateDSN  string

	// Prefix for every database / role / user the broker creates.
	NamePrefix string

	// HTTP server bind address.
	Host string
	Port int
}

// LoadConfig reads the environment and returns the effective configuration.
// It fails loudly on values that cannot be used at all.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		DBHost:           env("GAUSSDB_HOST", "localhost"),
		DBPort:           envInt("GAUSSDB_PORT", 5432),
		DBUser:           env("GAUSSDB_ADMIN_USER", "gaussdb"),
		DBPassword:       os.Getenv("GAUSSDB_ADMIN_PASSWORD"),
		DBAdminName:      env("GAUSSDB_ADMIN_DB", "postgres"),
		DBSSLMode:        env("GAUSSDB_SSLMODE", "disable"),
		DBConnTimeout:    envInt("GAUSSDB_CONNECT_TIMEOUT", 10),
		StorageMode:      env("GAUSSDB_STORAGE_MODE", "role_quota"),
		PlansFile:        env("GAUSSDB_PLANS_FILE", "plans.toml"),
		TablespacePrefix: env("GAUSSDB_TABLESPACE_LOCATION_PREFIX", "broker"),
		BrokerUsername:   env("BROKER_USERNAME", "broker"),
		BrokerPassword:   env("BROKER_PASSWORD", "broker-dev-password"),
		StatePath:        env("STATE_DB_PATH", "osb-opengauss-state.db"),
		StateDSN:         os.Getenv("STATE_DSN"),
		NamePrefix:       env("GAUSSDB_NAME_PREFIX", "gdb"),
		Host:             env("BROKER_HOST", "127.0.0.1"),
		Port:             envInt("BROKER_PORT", 5000),
	}
	if cfg.StorageMode != "role_quota" && cfg.StorageMode != "tablespace" {
		return nil, fmt.Errorf("GAUSSDB_STORAGE_MODE must be role_quota or tablespace, got %q", cfg.StorageMode)
	}
	cfg.TablespacePrefix = strings.Trim(cfg.TablespacePrefix, "/")
	if strings.Contains(cfg.TablespacePrefix, "/") {
		return nil, fmt.Errorf("GAUSSDB_TABLESPACE_LOCATION_PREFIX must be a single path segment")
	}
	for _, name := range strings.Split(os.Getenv("GAUSSDB_TABLESPACES"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.Tablespaces = append(cfg.Tablespaces, name)
		}
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}
