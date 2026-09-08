package main

import "testing"

var devPlan = Plan{
	ID: "gaussdb-dev", Name: "dev", Description: "dev",
	StorageGB: 5, TempGB: 1, SpillGB: 1, MaxConnections: 20,
}

func TestResolveInstanceParamsDefaults(t *testing.T) {
	spec, err := ResolveInstanceParams(devPlan, map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Compatibility != "PG" || spec.Encoding != "UTF8" || spec.MaxConnections != 20 || spec.StorageGB != 5 {
		t.Fatalf("defaults wrong: %+v", spec)
	}
}

func TestResolveInstanceParamsValidOverrides(t *testing.T) {
	spec, err := ResolveInstanceParams(devPlan, map[string]any{
		"compatibility": "A", "encoding": "GBK", "max_connections": 10, "storage_gb": 2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Compatibility != "A" || spec.Encoding != "GBK" || spec.MaxConnections != 10 || spec.StorageGB != 2 {
		t.Fatalf("overrides wrong: %+v", spec)
	}
}

func TestResolveInstanceParamsRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]any{
		"compatibility": {"compatibility": "ORACLE"},
		"encoding":      {"encoding": "UTF16"},
		"exceed":        {"storage_gb": 999},
		"fractional":    {"max_connections": 1.5},
	}
	for name, params := range cases {
		if _, err := ResolveInstanceParams(devPlan, params, nil); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolveInstanceParamsTablespaceAllowlist(t *testing.T) {
	if _, err := ResolveInstanceParams(devPlan, map[string]any{"tablespace": "ts_ssd"}, nil); err == nil {
		t.Error("tablespace without allowlist must be rejected")
	}
	if _, err := ResolveInstanceParams(devPlan, map[string]any{"tablespace": "nope"}, []string{"ts_ssd"}); err == nil {
		t.Error("tablespace outside allowlist must be rejected")
	}
	spec, err := ResolveInstanceParams(devPlan, map[string]any{"tablespace": "ts_ssd"}, []string{"ts_ssd"})
	if err != nil || spec.Tablespace != "ts_ssd" {
		t.Fatalf("allowlisted tablespace rejected: %v %+v", err, spec)
	}
}

func TestResolveBindingParams(t *testing.T) {
	spec, err := ResolveBindingParams(devPlan, map[string]any{})
	if err != nil || spec.MaxConnections != 20 {
		t.Fatalf("defaults wrong: %v %+v", err, spec)
	}
	spec, err = ResolveBindingParams(devPlan, map[string]any{"max_connections": 5})
	if err != nil || spec.MaxConnections != 5 {
		t.Fatalf("override wrong: %v %+v", err, spec)
	}
}
