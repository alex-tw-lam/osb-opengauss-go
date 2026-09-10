package main

import (
	"strings"
	"testing"
)

var names = NamesFor(iid, "gdb", "")

func TestShortHashNaming(t *testing.T) {
	// Deterministic
	n1 := NamesFor("abc", "gdb", "")
	n2 := NamesFor("abc", "gdb", "")
	if n1 != n2 {
		t.Fatal("same input must produce same names")
	}
	// Short enough
	if len(n1.Database) > 63 || len(n1.GroupRole) > 63 || len(n1.Tablespace) > 63 {
		t.Errorf("names exceed 63 chars: %s %s %s", n1.Database, n1.GroupRole, n1.Tablespace)
	}
	// Custom name overrides hash
	nc := NamesFor("abc", "gdb", "orders")
	if nc.Database != "gdb_orders" {
		t.Errorf("custom name: got %s, want gdb_orders", nc.Database)
	}
	if nc.GroupRole != "gdb_orders_grp" || nc.Tablespace != "gdb_orders_ts" {
		t.Errorf("custom name derivatives: %+v", nc)
	}
	// Sanitization
	ns := NamesFor("abc", "gdb", "My App-Name!")
	if ns.Database != "gdb_myappname" {
		t.Errorf("sanitized: got %s, want gdb_myappname", ns.Database)
	}
	if n1.Database == "gdb_" {
		t.Error("empty name must fall back to hash")
	}
}

func TestUserNaming(t *testing.T) {
	u1 := UserFor("bid1", "gdb", "")
	if len(u1) > 63 || !strings.HasPrefix(u1, "gdbu_") {
		t.Errorf("user name wrong: %s", u1)
	}
	uc := UserFor("bid1", "gdb", "reporting")
	if uc != "gdbu_reporting" {
		t.Errorf("custom: got %s, want gdbu_reporting", uc)
	}
}

// A long custom name must not push any derived identifier past 63 characters.
func TestLongNameStaysWithinIdentifierLimit(t *testing.T) {
	long := strings.Repeat("x", 100)
	n := NamesFor("abc", "gdb", long)
	if len(n.Database) > 63 || len(n.GroupRole) > 63 || len(n.Tablespace) > 63 {
		t.Fatalf("identifier exceeds 63 chars: %+v", n)
	}
	if u := UserFor("bid1", "gdb", long); len(u) > 63 {
		t.Fatalf("user name exceeds 63 chars: %s", u)
	}
}
