// state.go is the broker's memory: instance and binding records kept in a
// SQL database through GORM, so repeated and conflicting platform calls can
// be answered according to the Open Service Broker rules (identical repeats
// succeed, conflicts are rejected, unknown deletes report gone).
//
// The default is a SQLite file next to the binary. Setting STATE_DSN to a
// postgres:// URL moves the state to any PostgreSQL-compatible server, and a
// gaussdb:// URL moves it to an openGauss/GaussDB server (native sha256) -
// including the very instance the broker manages, which then needs no local
// state file at all.

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/HuaweiCloudDeveloper/gaussdb-go/stdlib" // registers the gaussdb database/sql driver
	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InstanceRecord is what the broker remembers about one service instance.
type InstanceRecord struct {
	InstanceID string `gorm:"primaryKey"`
	ServiceID  string
	PlanID     string
	Database   string
	Params     InstanceParams `gorm:"serializer:json"`
}

// BindingRecord is what the broker remembers about one binding. Credentials
// are stored so an identical repeated bind returns the same password.
type BindingRecord struct {
	BindingID   string `gorm:"primaryKey"`
	InstanceID  string
	Username    string
	Params      BindingParams     `gorm:"serializer:json"`
	Credentials map[string]string `gorm:"serializer:json"`
}

// Store wraps the state database.
type Store struct {
	db *gorm.DB
}

// OpenStore opens (creating if needed) the state database selected by the
// configuration and creates its two tables.
//
// The tables are created with plain CREATE TABLE IF NOT EXISTS statements
// instead of GORM's AutoMigrate: the migrator's existence checks use
// parameterized catalog queries that the gaussdb driver rejects on
// PostgreSQL-9.2-based servers, which would stop a broker restart whenever
// the tables already exist.
func OpenStore(cfg *Config) (*Store, error) {
	dialector, err := stateDialector(cfg)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("cannot open state database: %w", err)
	}
	for _, ddl := range stateSchema {
		if err := db.Exec(ddl).Error; err != nil {
			return nil, fmt.Errorf("cannot create state tables: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// stateSchema is the whole state schema, in plain SQL.
var stateSchema = []string{
	`CREATE TABLE IF NOT EXISTS instance_records (
		instance_id TEXT PRIMARY KEY,
		service_id  TEXT NOT NULL,
		plan_id     TEXT NOT NULL,
		database    TEXT NOT NULL,
		params      TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS binding_records (
		binding_id   TEXT PRIMARY KEY,
		instance_id  TEXT NOT NULL,
		username     TEXT NOT NULL,
		params       TEXT NOT NULL,
		credentials  TEXT NOT NULL
	)`,
}

// stateDialector picks the GORM dialector for the configured state backend.
func stateDialector(cfg *Config) (gorm.Dialector, error) {
	switch {
	case cfg.StateDSN == "":
		return sqlite.Open(cfg.StatePath), nil
	case strings.HasPrefix(cfg.StateDSN, "gaussdb://"):
		// The gaussdb driver speaks openGauss sha256; the statements GORM
		// emits are plain PostgreSQL, which openGauss accepts.
		sqlDB, err := sql.Open("gaussdb", cfg.StateDSN)
		if err != nil {
			return nil, err
		}
		return postgres.New(postgres.Config{Conn: sqlDB}), nil
	case strings.HasPrefix(cfg.StateDSN, "postgres://"), strings.HasPrefix(cfg.StateDSN, "postgresql://"):
		return postgres.Open(cfg.StateDSN), nil
	default:
		return nil, fmt.Errorf("STATE_DSN must be a postgres:// or gaussdb:// URL, got %q", cfg.StateDSN)
	}
}

// Close releases the state database.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// PutInstance records an instance (insert or update).
func (s *Store) PutInstance(instanceID string, record InstanceRecord) error {
	record.InstanceID = instanceID
	// Written as an explicit read-then-write: GORM's Save() would emit
	// INSERT ... ON CONFLICT, which openGauss (PostgreSQL 9.2 based) does
	// not support.
	err := s.db.First(&InstanceRecord{}, "instance_id = ?", instanceID).Error
	switch {
	case err == nil:
		return s.db.Model(&InstanceRecord{}).Where("instance_id = ?", instanceID).
			Select("*").Updates(record).Error
	case errors.Is(err, gorm.ErrRecordNotFound):
		return s.db.Create(&record).Error
	default:
		return err
	}
}

// GetInstance returns the record of an instance, or nil if unknown.
// A database error (as opposed to not-found) panics: the broker should
// not silently treat a broken state database as "everything is gone".
func (s *Store) GetInstance(instanceID string) *InstanceRecord {
	var record InstanceRecord
	err := s.db.First(&record, "instance_id = ?", instanceID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		panic(fmt.Sprintf("state database error: %v", err))
	}
	return &record
}

// DeleteInstance forgets an instance.
func (s *Store) DeleteInstance(instanceID string) error {
	return s.db.Delete(&InstanceRecord{}, "instance_id = ?", instanceID).Error
}

// PutBinding records a binding.
func (s *Store) PutBinding(bindingID string, record BindingRecord) error {
	record.BindingID = bindingID
	// Same read-then-write pattern as PutInstance: no ON CONFLICT upsert.
	err := s.db.First(&BindingRecord{}, "binding_id = ?", bindingID).Error
	switch {
	case err == nil:
		return s.db.Model(&BindingRecord{}).Where("binding_id = ?", bindingID).
			Select("*").Updates(record).Error
	case errors.Is(err, gorm.ErrRecordNotFound):
		return s.db.Create(&record).Error
	default:
		return err
	}
}

// GetBinding returns the record of a binding, or nil if unknown.
func (s *Store) GetBinding(bindingID string) *BindingRecord {
	var record BindingRecord
	err := s.db.First(&record, "binding_id = ?", bindingID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		panic(fmt.Sprintf("state database error: %v", err))
	}
	return &record
}

// DeleteBinding forgets a binding.
func (s *Store) DeleteBinding(bindingID string) error {
	return s.db.Delete(&BindingRecord{}, "binding_id = ?", bindingID).Error
}

// BindingsForInstance returns every binding that still exists on an instance.
func (s *Store) BindingsForInstance(instanceID string) []BindingRecord {
	var records []BindingRecord
	_ = s.db.Where("instance_id = ?", instanceID).Find(&records).Error
	return records
}
