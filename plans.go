// plans.go loads the service catalog from the plans.toml data file and
// assembles the Open Service Broker catalog from it. Every user-visible
// field is configurable so multiple broker deployments can offer
// differently-named services without collisions.

package main

import (
	"fmt"
	"os"
	"regexp"

	"code.cloudfoundry.org/brokerapi/v13/domain"
	"github.com/BurntSushi/toml"
)

// ServiceConfig holds the service-level metadata from the plans file.
type ServiceConfig struct {
	ServiceID   string   `toml:"service_id"`
	Name        string   `toml:"name"`
	DisplayName string   `toml:"display_name"`
	Description string   `toml:"description"`
	Provider    string   `toml:"provider"`
	DocsURL     string   `toml:"docs_url"`
	SupportURL  string   `toml:"support_url"`
	Tags        []string `toml:"tags"`
}

// Plan is one quota bundle.
type Plan struct {
	ID             string `toml:"id"`
	Name           string `toml:"name"`
	DisplayName    string `toml:"display_name"`
	Description    string `toml:"description"`
	StorageGB      int    `toml:"storage_gb"`
	MaxConnections int    `toml:"max_connections"`
	Free           *bool  `toml:"free"`
}

// CatalogData is the parsed contents of plans.toml.
type CatalogData struct {
	ServiceConfig
	Plans []Plan `toml:"plan"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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
	if err := validateService(&data.ServiceConfig, path); err != nil {
		return nil, err
	}
	if err := validatePlans(data.Plans, path); err != nil {
		return nil, err
	}
	return &data, nil
}

func validateService(svc *ServiceConfig, path string) error {
	switch {
	case svc.ServiceID == "":
		return fmt.Errorf("plans file %s is missing service_id", path)
	case !uuidPattern.MatchString(svc.ServiceID):
		return fmt.Errorf("service_id %q is not a valid UUID", svc.ServiceID)
	case svc.Name == "":
		return fmt.Errorf("plans file %s is missing service name", path)
	case svc.Description == "":
		return fmt.Errorf("plans file %s is missing service description", path)
	}
	return nil
}

func validatePlans(plans []Plan, path string) error {
	if len(plans) == 0 {
		return fmt.Errorf("plans file %s contains no [[plan]] entries", path)
	}
	seenIDs := map[string]bool{}
	seenNames := map[string]bool{}
	for i, plan := range plans {
		switch {
		case plan.ID == "" || plan.Name == "" || plan.Description == "":
			return fmt.Errorf("plan #%d in %s is missing id, name or description", i+1, path)
		case !uuidPattern.MatchString(plan.ID):
			return fmt.Errorf("plan %q id %q is not a valid UUID", plan.Name, plan.ID)
		case seenIDs[plan.ID]:
			return fmt.Errorf("duplicate plan id %q in %s", plan.ID, path)
		case seenNames[plan.Name]:
			return fmt.Errorf("duplicate plan name %q in %s", plan.Name, path)
		case plan.StorageGB < 1 || plan.MaxConnections < 1:
			return fmt.Errorf("plan %q in %s: quota values must be positive integers", plan.Name, path)
		}
		seenIDs[plan.ID] = true
		seenNames[plan.Name] = true
	}
	return nil
}

// Catalog assembles the /v2/catalog payload.
func Catalog(data *CatalogData) []domain.Service {
	plans := make([]domain.ServicePlan, 0, len(data.Plans))
	for _, plan := range data.Plans {
		free := plan.Free == nil || *plan.Free
		displayName := plan.DisplayName
		if displayName == "" {
			displayName = plan.Name
		}
		plans = append(plans, domain.ServicePlan{
			ID:          plan.ID,
			Name:        plan.Name,
			Description: plan.Description,
			Free:        &free,
			Metadata: &domain.ServicePlanMetadata{
				DisplayName: displayName,
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
	displayName := data.DisplayName
	if displayName == "" {
		displayName = data.Name
	}
	return []domain.Service{{
		ID:                   data.ServiceID,
		Name:                 data.Name,
		Description:          data.Description,
		Bindable:             true,
		InstancesRetrievable: true,
		BindingsRetrievable:  true,
		Tags:                 data.Tags,
		PlanUpdatable:        true,
		Plans:                plans,
		Metadata: &domain.ServiceMetadata{
			DisplayName:         displayName,
			ProviderDisplayName: data.Provider,
			DocumentationUrl:    data.DocsURL,
			SupportUrl:          data.SupportURL,
		},
	}}
}
