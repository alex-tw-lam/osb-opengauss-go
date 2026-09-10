package main

import (
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("BROKER_PASSWORD", "x")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlansFile != "plans.toml" || cfg.TablespacePrefix != "broker" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestLoadConfigRequiresBrokerPassword(t *testing.T) {
	t.Setenv("BROKER_PASSWORD", "")
	_, err := LoadConfig()
	if err == nil || !strings.Contains(err.Error(), "BROKER_PASSWORD") {
		t.Fatalf("expected BROKER_PASSWORD error, got %v", err)
	}
}

func TestLoadConfigRejectsBadNamePrefix(t *testing.T) {
	t.Setenv("BROKER_PASSWORD", "x")
	for _, prefix := range []string{"1abc", "ABC", "has space", "a" + strings.Repeat("b", 30)} {
		t.Setenv("GAUSSDB_NAME_PREFIX", prefix)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("prefix %q must be rejected", prefix)
		}
	}
}

func TestLoadConfigRejectsBadLocationPrefix(t *testing.T) {
	t.Setenv("BROKER_PASSWORD", "x")
	for _, prefix := range []string{"a/b", "a'b", "a;b", "a b"} {
		t.Setenv("GAUSSDB_TABLESPACE_LOCATION_PREFIX", prefix)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("prefix %q must be rejected", prefix)
		}
	}
}
