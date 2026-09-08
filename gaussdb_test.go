package main

import (
	"context"
	"strings"
	"testing"
)

// fakeDB stands in for the database: it records every executed statement
// per database and answers existence probes from its sets.
type fakeDB struct {
	statements  map[string][]string
	databases   map[string]bool
	roles       map[string]bool
	tablespaces map[string]bool
	pingErr     error
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
	f.statements[database] = append(f.statements[database], statements...)
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

func (f *fakeDB) all() string {
	var all []string
	for _, statements := range f.statements {
		all = append(all, statements...)
	}
	return strings.Join(all, "\n")
}

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

var names = NamesFor(iid, "gdb")

func instanceParams() InstanceParams {
	return InstanceParams{
		PlanID: "gaussdb-dev", Compatibility: "PG", Encoding: "UTF8",
		MaxConnections: 20, StorageGB: 5,
	}
}

func containsStatement(statements []string, prefix string) bool {
	for _, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			return true
		}
	}
	return false
}

const grp = `"gdb_11111111111111111111111111111111_grp"`
const ts = `"gdb_11111111111111111111111111111111_ts"`
const db = `"gdb_11111111111111111111111111111111"`

func TestProvision(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	if err := admin.Provision(context.Background(), names, instanceParams()); err != nil {
		t.Fatal(err)
	}
	adminStmts := fdb.statements["postgres"]
	want := []string{
		`CREATE ROLE ` + grp + ` NOLOGIN PASSWORD`,
		`GRANT ` + grp + ` TO "admin"`,
		`CREATE TABLESPACE ` + ts + ` OWNER ` + grp + ` RELATIVE LOCATION 'broker/gdb_11111111111111111111111111111111_ts' MAXSIZE '5G'`,
		`CREATE DATABASE ` + db + ` OWNER ` + grp + ` TEMPLATE template0 ENCODING 'UTF8' DBCOMPATIBILITY 'PG' TABLESPACE ` + ts + ` CONNECTION LIMIT 20`,
		`REVOKE CONNECT ON DATABASE ` + db + ` FROM PUBLIC`,
		`GRANT CONNECT ON DATABASE ` + db + ` TO ` + grp,
		`GRANT CONNECT ON DATABASE ` + db + ` TO "admin"`,
		`REVOKE ` + grp + ` FROM "admin"`,
	}
	for _, statement := range want {
		if !containsStatement(adminStmts, statement) {
			t.Errorf("missing:\n %s", statement)
		}
	}
	tenantStmts := fdb.statements[names.Database]
	for _, statement := range []string{
		`ALTER DATABASE ` + db + ` ENABLE PRIVATE OBJECT`,
		`GRANT USAGE, CREATE ON SCHEMA public TO ` + grp,
		`GRANT CREATE ON DATABASE ` + db + ` TO ` + grp,
	} {
		if !containsStatement(tenantStmts, statement) {
			t.Errorf("missing tenant:\n %s", statement)
		}
	}
	// No role-level space quotas (TEMP SPACE, SPILL SPACE, PERM SPACE).
	for _, quota := range []string{"TEMP SPACE", "SPILL SPACE", "PERM SPACE"} {
		if strings.Contains(fdb.all(), quota) {
			t.Errorf("must not emit %s statements", quota)
		}
	}
}

func TestBind(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	password, err := admin.Bind(context.Background(), names, "gdbu_user1", BindingParams{MaxConnections: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != 28 {
		t.Errorf("password length = %d", len(password))
	}
	for _, statement := range []string{
		`CREATE USER "gdbu_user1" LOGIN PASSWORD`,
		`GRANT ` + grp + ` TO "gdbu_user1"`,
	} {
		if !containsStatement(fdb.statements["postgres"], statement) {
			t.Errorf("missing:\n %s", statement)
		}
	}
	for _, statement := range []string{
		`ALTER DEFAULT PRIVILEGES FOR ROLE "gdbu_user1" IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ` + grp,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "gdbu_user1" IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO ` + grp,
	} {
		if !containsStatement(fdb.statements[names.Database], statement) {
			t.Errorf("missing tenant:\n %s", statement)
		}
	}
}

func TestUnbindAndDeprovision(t *testing.T) {
	fdb := newFakeDB()
	admin := NewAdmin(testConfig(), fdb)
	if err := admin.Unbind(context.Background(), names, "gdbu_user1"); err != nil {
		t.Fatal(err)
	}
	if !containsStatement(fdb.statements[names.Database], `DROP OWNED BY "gdbu_user1" CASCADE`) {
		t.Error("missing DROP OWNED BY")
	}

	if err := admin.Deprovision(context.Background(), names); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + db,
		`DROP TABLESPACE IF EXISTS ` + ts,
		`DROP ROLE IF EXISTS ` + grp,
	} {
		if !containsStatement(fdb.statements["postgres"], statement) {
			t.Errorf("missing:\n %s", statement)
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
	for _, statement := range []string{
		`ALTER DATABASE ` + db + ` CONNECTION LIMIT = 10`,
		`ALTER TABLESPACE ` + ts + ` RESIZE MAXSIZE '3G'`,
		`GRANT ` + grp + ` TO "admin"`,
		`REVOKE ` + grp + ` FROM "admin"`,
	} {
		if !containsStatement(fdb.statements["postgres"], statement) {
			t.Errorf("missing:\n %s", statement)
		}
	}
}
