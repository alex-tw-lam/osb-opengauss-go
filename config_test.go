package main

import (
	"os"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("GAUSSDB_TABLESPACES", " ts_ssd , ts_hdd ,")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageMode != "role_quota" || cfg.PlansFile != "plans.toml" || cfg.TablespacePrefix != "broker" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if len(cfg.Tablespaces) != 2 || cfg.Tablespaces[0] != "ts_ssd" {
		t.Fatalf("tablespace allowlist wrong: %v", cfg.Tablespaces)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	t.Setenv("GAUSSDB_STORAGE_MODE", "bogus")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "GAUSSDB_STORAGE_MODE") {
		t.Fatalf("expected storage mode error, got %v", err)
	}
}

func TestLoadConfigRejectsMultiSegmentPrefix(t *testing.T) {
	t.Setenv("GAUSSDB_STORAGE_MODE", "role_quota")
	t.Setenv("GAUSSDB_TABLESPACE_LOCATION_PREFIX", "a/b")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "single path segment") {
		t.Fatalf("expected prefix error, got %v", err)
	}
}

func TestLoadConfigReadsDotenvFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("GAUSSDB_HOST=from-env-file\nBROKER_PORT=6000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBHost != "from-env-file" || cfg.Port != 6000 {
		t.Fatalf(".env not applied: %+v", cfg)
	}
}

func TestLoadConfigRealEnvironmentWinsOverDotenv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GAUSSDB_HOST", "from-real-env")
	if err := os.WriteFile(".env", []byte("GAUSSDB_HOST=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBHost != "from-real-env" {
		t.Fatalf("real environment must win: %+v", cfg)
	}
}

func TestLoadConfigFailsOnBrokenDotenv(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("this is not a valid line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("a malformed .env must fail loudly")
	}
}
