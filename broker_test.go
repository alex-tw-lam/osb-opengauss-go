package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"

	"code.cloudfoundry.org/brokerapi/v13/domain"
)

const bid = "22222222-2222-2222-2222-222222222222"

// newTestBroker builds a broker on a fake database and a real temp state file.
func newTestBroker(t *testing.T, mode string) (*Broker, *fakeDB) {
	t.Helper()
	cfg := testConfig(mode)
	cfg.StatePath = filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := newFakeDB()
	plans := []Plan{devPlan, {ID: "gaussdb-pro", Name: "pro", Description: "pro",
		StorageGB: 200, TempGB: 40, SpillGB: 40, MaxConnections: 500}}
	return NewBroker(cfg, plans, NewAdmin(cfg, db), store, slog.Default()), db
}

func provisionDetails(params map[string]any) domain.ProvisionDetails {
	raw, _ := json.Marshal(params)
	return domain.ProvisionDetails{ServiceID: serviceID, PlanID: "gaussdb-dev", RawParameters: raw}
}

func bindDetails(params map[string]any) domain.BindDetails {
	raw, _ := json.Marshal(params)
	return domain.BindDetails{ServiceID: serviceID, PlanID: "gaussdb-dev", RawParameters: raw}
}

func TestProvisionLifecycle(t *testing.T) {
	broker, db := newTestBroker(t, "role_quota")
	ctx := context.Background()

	spec, err := broker.Provision(ctx, iid, provisionDetails(nil), false)
	if err != nil || spec.AlreadyExists {
		t.Fatalf("provision failed: %v %+v", err, spec)
	}
	if len(db.statements["postgres"]) == 0 {
		t.Fatal("no SQL executed")
	}

	// Identical repeat is an idempotent success.
	spec, err = broker.Provision(ctx, iid, provisionDetails(nil), false)
	if err != nil || !spec.AlreadyExists {
		t.Fatalf("identical re-provision must report AlreadyExists: %v %+v", err, spec)
	}

	// Conflicting repeat is a conflict.
	_, err = broker.Provision(ctx, iid, provisionDetails(map[string]any{"max_connections": 5}), false)
	if err == nil {
		t.Fatal("conflicting re-provision must fail")
	}

	// Unknown plan and invalid parameters are rejected.
	if _, err := broker.Provision(ctx, "other", domain.ProvisionDetails{ServiceID: serviceID, PlanID: "nope"}, false); err == nil {
		t.Error("unknown plan must be rejected")
	}
	if _, err := broker.Provision(ctx, "other", provisionDetails(map[string]any{"compatibility": "X"}), false); err == nil {
		t.Error("invalid parameter must be rejected")
	}
}

func TestBindUnbindDeprovision(t *testing.T) {
	broker, _ := newTestBroker(t, "role_quota")
	ctx := context.Background()
	if _, err := broker.Provision(ctx, iid, provisionDetails(nil), false); err != nil {
		t.Fatal(err)
	}

	binding, err := broker.Bind(ctx, iid, bid, bindDetails(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	credentials := binding.Credentials.(map[string]string)
	if credentials["database"] != names.Database || credentials["username"] != UserFor(bid, "gdb") {
		t.Fatalf("credentials wrong: %v", credentials)
	}

	// Identical repeat returns the same credentials.
	repeat, err := broker.Bind(ctx, iid, bid, bindDetails(nil), false)
	if err != nil || !repeat.AlreadyExists {
		t.Fatalf("identical re-bind must report AlreadyExists: %v", err)
	}
	if !reflect.DeepEqual(repeat.Credentials, credentials) {
		t.Fatal("identical re-bind must return the same credentials")
	}
	// Conflicting repeat fails.
	if _, err := broker.Bind(ctx, iid, bid, bindDetails(map[string]any{"access_role": "readonly"}), false); err == nil {
		t.Fatal("conflicting re-bind must fail")
	}

	// Deprovision is blocked while a binding exists.
	if _, err := broker.Deprovision(ctx, iid, domain.DeprovisionDetails{ServiceID: serviceID, PlanID: "gaussdb-dev"}, false); err == nil {
		t.Fatal("deprovision with bindings must fail")
	}

	if _, err := broker.Unbind(ctx, iid, bid, domain.UnbindDetails{ServiceID: serviceID, PlanID: "gaussdb-dev"}, false); err != nil {
		t.Fatal(err)
	}
	// Unknown unbind reports gone.
	if _, err := broker.Unbind(ctx, iid, bid, domain.UnbindDetails{ServiceID: serviceID, PlanID: "gaussdb-dev"}, false); err == nil {
		t.Fatal("second unbind must fail")
	}

	if _, err := broker.Deprovision(ctx, iid, domain.DeprovisionDetails{ServiceID: serviceID, PlanID: "gaussdb-dev"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Deprovision(ctx, iid, domain.DeprovisionDetails{ServiceID: serviceID, PlanID: "gaussdb-dev"}, false); err == nil {
		t.Fatal("second deprovision must fail")
	}
}

func TestUpdateAndRetrieval(t *testing.T) {
	broker, _ := newTestBroker(t, "role_quota")
	ctx := context.Background()
	if _, err := broker.Provision(ctx, iid, provisionDetails(nil), false); err != nil {
		t.Fatal(err)
	}

	update := domain.UpdateDetails{
		ServiceID: serviceID, PlanID: "gaussdb-dev",
		RawParameters:  mustJSON(map[string]any{"max_connections": 10}),
		PreviousValues: domain.PreviousValues{PlanID: "gaussdb-dev"},
	}
	if _, err := broker.Update(ctx, iid, update, false); err != nil {
		t.Fatal(err)
	}
	instance, err := broker.GetInstance(ctx, iid, domain.FetchInstanceDetails{})
	if err != nil {
		t.Fatal(err)
	}
	params := instance.Parameters.(InstanceParams)
	if params.MaxConnections != 10 {
		t.Fatalf("update not stored: %+v", params)
	}

	planChange := domain.UpdateDetails{ServiceID: serviceID, PlanID: "gaussdb-pro",
		PreviousValues: domain.PreviousValues{PlanID: "gaussdb-dev"}}
	if _, err := broker.Update(ctx, iid, planChange, false); err == nil {
		t.Fatal("plan change must be rejected")
	}
	if _, err := broker.Update(ctx, "missing", update, false); err == nil {
		t.Fatal("update of unknown instance must fail")
	}

	_, err = broker.Bind(ctx, iid, bid, bindDetails(map[string]any{"access_role": "readonly"}), false)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := broker.GetBinding(ctx, iid, bid, domain.FetchBindingDetails{})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Parameters.(BindingParams).AccessRole != "readonly" {
		t.Fatalf("binding retrieval wrong: %+v", fetched.Parameters)
	}
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
