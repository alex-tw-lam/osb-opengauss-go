package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	cfg := testConfig("role_quota")
	cfg.StatePath = filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestInstanceStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	record := InstanceRecord{
		ServiceID: serviceID, PlanID: "gaussdb-dev", Database: "gdb_x",
		Params: InstanceParams{PlanID: "gaussdb-dev", Compatibility: "A", MaxConnections: 10},
	}
	if err := store.PutInstance("i1", record); err != nil {
		t.Fatal(err)
	}
	got := store.GetInstance("i1")
	if got == nil || got.Database != "gdb_x" || got.Params.Compatibility != "A" || got.Params.MaxConnections != 10 {
		t.Fatalf("round trip wrong: %+v", got)
	}
	// An update over the same key is an upsert.
	record.Params.MaxConnections = 5
	_ = store.PutInstance("i1", record)
	if got := store.GetInstance("i1"); got.Params.MaxConnections != 5 {
		t.Fatalf("update not applied: %+v", got)
	}
	if err := store.DeleteInstance("i1"); err != nil {
		t.Fatal(err)
	}
	if store.GetInstance("i1") != nil {
		t.Fatal("deleted instance still found")
	}
}

func TestBindingStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	record := BindingRecord{
		InstanceID: "i1", Username: "gdbu_b",
		Params:      BindingParams{MaxConnections: 7},
		Credentials: map[string]string{"uri": "gaussdb://x", "password": "p"},
	}
	if err := store.PutBinding("b1", record); err != nil {
		t.Fatal(err)
	}
	got := store.GetBinding("b1")
	if got == nil || got.Params.MaxConnections != 7 || got.Credentials["uri"] != "gaussdb://x" {
		t.Fatalf("round trip wrong: %+v", got)
	}
	if list := store.BindingsForInstance("i1"); len(list) != 1 || list[0].Username != "gdbu_b" {
		t.Fatalf("BindingsForInstance wrong: %+v", list)
	}
	if list := store.BindingsForInstance("other"); len(list) != 0 {
		t.Fatalf("expected no bindings, got %+v", list)
	}
	_ = store.DeleteBinding("b1")
	if store.GetBinding("b1") != nil {
		t.Fatal("deleted binding still found")
	}
}

func TestStateDSNValidation(t *testing.T) {
	cfg := testConfig("role_quota")
	cfg.StateDSN = "mysql://nope"
	_, err := OpenStore(cfg)
	if err == nil || !strings.Contains(err.Error(), "STATE_DSN") {
		t.Fatalf("expected STATE_DSN error, got %v", err)
	}
}
