// state.go is the broker's memory: a bbolt file that records what was
// created, so repeated and conflicting platform calls can be answered
// according to the Open Service Broker rules (identical repeats succeed,
// conflicts are rejected, unknown deletes report gone).

package main

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	instancesBucket = []byte("instances")
	bindingsBucket  = []byte("bindings")
)

// InstanceRecord is what the broker remembers about one service instance.
type InstanceRecord struct {
	ServiceID string         `json:"service_id"`
	PlanID    string         `json:"plan_id"`
	Database  string         `json:"database"`
	Params    InstanceParams `json:"params"`
}

// BindingRecord is what the broker remembers about one binding. Credentials
// are stored so an identical repeated bind returns the same password.
type BindingRecord struct {
	InstanceID  string            `json:"instance_id"`
	Username    string            `json:"username"`
	Params      BindingParams     `json:"params"`
	Credentials map[string]string `json:"credentials"`
}

// Store wraps the bbolt file.
type Store struct {
	db *bolt.DB
}

// OpenStore opens (creating if needed) the state file.
func OpenStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("cannot open state file %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{instancesBucket, bindingsBucket} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("cannot create state bucket: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close() // nothing left to do; the bucket creation error is the real failure
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the state file.
func (s *Store) Close() error { return s.db.Close() }

// PutInstance records an instance.
func (s *Store) PutInstance(instanceID string, record InstanceRecord) error {
	return s.put(instancesBucket, instanceID, record)
}

// GetInstance returns the record of an instance, or nil if unknown.
func (s *Store) GetInstance(instanceID string) *InstanceRecord {
	var record InstanceRecord
	if !s.get(instancesBucket, instanceID, &record) {
		return nil
	}
	return &record
}

// DeleteInstance forgets an instance.
func (s *Store) DeleteInstance(instanceID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(instancesBucket).Delete([]byte(instanceID))
	})
}

// PutBinding records a binding.
func (s *Store) PutBinding(bindingID string, record BindingRecord) error {
	return s.put(bindingsBucket, bindingID, record)
}

// GetBinding returns the record of a binding, or nil if unknown.
func (s *Store) GetBinding(bindingID string) *BindingRecord {
	var record BindingRecord
	if !s.get(bindingsBucket, bindingID, &record) {
		return nil
	}
	return &record
}

// DeleteBinding forgets a binding.
func (s *Store) DeleteBinding(bindingID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bindingsBucket).Delete([]byte(bindingID))
	})
}

// BindingsForInstance returns every binding that still exists on an instance.
func (s *Store) BindingsForInstance(instanceID string) []BindingRecord {
	var records []BindingRecord
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bindingsBucket).ForEach(func(_, value []byte) error {
			var record BindingRecord
			if json.Unmarshal(value, &record) == nil && record.InstanceID == instanceID {
				records = append(records, record)
			}
			return nil
		})
	})
	return records
}

func (s *Store) put(bucket []byte, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), encoded)
	})
}

func (s *Store) get(bucket []byte, key string, out any) bool {
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(bucket).Get([]byte(key))
		if value == nil {
			return nil
		}
		found = json.Unmarshal(value, out) == nil
		return nil
	})
	return found
}
