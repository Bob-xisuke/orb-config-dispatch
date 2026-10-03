package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Entry is a single key/value pair carried by a rollout.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Rollout is one immutable config revision identified by config name, target
// environment, namespace and version.
type Rollout struct {
	ConfigName        string  `json:"configName"`
	TargetEnvironment string  `json:"targetEnvironment"`
	Namespace         string  `json:"namespace"`
	Version           int64   `json:"version"`
	Entries           []Entry `json:"entries"`
	Digest            string  `json:"digest"`
	Revoked           bool    `json:"revoked"`
}

// ErrConflict reports that a rollout with the same identity but a different
// content already exists.
var ErrConflict = errors.New("config rollout conflict")

// CanonicalEntries returns a copy of entries sorted by key. Entries with the
// same set of pairs compare equal regardless of their input order.
func CanonicalEntries(entries []Entry) []Entry {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	return sorted
}

// Digest computes the content digest of an entry set. Keys are length-prefixed
// so delimiters inside values cannot make two different sets collide, and the
// entries are first sorted so order never changes the result.
func Digest(entries []Entry) string {
	var length [4]byte
	hash := sha256.New()
	write := func(value string) {
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	for _, entry := range CanonicalEntries(entries) {
		write(entry.Key)
		write(entry.Value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Register stores a rollout inside a single transaction. The identity columns
// carry a UNIQUE constraint, so concurrent commits cannot create duplicates: a
// submission with the same content returns the existing record, and one with a
// different content fails with ErrConflict. The input is normalized in place
// (sorted entries and digest).
func (s *Store) Register(ctx context.Context, rollout *Rollout) (*Rollout, error) {
	entries := CanonicalEntries(rollout.Entries)
	digest := Digest(entries)

	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("begin rollout tx: %w", err)
		}

		storedEntries, storedDigest, found, err := lookupRollout(ctx, tx, rollout.ConfigName, rollout.TargetEnvironment, rollout.Namespace, rollout.Version)
		if err != nil {
			rollback(tx)
			return nil, err
		}
		if found {
			rollback(tx)
			if storedDigest == digest {
				rollout.Entries = storedEntries
				rollout.Digest = storedDigest
				rollout.Revoked = false
				return rollout, nil
			}
			return nil, ErrConflict
		}

		entriesJSON, err := json.Marshal(entries)
		if err != nil {
			rollback(tx)
			return nil, fmt.Errorf("encode entries: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO config_rollouts (config_name, target_environment, namespace, version, entries, digest)
VALUES (?, ?, ?, ?, ?, ?)`,
			rollout.ConfigName, rollout.TargetEnvironment, rollout.Namespace, rollout.Version, string(entriesJSON), digest); err != nil {
			rollback(tx)
			if isUniqueConstraint(err) {
				// A concurrent transaction won the race for this identity; re-read it.
				continue
			}
			return nil, fmt.Errorf("insert rollout: %w", err)
		}
		if err := tx.Commit(); err != nil {
			if isRetryableCommit(err) {
				continue
			}
			return nil, fmt.Errorf("commit rollout: %w", err)
		}

		rollout.Entries = entries
		rollout.Digest = digest
		rollout.Revoked = false
		return rollout, nil
	}
	return nil, fmt.Errorf("register rollout: exhausted retries under concurrent writes")
}

// List returns every rollout matching the environment/namespace/config query,
// ordered by ascending version. Readers run in WAL mode and only see committed
// rows.
func (s *Store) List(ctx context.Context, targetEnvironment, namespace, configName string) ([]Rollout, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT config_name, target_environment, namespace, version, entries, digest, revoked
FROM config_rollouts
WHERE target_environment = ? AND namespace = ? AND config_name = ?
ORDER BY version ASC`, targetEnvironment, namespace, configName)
	if err != nil {
		return nil, fmt.Errorf("query rollouts: %w", err)
	}
	defer rows.Close()

	var rollouts []Rollout
	for rows.Next() {
		var rollout Rollout
		var entriesJSON string
		var revoked int64
		if err := rows.Scan(&rollout.ConfigName, &rollout.TargetEnvironment, &rollout.Namespace, &rollout.Version, &entriesJSON, &rollout.Digest, &revoked); err != nil {
			return nil, fmt.Errorf("scan rollout: %w", err)
		}
		if err := json.Unmarshal([]byte(entriesJSON), &rollout.Entries); err != nil {
			return nil, fmt.Errorf("decode stored entries: %w", err)
		}
		rollout.Revoked = revoked != 0
		rollouts = append(rollouts, rollout)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rollouts: %w", err)
	}
	return rollouts, nil
}

func lookupRollout(ctx context.Context, tx *sql.Tx, configName, targetEnvironment, namespace string, version int64) ([]Entry, string, bool, error) {
	var entriesJSON, digest string
	err := tx.QueryRowContext(ctx, `
SELECT entries, digest
FROM config_rollouts
WHERE config_name = ? AND target_environment = ? AND namespace = ? AND version = ?`,
		configName, targetEnvironment, namespace, version).Scan(&entriesJSON, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("lookup rollout: %w", err)
	}
	var entries []Entry
	if err := json.Unmarshal([]byte(entriesJSON), &entries); err != nil {
		return nil, "", false, fmt.Errorf("decode stored entries: %w", err)
	}
	return entries, digest, true, nil
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func isUniqueConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func isRetryableCommit(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) &&
		(sqliteErr.Code() == sqlite3.SQLITE_BUSY || sqliteErr.Code() == sqlite3.SQLITE_BUSY_SNAPSHOT)
}
