package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateParametersAcceptsValidInput(t *testing.T) {
	schema := instanceSchema(devPlan)
	valid := json.RawMessage(`{"compatibility":"A","encoding":"GBK","max_connections":10,"name":"orders"}`)
	if err := ValidateParameters(schema, valid); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func TestValidateParametersAcceptsEmpty(t *testing.T) {
	schema := instanceSchema(devPlan)
	if err := ValidateParameters(schema, nil); err != nil {
		t.Fatalf("empty input rejected: %v", err)
	}
	if err := ValidateParameters(schema, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("empty object rejected: %v", err)
	}
}

func TestValidateParametersRejectsWrongType(t *testing.T) {
	schema := instanceSchema(devPlan)
	wrong := json.RawMessage(`{"max_connections":"twenty"}`)
	err := ValidateParameters(schema, wrong)
	if err == nil {
		t.Fatal("string for integer must be rejected")
	}
	if !strings.Contains(err.Error(), "max_connections") {
		t.Errorf("error should mention the field: %v", err)
	}
}

func TestValidateParametersRejectsOutOfRange(t *testing.T) {
	schema := instanceSchema(devPlan)
	wrong := json.RawMessage(`{"storage_gb":999}`)
	if err := ValidateParameters(schema, wrong); err == nil {
		t.Fatal("out of range must be rejected")
	}
}

func TestValidateParametersRejectsBadEnum(t *testing.T) {
	schema := instanceSchema(devPlan)
	wrong := json.RawMessage(`{"compatibility":"ORACLE"}`)
	if err := ValidateParameters(schema, wrong); err == nil {
		t.Fatal("invalid enum must be rejected")
	}
}

func TestValidateParametersRejectsNonObject(t *testing.T) {
	schema := instanceSchema(devPlan)
	wrong := json.RawMessage(`[1,2,3]`)
	if err := ValidateParameters(schema, wrong); err == nil {
		t.Fatal("array must be rejected for an object schema")
	}
}

func TestValidateBindingParameters(t *testing.T) {
	schema := bindingSchema(devPlan)
	if err := ValidateParameters(schema, json.RawMessage(`{"name":"reporting"}`)); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	if err := ValidateParameters(schema, json.RawMessage(`{"max_connections":5}`)); err != nil {
		t.Log("max_connections in binding is an unknown field; some validators reject it")
	}
}
