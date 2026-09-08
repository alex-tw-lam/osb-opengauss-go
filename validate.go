// validate.go validates incoming request parameters against the same JSON
// schemas that are published in the catalog. This is the single source of
// truth: what the platform sees is what the broker enforces.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"
)

// ValidateParameters checks raw JSON parameters against a JSON schema.
// Empty or null parameters are always valid (the caller applies defaults).
func ValidateParameters(schema map[string]any, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	schemaLoader := gojsonschema.NewGoLoader(schema)
	documentLoader := gojsonschema.NewBytesLoader(raw)
	result, err := gojsonschema.Validate(schemaLoader, documentLoader)
	if err != nil {
		return fmt.Errorf("cannot validate parameters: %w", err)
	}
	if !result.Valid() {
		var msgs []string
		for _, e := range result.Errors() {
			msgs = append(msgs, e.String())
		}
		return fmt.Errorf("invalid parameters: %s", strings.Join(msgs, "; "))
	}
	return nil
}
