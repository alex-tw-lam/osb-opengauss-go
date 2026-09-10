package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeDB stands in for the database.
type fakeDB struct {
	statements  map[string][]string
	databases   map[string]bool
	roles       map[string]bool
	tablespaces map[string]bool
	pingErr     error
	failOn      string // any statement containing this text fails
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		statements:  map[string][]string{},
		databases:   map[string]bool{},
		roles:       map[string]bool{},
		tablespaces: map[string]bool{},
	}
}

func (f *fakeDB) Exec(_ context.Context, database string, statements ...string) error {
	if database == "" {
		database = "postgres"
	}
	for _, statement := range statements {
		if f.failOn != "" && strings.Contains(statement, f.failOn) {
			return errors.New("injected failure on: " + f.failOn)
		}
		f.statements[database] = append(f.statements[database], statement)
	}
	return nil
}

func (f *fakeDB) Exists(_ context.Context, table, _, name string) (bool, error) {
	switch table {
	case "pg_database":
		return f.databases[name], nil
	case "pg_roles":
		return f.roles[name], nil
	case "pg_tablespace":
		return f.tablespaces[name], nil
	}
	return false, nil
}

func (f *fakeDB) Ping(context.Context) error { return f.pingErr }

func testConfig() *Config {
	return &Config{
		DBHost: "db.example.org", DBPort: 6789, DBUser: "admin", DBPassword: "admin-secret",
		DBAdminName: "postgres", DBSSLMode: "disable", DBConnTimeout: 10,
		PlansFile: "plans.toml", TablespacePrefix: "broker",
		BrokerUsername: "broker", BrokerPassword: "x", StatePath: "unused",
		NamePrefix: "gdb", Host: "127.0.0.1", Port: 5000,
	}
}

const iid = "11111111-1111-1111-1111-111111111111"

func instanceParams() InstanceParams {
	return InstanceParams{
		Compatibility: "PG", Encoding: "UTF8",
		MaxConnections: 20, StorageGB: 5,
	}
}

func containsStatement(statements []string, prefix string) bool {
	for _, s := range statements {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// A provision that fails partway must roll back what it already created.
func TestProvisionRollsBackPartialCreation(t *testing.T) {
	fdb := newFakeDB()
	fdb.failOn = "CREATE TABLESPACE"
	admin := NewAdmin(testConfig(), fdb)
	if err := admin.Provision(context.Background(), names, instanceParams()); err == nil {
		t.Fatal("provision must fail when a statement fails")
	}
	// The rollback must have dropped the role it managed to create.
	if !containsStatement(fdb.statements["postgres"], "DROP ROLE IF EXISTS "+quoteIdent(names.GroupRole)) {
		t.Fatal("rollback did not drop the partially created role")
	}
}

// A bind that fails partway must roll back the user it already created.
func TestBindRollsBackPartialCreation(t *testing.T) {
	fdb := newFakeDB()
	fdb.failOn = "GRANT"
	admin := NewAdmin(testConfig(), fdb)
	if _, err := admin.Bind(context.Background(), names, "gdbu_user1"); err == nil {
		t.Fatal("bind must fail when a statement fails")
	}
	if !containsStatement(fdb.statements["postgres"], "DROP USER IF EXISTS \"gdbu_user1\"") {
		t.Fatal("rollback did not drop the partially created user")
	}
}

func TestProvision(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	grp := quoteIdent(names.GroupRole)
	ts := quoteIdent(names.Tablespace)
	db := quoteIdent(names.Database)
	for _, s := range []string{
		"CREATE ROLE " + grp + " NOLOGIN PASSWORD",
		"CREATE TABLESPACE " + ts + " OWNER " + grp,
		"CREATE DATABASE " + db + " OWNER " + grp,
		"REVOKE CONNECT ON DATABASE " + db + " FROM PUBLIC",
	} {
		if !containsStatement(fdb.statements["postgres"], s) {
			t.Errorf("missing:\n %s", s)
		}
	}
	for _, s := range []string{
		"ALTER DATABASE " + db + " ENABLE PRIVATE OBJECT",
		"GRANT USAGE, CREATE ON SCHEMA public TO " + grp,
	} {
		if !containsStatement(fdb.statements[names.Database], s) {
			t.Errorf("missing tenant:\n %s", s)
		}
	}
}

func TestBind(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	pw, err := admin.Bind(context.Background(), names, "gdbu_user1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 28 {
		t.Errorf("password length = %d", len(pw))
	}
	grp := quoteIdent(names.GroupRole)
	if !containsStatement(fdb.statements["postgres"], "CREATE USER \"gdbu_user1\" LOGIN PASSWORD") {
		t.Error("missing CREATE USER")
	}
	if !containsStatement(fdb.statements["postgres"], "GRANT "+grp+" TO \"gdbu_user1\"") {
		t.Error("missing GRANT group TO user")
	}
}

func TestUnbindDeprovision(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	if err := admin.Unbind(context.Background(), names, "gdbu_user1"); err != nil {
		t.Fatal(err)
	}
	if !containsStatement(fdb.statements[names.Database], "DROP OWNED BY \"gdbu_user1\" CASCADE") {
		t.Error("missing DROP OWNED BY")
	}
	if err := admin.Deprovision(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	db := quoteIdent(names.Database)
	ts := quoteIdent(names.Tablespace)
	grp := quoteIdent(names.GroupRole)
	for _, s := range []string{
		"DROP DATABASE IF EXISTS " + db,
		"DROP TABLESPACE IF EXISTS " + ts,
		"DROP ROLE IF EXISTS " + grp,
	} {
		if !containsStatement(fdb.statements["postgres"], s) {
			t.Errorf("missing:\n %s", s)
		}
	}
}

func TestUpdate(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	updated := instanceParams()
	updated.MaxConnections = 10
	updated.StorageGB = 3
	if err := admin.Update(context.Background(), names, updated); err != nil {
		t.Fatal(err)
	}
	db := quoteIdent(names.Database)
	ts := quoteIdent(names.Tablespace)
	for _, s := range []string{
		"ALTER DATABASE " + db + " CONNECTION LIMIT = 10",
		"ALTER TABLESPACE " + ts + " RESIZE MAXSIZE '3G'",
	} {
		if !containsStatement(fdb.statements["postgres"], s) {
			t.Errorf("missing:\n %s", s)
		}
	}
}
