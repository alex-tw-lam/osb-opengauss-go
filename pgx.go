// pgx.go is the only file that knows the pgx driver. It implements the DB
// interface on top of real openGauss connections. Note on authentication:
// pgx speaks the PostgreSQL protocol (md5/SCRAM); openGauss's default sha256
// password storage is not supported by pgx, so the server must accept md5
// for the broker admin (password_encryption_type 0 or 1, and a matching
// pg_hba.conf line) - see README.

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PgxDB opens one short-lived connection per call group, mirroring the
// broker's small, transaction-free DDL batches.
type PgxDB struct {
	cfg *Config
}

// NewPgxDB creates the database access for the given configuration.
func NewPgxDB(cfg *Config) *PgxDB {
	return &PgxDB{cfg: cfg}
}

func (p *PgxDB) connect(ctx context.Context, database string) (*pgx.Conn, error) {
	cfg := p.cfg
	if database == "" {
		database = cfg.DBAdminName
	}
	url := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBPort, database, cfg.DBSSLMode)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.DBConnTimeout)*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	var mode string
	if err := conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&mode); err == nil && mode == "on" {
		_ = conn.Close(ctx) // best effort; the read-only node is rejected anyway
		return nil, fmt.Errorf("connected to a read-only node")
	}
	return conn, nil
}

// Exec runs the statements on the given database, one after another.
func (p *PgxDB) Exec(ctx context.Context, database string, statements ...string) error {
	conn, err := p.connect(ctx, database)
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
func (p *PgxDB) Exists(ctx context.Context, table, column, name string) (bool, error) {
	conn, err := p.connect(ctx, "")
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	// table and column are hardcoded internal constants; name is escaped.
	var one int //nolint:gosec // G101: no credential, just a probe result
	err = conn.QueryRow(ctx,
		fmt.Sprintf("SELECT 1 FROM %s WHERE %s = %s", table, column, quoteLiteral(name))).Scan(&one)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// Ping runs one round trip over the real connection path.
func (p *PgxDB) Ping(ctx context.Context) error {
	conn, err := p.connect(ctx, "")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var one int
	return conn.QueryRow(ctx, "SELECT 1").Scan(&one)
}
