package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// Entry is a single key/value pair carried by a rollout.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Rollout is one immutable config rollout record.
type Rollout struct {
	ConfigName        string  `json:"configName"`
	TargetEnvironment string  `json:"targetEnvironment"`
	Namespace         string  `json:"namespace"`
	Version           int64   `json:"version"`
	Entries           []Entry `json:"entries"`
	Digest            string  `json:"digest"`
	Revoked           bool    `json:"revoked"`
}

// ErrConflict reports that a rollout identity already exists with different content.
var ErrConflict = errors.New("config rollout identity exists with different content")

// DigestEntries returns the canonical content digest of an entry set. Entry order
// does not matter; any key or value change produces a different digest.
func DigestEntries(entries []Entry) string {
	sum := sha256.New()
	for _, entry := range SortedEntries(entries) {
		fmt.Fprintf(sum, "%d:%s%d:%s", len(entry.Key), entry.Key, len(entry.Value), entry.Value)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// SortedEntries returns a copy of entries ordered by key.
func SortedEntries(entries []Entry) []Entry {
	ordered := make([]Entry, len(entries))
	copy(ordered, entries)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	return ordered
}

// CreateRollout stores a new rollout inside a single transaction guarded by the
// primary key. It returns the stored record and whether this call created it.
// Re-submitting identical content returns the existing record with created set to
// false; different content for the same identity returns ErrConflict.
func (s *Store) CreateRollout(ctx context.Context, rollout Rollout) (Rollout, bool, error) {
	rollout.Entries = SortedEntries(rollout.Entries)
	rollout.Digest = DigestEntries(rollout.Entries)
	rollout.Revoked = false

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Rollout{}, false, fmt.Errorf("begin rollout tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO config_rollouts (config_name, target_environment, namespace, version, digest, revoked)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT DO NOTHING`,
		rollout.ConfigName, rollout.TargetEnvironment, rollout.Namespace, rollout.Version, rollout.Digest)
	if err != nil {
		return Rollout{}, false, fmt.Errorf("insert rollout: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Rollout{}, false, fmt.Errorf("insert rollout result: %w", err)
	}
	if affected == 0 {
		stored, err := readRollout(ctx, tx, rollout.ConfigName, rollout.TargetEnvironment, rollout.Namespace, rollout.Version)
		if err != nil {
			return Rollout{}, false, err
		}
		if stored.Digest != rollout.Digest {
			return Rollout{}, false, ErrConflict
		}
		return stored, false, nil
	}

	for _, entry := range rollout.Entries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO config_rollout_entries (config_name, target_environment, namespace, version, entry_key, entry_value)
			VALUES (?, ?, ?, ?, ?, ?)`,
			rollout.ConfigName, rollout.TargetEnvironment, rollout.Namespace, rollout.Version, entry.Key, entry.Value); err != nil {
			return Rollout{}, false, fmt.Errorf("insert rollout entry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Rollout{}, false, fmt.Errorf("commit rollout: %w", err)
	}
	return rollout, true, nil
}

// ListRollouts returns every rollout in the given scope ordered by version ascending.
func (s *Store) ListRollouts(ctx context.Context, environment, namespace, configName string) ([]Rollout, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.version, r.digest, r.revoked, e.entry_key, e.entry_value
		FROM config_rollouts AS r
		JOIN config_rollout_entries AS e
			ON e.config_name = r.config_name
			AND e.target_environment = r.target_environment
			AND e.namespace = r.namespace
			AND e.version = r.version
		WHERE r.target_environment = ? AND r.namespace = ? AND r.config_name = ?
		ORDER BY r.version ASC, e.entry_key ASC`,
		environment, namespace, configName)
	if err != nil {
		return nil, fmt.Errorf("list rollouts: %w", err)
	}
	defer rows.Close()

	var rollouts []Rollout
	for rows.Next() {
		var (
			version int64
			digest  string
			revoked int
			key     string
			value   string
		)
		if err := rows.Scan(&version, &digest, &revoked, &key, &value); err != nil {
			return nil, fmt.Errorf("scan rollout: %w", err)
		}
		if len(rollouts) == 0 || rollouts[len(rollouts)-1].Version != version {
			rollouts = append(rollouts, Rollout{
				ConfigName:        configName,
				TargetEnvironment: environment,
				Namespace:         namespace,
				Version:           version,
				Digest:            digest,
				Revoked:           revoked != 0,
			})
		}
		last := &rollouts[len(rollouts)-1]
		last.Entries = append(last.Entries, Entry{Key: key, Value: value})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rollouts rows: %w", err)
	}
	return rollouts, nil
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readRollout loads one rollout and its entries ordered by key.
func readRollout(ctx context.Context, q querier, configName, environment, namespace string, version int64) (Rollout, error) {
	rollout := Rollout{
		ConfigName:        configName,
		TargetEnvironment: environment,
		Namespace:         namespace,
		Version:           version,
	}
	var revoked int
	err := q.QueryRowContext(ctx, `
		SELECT digest, revoked FROM config_rollouts
		WHERE config_name = ? AND target_environment = ? AND namespace = ? AND version = ?`,
		configName, environment, namespace, version).Scan(&rollout.Digest, &revoked)
	if err != nil {
		return Rollout{}, fmt.Errorf("read rollout: %w", err)
	}
	rollout.Revoked = revoked != 0

	rows, err := q.QueryContext(ctx, `
		SELECT entry_key, entry_value FROM config_rollout_entries
		WHERE config_name = ? AND target_environment = ? AND namespace = ? AND version = ?
		ORDER BY entry_key ASC`,
		configName, environment, namespace, version)
	if err != nil {
		return Rollout{}, fmt.Errorf("read rollout entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry Entry
		if err := rows.Scan(&entry.Key, &entry.Value); err != nil {
			return Rollout{}, fmt.Errorf("scan rollout entry: %w", err)
		}
		rollout.Entries = append(rollout.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return Rollout{}, fmt.Errorf("read rollout entries rows: %w", err)
	}
	return rollout, nil
}
