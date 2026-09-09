// state.go is the broker's memory: instance and binding records kept in a
// SQL database through GORM. Binding credentials are encrypted at rest with
// AES-256-GCM when STATE_ENCRYPTION_KEY is set; the SQLite file is
// restricted to 0600.

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
// are stored encrypted so an identical repeated bind returns the same password.
type BindingRecord struct {
	BindingID   string `gorm:"primaryKey"`
	InstanceID  string
	Username    string
	Params      BindingParams `gorm:"serializer:json"`
	Credentials string        // encrypted JSON; decrypted by the Store
}

// Store wraps the state database and its encryptor.
type Store struct {
	db        *gorm.DB
	encryptor Encryptor
}

// OpenStore opens (creating if needed) the state database selected by the
// configuration and creates its two tables.
func OpenStore(cfg *Config, encryptor Encryptor) (*Store, error) {
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
	// Restrict the SQLite file to the broker's own user.
	if cfg.StateDSN == "" {
		if err := os.Chmod(cfg.StatePath, 0o600); err != nil {
			return nil, fmt.Errorf("cannot restrict state file permissions: %w", err)
		}
	}
	return &Store{db: db, encryptor: encryptor}, nil
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

// PutBinding records a binding with encrypted credentials.
func (s *Store) PutBinding(bindingID string, username, instanceID string, params BindingParams, credentials map[string]string) error {
	plain, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	encrypted, err := s.encryptor.Encrypt(plain)
	if err != nil {
		return fmt.Errorf("cannot encrypt credentials: %w", err)
	}
	record := BindingRecord{
		BindingID:   bindingID,
		InstanceID:  instanceID,
		Username:    username,
		Params:      params,
		Credentials: string(encrypted),
	}
	err = s.db.First(&BindingRecord{}, "binding_id = ?", bindingID).Error
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

// BindingInfo is the decrypted view of a stored binding.
type BindingInfo struct {
	BindingID   string
	InstanceID  string
	Username    string
	Params      BindingParams
	Credentials map[string]string
}

// GetBinding returns the decrypted binding, or nil if unknown.
func (s *Store) GetBinding(bindingID string) *BindingInfo {
	var record BindingRecord
	err := s.db.First(&record, "binding_id = ?", bindingID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		panic(fmt.Sprintf("state database error: %v", err))
	}
	plain, err := s.encryptor.Decrypt([]byte(record.Credentials))
	if err != nil {
		panic(fmt.Sprintf("cannot decrypt credentials for binding %s: %v", bindingID, err))
	}
	var creds map[string]string
	if err := json.Unmarshal(plain, &creds); err != nil {
		panic(fmt.Sprintf("corrupted credentials for binding %s: %v", bindingID, err))
	}
	return &BindingInfo{
		BindingID:   record.BindingID,
		InstanceID:  record.InstanceID,
		Username:    record.Username,
		Params:      record.Params,
		Credentials: creds,
	}
}

// DeleteBinding forgets a binding.
func (s *Store) DeleteBinding(bindingID string) error {
	return s.db.Delete(&BindingRecord{}, "binding_id = ?", bindingID).Error
}

// BindingsForInstance returns every binding that still exists on an instance.
func (s *Store) BindingsForInstance(instanceID string) []BindingInfo {
	var records []BindingRecord
	_ = s.db.Where("instance_id = ?", instanceID).Find(&records).Error
	result := make([]BindingInfo, 0, len(records))
	for _, record := range records {
		plain, err := s.encryptor.Decrypt([]byte(record.Credentials))
		if err != nil {
			continue // skip unreadable records rather than crashing the list
		}
		var creds map[string]string
		if err := json.Unmarshal(plain, &creds); err != nil {
			continue
		}
		result = append(result, BindingInfo{
			BindingID:   record.BindingID,
			InstanceID:  record.InstanceID,
			Username:    record.Username,
			Params:      record.Params,
			Credentials: creds,
		})
	}
	return result
}
