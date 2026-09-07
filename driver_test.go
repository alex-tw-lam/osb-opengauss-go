package main

import (
	"strings"
	"testing"
)

func TestConnURLEscaping(t *testing.T) {
	cfg := testConfig("role_quota") // user admin / admin-secret @ db.example.org:6789
	url := connURL(cfg, "")
	for _, want := range []string{
		"gaussdb://admin:admin-secret@db.example.org:6789/postgres",
		"sslmode=disable",
		"connect_timeout=10",
	} {
		if !strings.Contains(url, want) {
			t.Errorf("connURL %q missing %q", url, want)
		}
	}
	if got := connURL(cfg, "tenantdb"); !strings.Contains(got, "/tenantdb?") {
		t.Errorf("database name not in path: %s", got)
	}
	// A password with URL-special characters must be percent-encoded.
	cfg.DBPassword = "p@ss:word/x"
	encoded := connURL(cfg, "")
	if strings.Contains(encoded, "p@ss:word/x") {
		t.Errorf("password not escaped: %s", encoded)
	}
}
