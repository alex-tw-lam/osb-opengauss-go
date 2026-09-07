// plans.go loads the plan catalog from the plans.toml data file and assembles
// the Open Service Broker catalog from it. The data file is deployment data;
// this file is the code that validates it and publishes it.

package main

import (
	"fmt"
	"os"

	"code.cloudfoundry.org/brokerapi/v13/domain"
	"github.com/BurntSushi/toml"
)

// Plan is one quota bundle; the only difference between plans are these numbers.
type Plan struct {
	ID             string `toml:"id"`
	Name           string `toml:"name"`
	Description    string `toml:"description"`
	StorageGB      int    `toml:"storage_gb"` // PERM SPACE / tablespace MAXSIZE
	TempGB         int    `toml:"temp_gb"`    // TEMP SPACE
	SpillGB        int    `toml:"spill_gb"`   // SPILL SPACE
	MaxConnections int    `toml:"max_connections"`
	Free           *bool  `toml:"free"` // defaults to true when omitted
}

const serviceID = "4c6f6a1e-0f5a-4a5b-9d7e-2f8b3a1c5e01"

// LoadPlans reads and validates the plans file; any problem is an error so a
// broken data file can never produce a half-usable catalog.
func LoadPlans(path string) ([]Plan, error) {
	raw, err := os.ReadFile(path) // #nosec G304 // the path is operator configuration (GAUSSDB_PLANS_FILE), not request input
	if err != nil {
		return nil, fmt.Errorf("plans file not readable: %w", err)
	}
	var file struct {
		Plan []Plan `toml:"plan"`
	}
	if err := toml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("plans file %s is not valid TOML: %w", path, err)
	}
	if len(file.Plan) == 0 {
		return nil, fmt.Errorf("plans file %s contains no [[plan]] entries", path)
	}
	seen := map[string]bool{}
	for i, plan := range file.Plan {
		switch {
		case plan.ID == "" || plan.Name == "" || plan.Description == "":
			return nil, fmt.Errorf("plan #%d in %s is missing id, name or description", i+1, path)
		case seen[plan.ID]:
			return nil, fmt.Errorf("duplicate plan id %q in %s", plan.ID, path)
		case plan.StorageGB < 1 || plan.TempGB < 1 || plan.SpillGB < 1 || plan.MaxConnections < 1:
			return nil, fmt.Errorf("plan %q in %s: quota values must be positive integers", plan.ID, path)
		}
		seen[plan.ID] = true
	}
	return file.Plan, nil
}

// Catalog assembles the /v2/catalog payload for the loaded plans.
func Catalog(plans []Plan, tablespaces []string) []domain.Service {
	servicePlans := make([]domain.ServicePlan, 0, len(plans))
	for _, plan := range plans {
		free := true
		if plan.Free != nil {
			free = *plan.Free
		}
		servicePlans = append(servicePlans, domain.ServicePlan{
			ID:          plan.ID,
			Name:        plan.Name,
			Description: plan.Description,
			Free:        &free,
			Metadata: &domain.ServicePlanMetadata{
				DisplayName: "GaussDB " + plan.Name,
				Bullets: []string{
					fmt.Sprintf("%d GB storage quota", plan.StorageGB),
					fmt.Sprintf("%d GB temp / %d GB spill quota", plan.TempGB, plan.SpillGB),
					fmt.Sprintf("up to %d concurrent connections", plan.MaxConnections),
				},
			},
			Schemas: &domain.ServiceSchemas{
				Instance: domain.ServiceInstanceSchema{
					Create: domain.Schema{Parameters: instanceSchema(plan, tablespaces)},
					Update: domain.Schema{Parameters: updatableSchema(plan)},
				},
				Binding: domain.ServiceBindingSchema{
					Create: domain.Schema{Parameters: bindingSchema(plan)},
				},
			},
		})
	}
	return []domain.Service{{
		ID:                   serviceID,
		Name:                 "gaussdb",
		Description:          "openGauss/GaussDB logical databases as multi-tenant service instances. Each instance is an isolated logical database; bindings are user accounts scoped to that database.",
		Bindable:             true,
		InstancesRetrievable: true,
		BindingsRetrievable:  true,
		Tags:                 []string{"gaussdb", "opengauss", "postgresql", "database", "sql"},
		PlanUpdatable:        false,
		Plans:                servicePlans,
		Metadata: &domain.ServiceMetadata{
			DisplayName:         "GaussDB (openGauss)",
			LongDescription:     "Provisions logical databases (tenants) on a shared openGauss instance. Isolation: per-database connection separation, PRIVATE OBJECT filtering and per-user space quotas.",
			ProviderDisplayName: "openGauss",
			DocumentationUrl:    "https://docs.opengauss.org/",
			SupportUrl:          "https://opengauss.org/",
		},
	}}
}
