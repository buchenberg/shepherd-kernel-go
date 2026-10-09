package shepherd

// SQLiteKVSubstrate (plan 03 §3): deterministic local key/value substrate
// backed by SQLite, porting shepherd2's SQLiteKVSubstrate.
//
// Separation law (checked against the reference): the KV store is a separate
// world-side database, not a table in the trace DB. The trace records what
// was applied; the world (the kv file) is what it was applied to. Keeping
// them apart is what makes the capture records a description rather than
// the state itself.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const (
	// KVSQLiteSubstrateRef mirrors KV_SQLITE_SUBSTRATE_REF.
	KVSQLiteSubstrateRef = "kv.sqlite.local.v1"
	// KVPutDeclarationSchema mirrors KV_PUT_DECLARATION_SCHEMA.
	KVPutDeclarationSchema = "shepherd2.kv.put.v1"
	// KVPutCaptureSchema mirrors KV_PUT_CAPTURE_SCHEMA.
	KVPutCaptureSchema = "shepherd2.kv.put.applied.v1"
)

// KVSubstrate is the SQLite-backed key/value substrate.
type KVSubstrate struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
}

// NewKVSubstrate opens (creating if needed) the world-side KV database at
// path. ":memory:" gives an in-memory store.
func NewKVSubstrate(path string) (*KVSubstrate, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open kv substrate: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value_json TEXT NOT NULL)"); err != nil {
		db.Close()
		return nil, fmt.Errorf("create kv table: %w", err)
	}
	return &KVSubstrate{path: path, db: db}, nil
}

// Close closes the underlying database.
func (k *KVSubstrate) Close() error { return k.db.Close() }

// Path returns the world-side database path.
func (k *KVSubstrate) Path() string { return k.path }

// Get returns the decoded value stored under key, or nil when absent.
//
// The value is decoded with UseNumber for the same reason retained bodies
// are: a stored 42 must read back as 42, not 42.0, or a later digest over
// the value would not match the capture record that describes it.
func (k *KVSubstrate) Get(key string) (any, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var valueJSON string
	err := k.db.QueryRow("SELECT value_json FROM kv WHERE key = ?", key).Scan(&valueJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kv get %q: %w", key, err)
	}
	dec := json.NewDecoder(strings.NewReader(valueJSON))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("kv get %q: decode value: %w", key, err)
	}
	return value, nil
}

func (k *KVSubstrate) SubstrateRef() string         { return KVSQLiteSubstrateRef }
func (k *KVSubstrate) DeclarationSchemas() []string { return []string{KVPutDeclarationSchema} }
func (k *KVSubstrate) CaptureSchemas() []string     { return []string{KVPutCaptureSchema} }
func (k *KVSubstrate) Containment() Containment     { return ContainContained }

// Materialize applies each kv put declaration to the world-side store and
// returns one applied capture per put. The whole batch is one transaction:
// either every put lands (and every capture describes a landed put) or none
// does.
func (k *KVSubstrate) Materialize(_ context.Context, records []Record) (MaterializationResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	tx, err := k.db.Begin()
	if err != nil {
		return MaterializationResult{}, fmt.Errorf("kv substrate begin: %w", err)
	}

	captures := make([]RecordDraft, 0, len(records))
	anchors := make([]map[string]any, 0, len(records))
	for _, record := range records {
		key, value, err := kvPutPayload(record)
		if err != nil {
			tx.Rollback()
			return MaterializationResult{}, err
		}
		valueJSON, err := canonicalJSONASCIIBytes(value)
		if err != nil {
			tx.Rollback()
			return MaterializationResult{}, &SubstrateError{fmt.Sprintf("kv put value for key %q is not serializable", key)}
		}
		if _, err := tx.Exec(
			"INSERT INTO kv(key, value_json) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json",
			key, string(valueJSON),
		); err != nil {
			tx.Rollback()
			return MaterializationResult{}, fmt.Errorf("kv put %q: %w", key, err)
		}
		captures = append(captures, RecordDraft{
			Mode:            Capture,
			SchemaRef:       KVPutCaptureSchema,
			KindLabel:       "kv_put_applied",
			Payload:         map[string]any{"key": key, "value": value},
			CausedByFactIDs: []string{record.Envelope.RecordID},
		})
		anchors = append(anchors, map[string]any{
			"kind":          "kv_key",
			"key":           key,
			"substrate_ref": k.SubstrateRef(),
		})
	}

	if err := tx.Commit(); err != nil {
		return MaterializationResult{}, fmt.Errorf("kv substrate commit: %w", err)
	}
	return MaterializationResult{
		Outcome:          MaterializationSuccess,
		CaptureDrafts:    captures,
		WorldSideAnchors: anchors,
	}, nil
}

// kvPutPayload mirrors _kv_put_payload: a non-empty string key and a value
// must be present; anything else is a substrate error, which fails the whole
// batch before any write lands.
func kvPutPayload(record Record) (string, any, error) {
	key, _ := record.Body.Payload["key"].(string)
	if key == "" {
		return "", nil, &SubstrateError{"kv put declarations require a non-empty string key"}
	}
	value, ok := record.Body.Payload["value"]
	if !ok {
		return "", nil, &SubstrateError{"kv put declarations require a value"}
	}
	return key, value, nil
}
