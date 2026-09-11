package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	cfg := testConfig()
	cfg.StatePath = filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(cfg, NoopEncryptor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestInstanceStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	record := InstanceRecord{
		ServiceID: "svc-1", PlanID: "plan-1", Database: "gdb_x",
		Params: InstanceParams{Compatibility: "A", MaxConnections: 10},
	}
	if err := store.PutInstance("i1", record); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetInstance("i1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Database != "gdb_x" || got.Params.Compatibility != "A" || got.Params.MaxConnections != 10 {
		t.Fatalf("round trip wrong: %+v", got)
	}
	record.Params.MaxConnections = 5
	_ = store.PutInstance("i1", record)
	got, _ = store.GetInstance("i1")
	if got.Params.MaxConnections != 5 {
		t.Fatalf("update not applied: %+v", got)
	}
	if err := store.DeleteInstance("i1"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetInstance("i1")
	if got != nil {
		t.Fatal("deleted instance still found")
	}
}

func TestBindingStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	creds := map[string]string{"uri": "gaussdb://x", "password": "p"}
	if err := store.PutBinding("b1", "gdbu_b", "i1", BindingParams{Name: "test"}, creds); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetBinding("b1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Params.Name != "test" || got.Credentials["uri"] != "gaussdb://x" {
		t.Fatalf("round trip wrong: %+v", got)
	}
	list, err := store.BindingsForInstance("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Username != "gdbu_b" {
		t.Fatalf("BindingsForInstance wrong: %+v", list)
	}
	if list, _ := store.BindingsForInstance("other"); len(list) != 0 {
		t.Fatalf("expected no bindings, got %+v", list)
	}
	_ = store.DeleteBinding("b1")
	if got, _ := store.GetBinding("b1"); got != nil {
		t.Fatal("deleted binding still found")
	}
}

func TestBindingCredentialsEncrypted(t *testing.T) {
	cfg := testConfig()
	cfg.StatePath = filepath.Join(t.TempDir(), "enc.db")
	key := [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}
	store, err := OpenStore(cfg, &GCMEncryptor{key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	creds := map[string]string{"password": "super-secret"}
	if err := store.PutBinding("b1", "u1", "i1", BindingParams{}, creds); err != nil {
		t.Fatal(err)
	}

	var raw string
	if err := store.db.Raw("SELECT credentials FROM binding_records WHERE binding_id = 'b1'").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "super-secret") {
		t.Fatalf("credentials stored in plaintext: %q", raw)
	}

	got, err := store.GetBinding("b1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Credentials["password"] != "super-secret" {
		t.Fatalf("decrypt round trip failed: %+v", got)
	}
}

// A key rotation must surface as an error, never as a panic or a missing binding.
func TestUndecryptableBindingFailsClosed(t *testing.T) {
	cfg := testConfig()
	cfg.StatePath = filepath.Join(t.TempDir(), "rotated.db")
	first := [32]byte{1}
	second := [32]byte{2}
	store, err := OpenStore(cfg, &GCMEncryptor{key: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutBinding("b1", "u1", "i1", BindingParams{}, map[string]string{"password": "p"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = OpenStore(cfg, &GCMEncryptor{key: second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.GetBinding("b1"); err == nil {
		t.Fatal("reading with the wrong key must fail")
	}
	if _, err := store.BindingsForInstance("i1"); err == nil {
		t.Fatal("listing with the wrong key must fail")
	}
}

// Rotating to a new key with the old key as previous re-encrypts old records,
// leaves new-key records alone, and fails closed on records neither key reads.
func TestRotateBindings(t *testing.T) {
	cfg := testConfig()
	cfg.StatePath = filepath.Join(t.TempDir(), "rotate.db")
	first := [32]byte{1}
	second := [32]byte{2}
	store, err := OpenStore(cfg, &GCMEncryptor{key: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutBinding("old", "u1", "i1", BindingParams{}, map[string]string{"password": "p1"}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = OpenStore(cfg, &GCMEncryptor{key: second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.PutBinding("new", "u2", "i1", BindingParams{}, map[string]string{"password": "p2"}); err != nil {
		t.Fatal(err)
	}

	rotated, err := store.RotateBindings(&GCMEncryptor{key: first})
	if err != nil {
		t.Fatal(err)
	}
	if rotated != 1 {
		t.Fatalf("expected 1 rotated record, got %d", rotated)
	}
	for id, want := range map[string]string{"old": "p1", "new": "p2"} {
		got, err := store.GetBinding(id)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Credentials["password"] != want {
			t.Fatalf("binding %s unreadable after rotation: %+v", id, got)
		}
	}

	// Rotating again must be a no-op now that every record is on the new key.
	if rotated, err := store.RotateBindings(&GCMEncryptor{key: first}); err != nil || rotated != 0 {
		t.Fatalf("second rotation expected 0 records, no error; got %d, %v", rotated, err)
	}

	// A record encrypted with an unknown key must stop the rotation.
	if err := store.db.Exec("INSERT INTO binding_records (binding_id, instance_id, username, params, credentials) VALUES ('alien', 'i1', 'u3', '{}', 'bm9wZQ==')").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := store.RotateBindings(&GCMEncryptor{key: first}); err == nil {
		t.Fatal("rotation must fail on a record neither key can read")
	}
}

func TestStateDSNValidation(t *testing.T) {
	cfg := testConfig()
	cfg.StateDSN = "mysql://nope"
	_, err := OpenStore(cfg, NoopEncryptor{})
	if err == nil || !strings.Contains(err.Error(), "STATE_DSN") {
		t.Fatalf("expected STATE_DSN error, got %v", err)
	}
}
