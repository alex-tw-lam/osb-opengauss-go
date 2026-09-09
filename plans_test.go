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
service_id = "aaaa1111-2222-3333-4444-555555555555"
name = "gaussdb"
description = "test service"

[[plan]]
id = "bbbb1111-2222-3333-4444-555555555555"
name = "one"
description = "first"
storage_gb = 5
max_connections = 20

[[plan]]
id = "cccc1111-2222-3333-4444-555555555555"
name = "two"
description = "second"
storage_gb = 50
max_connections = 100
free = false
`

func TestLoadPlansValid(t *testing.T) {
	data, err := LoadCatalog(writePlans(t, validPlans))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Plans) != 2 || data.Plans[0].ID != "bbbb1111-2222-3333-4444-555555555555" || data.Plans[0].StorageGB != 5 {
		t.Fatalf("unexpected data.Plans: %+v", data.Plans)
	}
	if data.Plans[0].Free != nil || data.Plans[1].Free == nil || *data.Plans[1].Free {
		t.Errorf("free default/override wrong: %+v %+v", data.Plans[0].Free, data.Plans[1].Free)
	}
}

func TestLoadPlansMissing(t *testing.T) {
	if _, err := LoadCatalog(filepath.Join(t.TempDir(), "nope.toml")); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("expected not-readable error, got %v", err)
	}
}

func TestLoadPlansEmpty(t *testing.T) {
	_, err := LoadCatalog(writePlans(t, "service_id = \"aaaa1111-2222-3333-4444-555555555555\"\nname = \"gaussdb\"\ndescription = \"test\"\n"))
	if err == nil || !strings.Contains(err.Error(), "no [[plan]] entries") {
		t.Fatalf("expected empty error, got %v", err)
	}
}

func TestLoadPlansDuplicateID(t *testing.T) {
	_, err := LoadCatalog(writePlans(t, strings.Replace(validPlans, `id = "cccc1111-2222-3333-4444-555555555555"`, `id = "bbbb1111-2222-3333-4444-555555555555"`, 1)))
	if err == nil || !strings.Contains(err.Error(), "duplicate plan id") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestLoadPlansNonPositiveQuota(t *testing.T) {
	_, err := LoadCatalog(writePlans(t, strings.Replace(validPlans, "storage_gb = 5", "storage_gb = 0", 1)))
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("expected quota error, got %v", err)
	}
}

func TestCatalogExposesSchemas(t *testing.T) {
	data, _ := LoadCatalog(writePlans(t, validPlans))
	services := Catalog(data)
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
