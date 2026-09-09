// params.go decides what a valid request looks like: it merges user-supplied
// parameters over a plan's defaults and rejects anything that would exceed
// the plan. The JSON schemas published in the catalog state the same rules.

package main

import (
	"encoding/json"
	"fmt"
	"slices"
)

var (
	compatibilities = []string{"PG", "A", "B", "C"} // openGauss DBCOMPATIBILITY
	encodings       = []string{"UTF8", "GBK", "GB18030", "Latin1"}
)

// InstanceParams is the fully resolved parameter set of one logical database.
type InstanceParams struct {
	PlanID         string `json:"plan_id"`
	Name           string `json:"name"`
	Compatibility  string `json:"compatibility"`
	Encoding       string `json:"encoding"`
	MaxConnections int    `json:"max_connections"`
	StorageGB      int    `json:"storage_gb"`
}

// BindingParams is the fully resolved parameter set of one binding user.
// All bindings are read-write; there is no access_role.
type BindingParams struct {
	Name string `json:"name"`
}

// rawParameters converts the raw JSON parameters of a request into a map.
func rawParameters(raw json.RawMessage) (map[string]any, error) {
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
func ResolveInstanceParams(plan Plan, params map[string]any) (InstanceParams, error) {
	resolved := InstanceParams{
		PlanID:         plan.ID,
		Compatibility:  "PG",
		Encoding:       "UTF8",
		MaxConnections: plan.MaxConnections,
		StorageGB:      plan.StorageGB,
	}
	if v, ok := params["compatibility"]; ok {
		resolved.Compatibility = fmt.Sprint(v)
		if !slices.Contains(compatibilities, resolved.Compatibility) {
			return resolved, fmt.Errorf("'compatibility' must be one of %v", compatibilities)
		}
	}
	if v, ok := params["encoding"]; ok {
		resolved.Encoding = fmt.Sprint(v)
		if !slices.Contains(encodings, resolved.Encoding) {
			return resolved, fmt.Errorf("'encoding' must be one of %v", encodings)
		}
	}
	if v, ok := params["name"]; ok {
		resolved.Name = fmt.Sprint(v)
	}
	var err error
	if resolved.MaxConnections, err = boundedInt(params, "max_connections", plan.MaxConnections, plan.MaxConnections); err != nil {
		return resolved, err
	}
	if resolved.StorageGB, err = boundedInt(params, "storage_gb", plan.StorageGB, plan.StorageGB); err != nil {
		return resolved, err
	}
	return resolved, nil
}

// ResolveBindingParams merges user parameters over the plan defaults.
func ResolveBindingParams(params map[string]any) (BindingParams, error) {
	resolved := BindingParams{}
	if v, ok := params["name"]; ok {
		resolved.Name = fmt.Sprint(v)
	}
	return resolved, nil
}

// boundedInt reads key from params as an integer between 1 and maximum.
func boundedInt(params map[string]any, key string, fallback, maximum int) (int, error) {
	raw, ok := params[key]
	if !ok {
		return fallback, nil
	}
	// JSON numbers decode as float64, so one type assertion covers them all.
	v, isNumber := raw.(float64)
	if isNumber && v == float64(int(v)) && v >= 1 && v <= float64(maximum) {
		return int(v), nil
	}
	return 0, fmt.Errorf("'%s' must be a whole number between 1 and %d", key, maximum)
}

// instanceSchema is the JSON schema for instance create parameters.
func instanceSchema(plan Plan) map[string]any {
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
			"description": "Storage quota of the logical database (tablespace MAXSIZE).",
		},
		"name": map[string]any{
			"type":        "string",
			"description": "Human-readable name for the database.",
		},
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-04/schema#", "type": "object", "properties": properties}
}

// updatableSchema is the JSON schema for instance update parameters; only
// these parameters may change after creation.
func updatableSchema(plan Plan) map[string]any {
	full := instanceSchema(plan)
	properties := full["properties"].(map[string]any)
	updatable := map[string]any{}
	for _, key := range []string{"max_connections", "storage_gb"} {
		updatable[key] = properties[key]
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-04/schema#", "type": "object", "properties": updatable}
}

// bindingSchema is the JSON schema for binding create parameters.
func bindingSchema() map[string]any {
	return map[string]any{
		"$schema": "http://json-schema.org/draft-04/schema#",
		"type":    "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Human-readable name for the binding user.",
			},
		},
	}
}
