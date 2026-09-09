package main

import "testing"

var devPlan = Plan{
	ID: "bbbb1111-2222-3333-4444-555555555555", Name: "dev", Description: "dev",
	StorageGB: 5, MaxConnections: 20,
}

func TestResolveInstanceParamsDefaults(t *testing.T) {
	spec, err := ResolveInstanceParams(devPlan, map[string]any{})
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
	})
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
		if _, err := ResolveInstanceParams(devPlan, params); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolveBindingParams(t *testing.T) {
	spec, err := ResolveBindingParams(devPlan, map[string]any{})
	if err != nil || spec.Name != "" {
		t.Fatalf("defaults wrong: %v %+v", err, spec)
	}
	spec, err = ResolveBindingParams(devPlan, map[string]any{"name": "reporting"})
	if err != nil || spec.Name != "reporting" {
		t.Fatalf("override wrong: %v %+v", err, spec)
	}
}
