// driver.go is the only file that knows the database driver:
// github.com/HuaweiCloudDeveloper/gaussdb-go, a pgx fork maintained by the
// same Huawei org as the Python repository's driver. It speaks openGauss's
// native sha256 authentication (password_encryption_type = 2) as well as
// md5 (type 0/1), so it is the single driver for every server configuration.
// Tests never touch this file: they inject a fake DB.

package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	gaussdb "github.com/HuaweiCloudDeveloper/gaussdb-go"
)

// NewDB returns the database access for the given configuration.
func NewDB(cfg *Config) DB {
	return &gaussdbDB{cfg: cfg}
}

// gaussdbDB opens one short-lived connection per call group, mirroring the
// broker's small, transaction-free DDL batches.
type gaussdbDB struct {
	cfg *Config
}

// Exec runs the statements on the given database, one after another.
func (g *gaussdbDB) Exec(ctx context.Context, database string, statements ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(g.cfg.DBConnTimeout)*time.Second)
	defer cancel()
	conn, err := gaussdb.Connect(ctx, connURL(g.cfg, database))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	return nil
}

// Exists probes a system catalog (pg_database / pg_roles / pg_tablespace).
func (g *gaussdbDB) Exists(ctx context.Context, table, column, name string) (bool, error) {
	conn, err := gaussdb.Connect(ctx, connURL(g.cfg, ""))
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	// table and column are hardcoded internal constants; name is escaped.
	var one int
	err = conn.QueryRow(ctx,
		fmt.Sprintf("SELECT 1 FROM %s WHERE %s = %s", table, column, quoteLiteral(name))).Scan(&one)
	if err == gaussdb.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// Ping runs one round trip over the real connection path.
func (g *gaussdbDB) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(g.cfg.DBConnTimeout)*time.Second)
	defer cancel()
	conn, err := gaussdb.Connect(ctx, connURL(g.cfg, ""))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}

// connURL builds the escaped gaussdb:// connection URL for one database.
func connURL(cfg *Config, database string) string {
	if database == "" {
		database = cfg.DBAdminName
	}
	u := url.URL{
		Scheme:   "gaussdb",
		User:     url.UserPassword(cfg.DBUser, cfg.DBPassword),
		Host:     fmt.Sprintf("%s:%d", cfg.DBHost, cfg.DBPort),
		Path:     database,
		RawQuery: url.Values{"sslmode": {cfg.DBSSLMode}, "connect_timeout": {fmt.Sprint(cfg.DBConnTimeout)}}.Encode(),
	}
	return u.String()
}
