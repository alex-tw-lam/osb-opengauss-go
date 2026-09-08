package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlans(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plans.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validPlans = `
[[plan]]
id = "p1"
name = "one"
description = "first"
storage_gb = 5
temp_gb = 1
spill_gb = 1
max_connections = 20

[[plan]]
id = "p2"
name = "two"
description = "second"
storage_gb = 50
temp_gb = 10
spill_gb = 10
max_connections = 100
free = false
`

func TestLoadPlansValid(t *testing.T) {
	plans, err := LoadPlans(writePlans(t, validPlans))
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].ID != "p1" || plans[0].StorageGB != 5 {
		t.Fatalf("unexpected plans: %+v", plans)
	}
	if plans[0].Free != nil || plans[1].Free == nil || *plans[1].Free {
		t.Errorf("free default/override wrong: %+v %+v", plans[0].Free, plans[1].Free)
	}
}

func TestLoadPlansMissing(t *testing.T) {
	if _, err := LoadPlans(filepath.Join(t.TempDir(), "nope.toml")); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("expected not-readable error, got %v", err)
	}
}

func TestLoadPlansEmpty(t *testing.T) {
	_, err := LoadPlans(writePlans(t, "# nothing\n"))
	if err == nil || !strings.Contains(err.Error(), "no [[plan]] entries") {
		t.Fatalf("expected empty error, got %v", err)
	}
}

func TestLoadPlansDuplicateID(t *testing.T) {
	_, err := LoadPlans(writePlans(t, strings.Replace(validPlans, `id = "p2"`, `id = "p1"`, 1)))
	if err == nil || !strings.Contains(err.Error(), "duplicate plan id") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestLoadPlansNonPositiveQuota(t *testing.T) {
	_, err := LoadPlans(writePlans(t, strings.Replace(validPlans, "storage_gb = 5", "storage_gb = 0", 1)))
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("expected quota error, got %v", err)
	}
}

func TestCatalogExposesSchemas(t *testing.T) {
	plans, _ := LoadPlans(writePlans(t, validPlans))
	services := Catalog(plans)
	if len(services) != 1 || len(services[0].Plans) != 2 {
		t.Fatalf("unexpected catalog: %+v", services)
	}
	if !services[0].PlanUpdatable {
		t.Error("plan_updateable must be true")
	}
	properties := services[0].Plans[0].Schemas.Instance.Create.Parameters["properties"].(map[string]any)
	if properties["name"] == nil {
		t.Error("name property missing from instance schema")
	}
	if _, ok := properties["tablespace"]; ok {
		t.Error("tablespace property must not exist")
	}
}

func TestCatalogHidesTablespaceWithoutAllowlist(t *testing.T) {
	plans, _ := LoadPlans(writePlans(t, validPlans))
	services := Catalog(plans)
	properties := services[0].Plans[0].Schemas.Instance.Create.Parameters["properties"].(map[string]any)
	if _, ok := properties["tablespace"]; ok {
		t.Error("tablespace property must be hidden without an allowlist")
	}
}
