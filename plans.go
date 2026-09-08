// plans.go loads the service catalog from the plans.toml data file and
// assembles the Open Service Broker catalog from it. The data file is
// deployment data; this file is the code that validates it and publishes it.

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
	StorageGB      int    `toml:"storage_gb"`
	MaxConnections int    `toml:"max_connections"`
	Free           *bool  `toml:"free"`
}

// ServiceConfig holds the service-level metadata from the plans file.
type ServiceConfig struct {
	ServiceID string `toml:"service_id"`
}

// CatalogData is the parsed contents of plans.toml.
type CatalogData struct {
	ServiceConfig
	Plans []Plan `toml:"plan"`
}

// LoadCatalog reads and validates the plans file.
func LoadCatalog(path string) (*CatalogData, error) {
	raw, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("plans file not readable: %w", err)
	}
	var data CatalogData
	if err := toml.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("plans file %s is not valid TOML: %w", path, err)
	}
	if data.ServiceID == "" {
		return nil, fmt.Errorf("plans file %s is missing service_id", path)
	}
	if len(data.Plans) == 0 {
		return nil, fmt.Errorf("plans file %s contains no [[plan]] entries", path)
	}
	seen := map[string]bool{}
	for i, plan := range data.Plans {
		switch {
		case plan.ID == "" || plan.Name == "" || plan.Description == "":
			return nil, fmt.Errorf("plan #%d in %s is missing id, name or description", i+1, path)
		case seen[plan.ID]:
			return nil, fmt.Errorf("duplicate plan id %q in %s", plan.ID, path)
		case plan.StorageGB < 1 || plan.MaxConnections < 1:
			return nil, fmt.Errorf("plan %q in %s: quota values must be positive integers", plan.ID, path)
		}
		seen[plan.ID] = true
	}
	return &data, nil
}

// Catalog assembles the /v2/catalog payload.
func Catalog(data *CatalogData) []domain.Service {
	plans := make([]domain.ServicePlan, 0, len(data.Plans))
	for _, plan := range data.Plans {
		free := plan.Free == nil || *plan.Free
		plans = append(plans, domain.ServicePlan{
			ID:          plan.ID,
			Name:        plan.Name,
			Description: plan.Description,
			Free:        &free,
			Metadata: &domain.ServicePlanMetadata{
				DisplayName: "GaussDB " + plan.Name,
				Bullets: []string{
					fmt.Sprintf("%d GB storage (tablespace MAXSIZE)", plan.StorageGB),
					fmt.Sprintf("up to %d concurrent connections", plan.MaxConnections),
				},
			},
			Schemas: &domain.ServiceSchemas{
				Instance: domain.ServiceInstanceSchema{
					Create: domain.Schema{Parameters: instanceSchema(plan)},
					Update: domain.Schema{Parameters: updatableSchema(plan)},
				},
				Binding: domain.ServiceBindingSchema{
					Create: domain.Schema{Parameters: bindingSchema(plan)},
				},
			},
		})
	}
	return []domain.Service{{
		ID:                   data.ServiceID,
		Name:                 "gaussdb",
		Description:          "openGauss/GaussDB logical databases as multi-tenant service instances.",
		Bindable:             true,
		InstancesRetrievable: true,
		BindingsRetrievable:  true,
		Tags:                 []string{"gaussdb", "opengauss", "postgresql", "database", "sql"},
		PlanUpdatable:        true,
		Plans:                plans,
		Metadata: &domain.ServiceMetadata{
			DisplayName:         "GaussDB (openGauss)",
			ProviderDisplayName: "openGauss",
			DocumentationUrl:    "https://docs.opengauss.org/",
			SupportUrl:          "https://opengauss.org/",
		},
	}}
}
