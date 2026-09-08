package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateParameters(t *testing.T) {
	schema := instanceSchema(devPlan)
	cases := []struct {
		name string
		raw  json.RawMessage
		ok   bool
	}{
		{"valid full", json.RawMessage(`{"compatibility":"A","encoding":"GBK","max_connections":10,"name":"orders"}`), true},
		{"valid empty object", json.RawMessage(`{}`), true},
		{"nil parameters", nil, true},
		{"null parameters", json.RawMessage(`null`), true},
		{"string for integer", json.RawMessage(`{"max_connections":"twenty"}`), false},
		{"out of range", json.RawMessage(`{"storage_gb":999}`), false},
		{"invalid enum", json.RawMessage(`{"compatibility":"ORACLE"}`), false},
		{"array not object", json.RawMessage(`[1,2,3]`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateParameters(schema, tc.raw)
			if tc.ok && err != nil {
				t.Fatalf("rejected valid input: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}

func TestValidateParametersFieldInError(t *testing.T) {
	err := ValidateParameters(instanceSchema(devPlan), json.RawMessage(`{"max_connections":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "max_connections") {
		t.Errorf("error should mention the field: %v", err)
	}
}

func TestValidateBindingParameters(t *testing.T) {
	if err := ValidateParameters(bindingSchema(devPlan), json.RawMessage(`{"name":"reporting"}`)); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
}
