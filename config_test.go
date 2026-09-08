package main

import (
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("GAUSSDB_TABLESPACES", " ts_ssd , ts_hdd ,")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlansFile != "plans.toml" || cfg.TablespacePrefix != "broker" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if len(cfg.Tablespaces) != 2 || cfg.Tablespaces[0] != "ts_ssd" {
		t.Fatalf("tablespace allowlist wrong: %v", cfg.Tablespaces)
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
