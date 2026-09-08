package main

import (
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlansFile != "plans.toml" || cfg.TablespacePrefix != "broker" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestLoadConfigRejectsMultiSegmentPrefix(t *testing.T) {
	t.Setenv("GAUSSDB_TABLESPACE_LOCATION_PREFIX", "a/b")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "single path segment") {
		t.Fatalf("expected prefix error, got %v", err)
	}
}
