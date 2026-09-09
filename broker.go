// broker.go is the Open Service Broker layer: it maps brokerapi calls to
// Admin (SQL) and Store (memory) calls and decides which failure means which
// HTTP status. It contains no SQL and no storage logic.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"

	"code.cloudfoundry.org/brokerapi/v13/domain"
	"code.cloudfoundry.org/brokerapi/v13/domain/apiresponses"
)

// Broker implements the brokerapi ServiceBroker interface for openGauss.
type Broker struct {
	cfg     *Config
	catalog *CatalogData
	admin   *Admin
	store   *Store
	log     *slog.Logger
}

// NewBroker wires the broker to its configuration, plans, admin and store.
func NewBroker(cfg *Config, data *CatalogData, admin *Admin, store *Store, log *slog.Logger) *Broker {
	return &Broker{cfg: cfg, catalog: data, admin: admin, store: store, log: log}
}

// HealthCheck reports whether the database can be reached.
func (b *Broker) HealthCheck(ctx context.Context) error {
	return b.admin.HealthCheck(ctx)
}

func (b *Broker) plan(planID string) (Plan, error) {
	for _, plan := range b.catalog.Plans {
		if plan.ID == planID {
			return plan, nil
		}
	}
	return Plan{}, invalidInput(fmt.Sprintf("unknown plan_id %q", planID))
}

// Services returns the /v2/catalog payload.
func (b *Broker) Services(context.Context) ([]domain.Service, error) {
	return Catalog(b.catalog), nil
}

// Provision creates a logical database (tenant).
func (b *Broker) Provision(ctx context.Context, instanceID string, details domain.ProvisionDetails, _ bool) (domain.ProvisionedServiceSpec, error) {
	if details.ServiceID != b.catalog.ServiceID {
		return domain.ProvisionedServiceSpec{}, invalidInput(fmt.Sprintf("unknown service_id %q", details.ServiceID))
	}
	plan, err := b.plan(details.PlanID)
	if err != nil {
		return domain.ProvisionedServiceSpec{}, err
	}
	params, err := parseParams(instanceSchema(plan), details.RawParameters)
	if err != nil {
		return domain.ProvisionedServiceSpec{}, invalidInput(err.Error())
	}
	spec, err := ResolveInstanceParams(plan, params)
	if err != nil {
		return domain.ProvisionedServiceSpec{}, invalidInput(err.Error())
	}
	if existing := b.store.GetInstance(instanceID); existing != nil {
		if existing.PlanID == spec.PlanID && reflect.DeepEqual(existing.Params, spec) {
			return domain.ProvisionedServiceSpec{AlreadyExists: true}, nil
		}
		return domain.ProvisionedServiceSpec{}, apiresponses.ErrInstanceAlreadyExists
	}

	names := NamesFor(instanceID, b.cfg.NamePrefix, spec.Name)
	b.log.Info("provisioning logical database", "database", names.Database, "plan", plan.ID)
	if err := b.admin.Provision(ctx, names, spec); err != nil {
		return domain.ProvisionedServiceSpec{}, mapAdminError(err, apiresponses.ErrInstanceAlreadyExists)
	}
	if err := b.store.PutInstance(instanceID, InstanceRecord{
		ServiceID: b.catalog.ServiceID, PlanID: spec.PlanID, Database: names.Database, Params: spec,
	}); err != nil {
		return domain.ProvisionedServiceSpec{}, err
	}
	return domain.ProvisionedServiceSpec{}, nil
}

// Update changes the connection limit and quotas of an instance.
func (b *Broker) Update(ctx context.Context, instanceID string, details domain.UpdateDetails, _ bool) (domain.UpdateServiceSpec, error) {
	if details.ServiceID != b.catalog.ServiceID {
		return domain.UpdateServiceSpec{}, invalidInput(fmt.Sprintf("unknown service_id %q", details.ServiceID))
	}
	existing := b.store.GetInstance(instanceID)
	if existing == nil {
		return domain.UpdateServiceSpec{}, apiresponses.ErrInstanceNotFound
	}
	// Plan changes are advertised as unsupported in the catalog.
	previousPlan := existing.PlanID
	if details.PreviousValues.PlanID != "" {
		previousPlan = details.PreviousValues.PlanID
	}
	if details.PlanID != "" && details.PlanID != previousPlan {
		return domain.UpdateServiceSpec{}, apiresponses.ErrPlanChangeNotSupported
	}
	plan, err := b.plan(previousPlan)
	if err != nil {
		return domain.UpdateServiceSpec{}, err
	}
	params, err := parseParams(updatableSchema(plan), details.RawParameters)
	if err != nil {
		return domain.UpdateServiceSpec{}, invalidInput(err.Error())
	}
	spec, err := ResolveInstanceParams(plan, params)
	if err != nil {
		return domain.UpdateServiceSpec{}, invalidInput(err.Error())
	}

	names := NamesFor(instanceID, b.cfg.NamePrefix, spec.Name)
	b.log.Info("updating logical database", "database", names.Database)
	if err := b.admin.Update(ctx, names, spec); err != nil {
		return domain.UpdateServiceSpec{}, err
	}
	existing.Params = spec
	if err := b.store.PutInstance(instanceID, *existing); err != nil {
		return domain.UpdateServiceSpec{}, err
	}
	return domain.UpdateServiceSpec{}, nil
}

// Deprovision removes the whole tenant; bindings must be gone first.
func (b *Broker) Deprovision(ctx context.Context, instanceID string, details domain.DeprovisionDetails, _ bool) (domain.DeprovisionServiceSpec, error) {
	if details.ServiceID != b.catalog.ServiceID {
		return domain.DeprovisionServiceSpec{}, invalidInput(fmt.Sprintf("unknown service_id %q", details.ServiceID))
	}
	instance := b.store.GetInstance(instanceID)
	if instance == nil {
		return domain.DeprovisionServiceSpec{}, apiresponses.ErrInstanceNotFound
	}
	if len(b.store.BindingsForInstance(instanceID)) > 0 {
		return domain.DeprovisionServiceSpec{}, invalidInput(
			"service instance still has bindings; unbind them before deprovisioning")
	}

	names := NamesFor(instanceID, b.cfg.NamePrefix, instance.Params.Name)
	b.log.Info("deprovisioning logical database", "database", names.Database)
	if err := b.admin.Deprovision(ctx, names); err != nil {
		return domain.DeprovisionServiceSpec{}, err
	}
	if err := b.store.DeleteInstance(instanceID); err != nil {
		return domain.DeprovisionServiceSpec{}, err
	}
	return domain.DeprovisionServiceSpec{}, nil
}

// Bind creates a login user scoped to one logical database.
func (b *Broker) Bind(ctx context.Context, instanceID, bindingID string, details domain.BindDetails, _ bool) (domain.Binding, error) {
	if details.ServiceID != b.catalog.ServiceID {
		return domain.Binding{}, invalidInput(fmt.Sprintf("unknown service_id %q", details.ServiceID))
	}
	instance := b.store.GetInstance(instanceID)
	if instance == nil {
		return domain.Binding{}, apiresponses.ErrInstanceNotFound
	}
	plan, err := b.plan(instance.PlanID)
	if err != nil {
		return domain.Binding{}, err
	}
	params, err := parseParams(bindingSchema(plan), details.RawParameters)
	if err != nil {
		return domain.Binding{}, invalidInput(err.Error())
	}
	spec, err := ResolveBindingParams(plan, params)
	if err != nil {
		return domain.Binding{}, invalidInput(err.Error())
	}

	if existing := b.store.GetBinding(bindingID); existing != nil {
		if existing.InstanceID == instanceID && reflect.DeepEqual(existing.Params, spec) {
			return domain.Binding{AlreadyExists: true, Credentials: existing.Credentials}, nil
		}
		return domain.Binding{}, apiresponses.ErrBindingAlreadyExists
	}

	names := NamesFor(instanceID, b.cfg.NamePrefix, spec.Name)
	username := UserFor(bindingID, b.cfg.NamePrefix, spec.Name)
	b.log.Info("binding user", "user", username, "database", names.Database)
	password, err := b.admin.Bind(ctx, names, username)
	if err != nil {
		return domain.Binding{}, mapAdminError(err, apiresponses.ErrBindingAlreadyExists)
	}
	credentials := b.credentials(names.Database, username, password)
	if err := b.store.PutBinding(bindingID, username, instanceID, spec, credentials); err != nil {
		return domain.Binding{}, err
	}
	return domain.Binding{Credentials: credentials}, nil
}

// Unbind removes the binding user.
func (b *Broker) Unbind(ctx context.Context, instanceID, bindingID string, details domain.UnbindDetails, _ bool) (domain.UnbindSpec, error) {
	if details.ServiceID != b.catalog.ServiceID {
		return domain.UnbindSpec{}, invalidInput(fmt.Sprintf("unknown service_id %q", details.ServiceID))
	}
	instance := b.store.GetInstance(instanceID)
	binding := b.store.GetBinding(bindingID)
	if instance == nil || binding == nil || binding.InstanceID != instanceID {
		return domain.UnbindSpec{}, apiresponses.ErrBindingNotFound
	}

	names := NamesFor(instanceID, b.cfg.NamePrefix, instance.Params.Name)
	b.log.Info("unbinding user", "user", binding.Username, "database", names.Database)
	if err := b.admin.Unbind(ctx, names, binding.Username); err != nil {
		return domain.UnbindSpec{}, err
	}
	if err := b.store.DeleteBinding(bindingID); err != nil {
		return domain.UnbindSpec{}, err
	}
	return domain.UnbindSpec{}, nil
}

// GetInstance reports the stored parameters of an instance.
func (b *Broker) GetInstance(_ context.Context, instanceID string, _ domain.FetchInstanceDetails) (domain.GetInstanceDetailsSpec, error) {
	instance := b.store.GetInstance(instanceID)
	if instance == nil {
		return domain.GetInstanceDetailsSpec{}, apiresponses.ErrInstanceNotFound
	}
	return domain.GetInstanceDetailsSpec{
		ServiceID: instance.ServiceID, PlanID: instance.PlanID, Parameters: instance.Params,
	}, nil
}

// GetBinding reports the stored credentials and parameters of a binding.
func (b *Broker) GetBinding(_ context.Context, _, bindingID string, _ domain.FetchBindingDetails) (domain.GetBindingSpec, error) {
	binding := b.store.GetBinding(bindingID)
	if binding == nil {
		return domain.GetBindingSpec{}, apiresponses.ErrBindingNotFound
	}
	return domain.GetBindingSpec{Credentials: binding.Credentials, Parameters: binding.Params}, nil
}

// LastOperation always reports success: every broker operation is synchronous.
func (b *Broker) LastOperation(_ context.Context, instanceID string, _ domain.PollDetails) (domain.LastOperation, error) {
	if b.store.GetInstance(instanceID) == nil {
		return domain.LastOperation{}, apiresponses.ErrInstanceNotFound
	}
	return domain.LastOperation{State: domain.Succeeded, Description: "synchronous operation"}, nil
}

// LastBindingOperation always reports success for the same reason.
func (b *Broker) LastBindingOperation(_ context.Context, _, bindingID string, _ domain.PollDetails) (domain.LastOperation, error) {
	if b.store.GetBinding(bindingID) == nil {
		return domain.LastOperation{}, apiresponses.ErrBindingNotFound
	}
	return domain.LastOperation{State: domain.Succeeded, Description: "synchronous operation"}, nil
}

// credentials builds the credential payload handed to the bound application.
func (b *Broker) credentials(database, username, password string) map[string]string {
	c := b.cfg
	uri := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
		url.QueryEscape(username), url.QueryEscape(password), c.DBHost, c.DBPort, database)
	if c.DBSSLMode != "disable" {
		uri += "?sslmode=" + url.QueryEscape(c.DBSSLMode)
	}
	return map[string]string{
		"uri":      uri,
		"hostname": c.DBHost,
		"port":     fmt.Sprintf("%d", c.DBPort),
		"database": database,
		"username": username,
		"password": password,
		"sslmode":  c.DBSSLMode,
		"jdbcUrl":  fmt.Sprintf("jdbc:postgresql://%s:%d/%s", c.DBHost, c.DBPort, database),
	}
}

// mapAdminError converts an Admin AlreadyExistsError into the matching
// brokerapi conflict response; any other error passes through unchanged.
func mapAdminError(err error, conflict error) error {
	var exists AlreadyExistsError
	if errors.As(err, &exists) {
		return conflict
	}
	return err
}

// parseParams validates raw JSON against a schema, then decodes it.
func parseParams(schema map[string]any, raw json.RawMessage) (map[string]any, error) {
	if err := ValidateParameters(schema, raw); err != nil {
		return nil, err
	}
	return RawParameters(raw)
}

// invalidInput wraps a validation message as an HTTP 400 response.
func invalidInput(message string) error {
	return apiresponses.NewFailureResponse(errors.New(message), 400, "validate-parameters")
}
