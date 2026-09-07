// params.go decides what a valid request looks like: it merges user-supplied
// parameters over a plan's defaults and rejects anything that would exceed
// the plan. The JSON schemas published in the catalog state the same rules.

package main

import (
	"encoding/json"
	"fmt"
)

var (
	compatibilities = []string{"PG", "A", "B", "C"} // openGauss DBCOMPATIBILITY
	encodings       = []string{"UTF8", "GBK", "GB18030", "Latin1"}
	accessRoles     = []string{"owner", "readwrite", "readonly"}
)

// InstanceParams is the fully resolved parameter set of one logical database.
type InstanceParams struct {
	PlanID         string `json:"plan_id"`
	Compatibility  string `json:"compatibility"`
	Encoding       string `json:"encoding"`
	Tablespace     string `json:"tablespace"`
	MaxConnections int    `json:"max_connections"`
	StorageGB      int    `json:"storage_gb"`
	TempGB         int    `json:"temp_gb"`
	SpillGB        int    `json:"spill_gb"`
}

// BindingParams is the fully resolved parameter set of one binding user.
type BindingParams struct {
	AccessRole     string `json:"access_role"`
	MaxConnections int    `json:"max_connections"`
}

// RawParameters converts the raw JSON parameters of a request into a map.
func RawParameters(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	params := map[string]any{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("parameters are not a valid JSON object: %w", err)
	}
	return params, nil
}

// ResolveInstanceParams merges user parameters over the plan defaults.
// Parameters may tighten a plan but never exceed it.
func ResolveInstanceParams(plan Plan, params map[string]any, allowedTablespaces []string) (InstanceParams, error) {
	resolved := InstanceParams{
		PlanID:         plan.ID,
		Compatibility:  "PG",
		Encoding:       "UTF8",
		MaxConnections: plan.MaxConnections,
		StorageGB:      plan.StorageGB,
		TempGB:         plan.TempGB,
		SpillGB:        plan.SpillGB,
	}
	if v, ok := params["compatibility"]; ok {
		resolved.Compatibility = fmt.Sprint(v)
		if !contains(compatibilities, resolved.Compatibility) {
			return resolved, fmt.Errorf("'compatibility' must be one of %v", compatibilities)
		}
	}
	if v, ok := params["encoding"]; ok {
		resolved.Encoding = fmt.Sprint(v)
		if !contains(encodings, resolved.Encoding) {
			return resolved, fmt.Errorf("'encoding' must be one of %v", encodings)
		}
	}
	if v, ok := params["tablespace"]; ok {
		resolved.Tablespace = fmt.Sprint(v)
		switch {
		case len(allowedTablespaces) == 0:
			return resolved, fmt.Errorf("'tablespace' is not offered by this deployment")
		case !contains(allowedTablespaces, resolved.Tablespace):
			return resolved, fmt.Errorf("'tablespace' must be one of %v", allowedTablespaces)
		}
	}
	var err error
	if resolved.MaxConnections, err = boundedInt(params, "max_connections", plan.MaxConnections, plan.MaxConnections); err != nil {
		return resolved, err
	}
	if resolved.StorageGB, err = boundedInt(params, "storage_gb", plan.StorageGB, plan.StorageGB); err != nil {
		return resolved, err
	}
	if resolved.TempGB, err = boundedInt(params, "temp_gb", plan.TempGB, plan.TempGB); err != nil {
		return resolved, err
	}
	if resolved.SpillGB, err = boundedInt(params, "spill_gb", plan.SpillGB, plan.SpillGB); err != nil {
		return resolved, err
	}
	return resolved, nil
}

// ResolveBindingParams merges user parameters over the plan defaults.
func ResolveBindingParams(plan Plan, params map[string]any) (BindingParams, error) {
	resolved := BindingParams{AccessRole: "readwrite", MaxConnections: plan.MaxConnections}
	if v, ok := params["access_role"]; ok {
		resolved.AccessRole = fmt.Sprint(v)
		if !contains(accessRoles, resolved.AccessRole) {
			return resolved, fmt.Errorf("'access_role' must be one of %v", accessRoles)
		}
	}
	var err error
	if resolved.MaxConnections, err = boundedInt(params, "max_connections", plan.MaxConnections, plan.MaxConnections); err != nil {
		return resolved, err
	}
	return resolved, nil
}

// boundedInt reads key from params as an integer between 1 and maximum.
func boundedInt(params map[string]any, key string, fallback, maximum int) (int, error) {
	raw, ok := params[key]
	if !ok {
		return fallback, nil
	}
	// JSON numbers decode as float64; whole numbers of any numeric type are fine.
	switch v := raw.(type) {
	case int:
		if v >= 1 && v <= maximum {
			return v, nil
		}
	case float64:
		if v == float64(int(v)) && v >= 1 && v <= float64(maximum) {
			return int(v), nil
		}
	}
	return 0, fmt.Errorf("'%s' must be a whole number between 1 and %d", key, maximum)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// instanceSchema is the JSON schema for instance create parameters.
func instanceSchema(plan Plan, tablespaces []string) map[string]any {
	properties := map[string]any{
		"compatibility": map[string]any{
			"type": "string", "enum": compatibilities, "default": "PG",
			"description": "openGauss DBCOMPATIBILITY: PG=PostgreSQL, A=Oracle, B=MySQL, C=Teradata.",
		},
		"encoding": map[string]any{
			"type": "string", "enum": encodings, "default": "UTF8",
			"description": "Character set of the logical database (LC_COLLATE/LC_CTYPE stay 'C').",
		},
		"max_connections": map[string]any{
			"type": "integer", "minimum": 1, "maximum": plan.MaxConnections, "default": plan.MaxConnections,
			"description": "CONNECTION LIMIT of the logical database.",
		},
		"storage_gb": map[string]any{
			"type": "integer", "minimum": 1, "maximum": plan.StorageGB, "default": plan.StorageGB,
			"description": "Storage quota of the logical database.",
		},
		"temp_gb": map[string]any{
			"type": "integer", "minimum": 1, "maximum": plan.TempGB, "default": plan.TempGB,
			"description": "Temp-table space quota (TEMP SPACE).",
		},
		"spill_gb": map[string]any{
			"type": "integer", "minimum": 1, "maximum": plan.SpillGB, "default": plan.SpillGB,
			"description": "Operator spill-to-disk quota (SPILL SPACE).",
		},
	}
	// Only curated tablespaces are offered, as an enum.
	if len(tablespaces) > 0 {
		properties["tablespace"] = map[string]any{
			"type": "string", "enum": tablespaces,
			"description": "Existing tablespace for the logical database (default pg_default).",
		}
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-04/schema#", "type": "object", "properties": properties}
}

// updatableSchema is the JSON schema for instance update parameters; only
// these parameters may change after creation.
func updatableSchema(plan Plan) map[string]any {
	full := instanceSchema(plan, nil)
	properties := full["properties"].(map[string]any)
	updatable := map[string]any{}
	for _, key := range []string{"max_connections", "storage_gb", "temp_gb", "spill_gb"} {
		updatable[key] = properties[key]
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-04/schema#", "type": "object", "properties": updatable}
}

// bindingSchema is the JSON schema for binding create parameters.
func bindingSchema(plan Plan) map[string]any {
	return map[string]any{
		"$schema": "http://json-schema.org/draft-04/schema#",
		"type":    "object",
		"properties": map[string]any{
			"access_role": map[string]any{
				"type": "string", "enum": accessRoles, "default": "readwrite",
				"description": "Access boundary of the binding user: owner (full DDL+DML), readwrite (DML on the tenant schema), readonly (SELECT only).",
			},
			"max_connections": map[string]any{
				"type": "integer", "minimum": 1, "maximum": plan.MaxConnections, "default": plan.MaxConnections,
				"description": "Per-user CONNECTION LIMIT.",
			},
		},
	}
}
