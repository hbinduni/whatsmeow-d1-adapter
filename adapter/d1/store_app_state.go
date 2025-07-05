package d1

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.mau.fi/whatsmeow/store"
)

// AppStateSyncKeyStore implementation

const (
	putAppStateSyncKeyQuery = `
		INSERT INTO whatsmeow_app_state_sync_keys (jid, key_id, key_data, timestamp, fingerprint) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (jid, key_id) DO UPDATE
			SET key_data=excluded.key_data, timestamp=excluded.timestamp, fingerprint=excluded.fingerprint
			WHERE excluded.timestamp > whatsmeow_app_state_sync_keys.timestamp
	`
	getAppStateSyncKeyQuery         = `SELECT key_data, timestamp, fingerprint FROM whatsmeow_app_state_sync_keys WHERE jid=? AND key_id=?`
	getLatestAppStateSyncKeyIDQuery = `SELECT key_id FROM whatsmeow_app_state_sync_keys WHERE jid=? ORDER BY timestamp DESC LIMIT 1`
)

func (s *D1Store) PutAppStateSyncKey(ctx context.Context, id []byte, key store.AppStateSyncKey) error {
	_, err := s.db().Exec(ctx, putAppStateSyncKeyQuery, s.JID, id, key.Data, key.Timestamp, key.Fingerprint)
	return err
}

func (s *D1Store) GetAppStateSyncKey(ctx context.Context, id []byte) (*store.AppStateSyncKey, error) {
	var key store.AppStateSyncKey
	err := s.db().QueryRow(ctx, getAppStateSyncKeyQuery, s.JID, id).Scan(&key.Data, &key.Timestamp, &key.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &key, err
}

func (s *D1Store) GetLatestAppStateSyncKeyID(ctx context.Context) ([]byte, error) {
	var keyID []byte
	err := s.db().QueryRow(ctx, getLatestAppStateSyncKeyIDQuery, s.JID).Scan(&keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return keyID, err
}

// AppStateStore implementation

const (
	putAppStateVersionQuery = `
		INSERT INTO whatsmeow_app_state_version (jid, name, version, hash) VALUES (?, ?, ?, ?)
		ON CONFLICT (jid, name) DO UPDATE SET version=excluded.version, hash=excluded.hash
	`
	getAppStateVersionQuery                = `SELECT version, hash FROM whatsmeow_app_state_version WHERE jid=? AND name=?`
	deleteAppStateVersionQuery             = `DELETE FROM whatsmeow_app_state_version WHERE jid=? AND name=?`
	putAppStateMutationMACsQuery           = `INSERT INTO whatsmeow_app_state_mutation_macs (jid, name, version, index_mac, value_mac) VALUES `
	deleteAppStateMutationMACsQueryGeneric = `DELETE FROM whatsmeow_app_state_mutation_macs WHERE jid=? AND name=? AND index_mac IN `
	getAppStateMutationMACQuery            = `SELECT value_mac FROM whatsmeow_app_state_mutation_macs WHERE jid=? AND name=? AND index_mac=? ORDER BY version DESC LIMIT 1`
)

func (s *D1Store) PutAppStateVersion(ctx context.Context, name string, version uint64, hash [128]byte) error {
	_, err := s.db().Exec(ctx, putAppStateVersionQuery, s.JID, name, version, hash[:])
	return err
}

func (s *D1Store) GetAppStateVersion(ctx context.Context, name string) (version uint64, hash [128]byte, err error) {
	var uncheckedHash []byte
	var versionRaw interface{}
	
	// Use QueryRowDirect to get the raw result and handle D1's type conversions
	row, err := s.db().QueryRowDirect(ctx, getAppStateVersionQuery, s.JID, name)
	if errors.Is(err, sql.ErrNoRows) {
		// version will be 0 and hash will be an empty array, which is the correct initial state
		return 0, [128]byte{}, nil
	} else if err != nil {
		return 0, [128]byte{}, err
	}
	
	// Extract values from the row map with proper type handling
	versionRaw = row["version"]
	hashRaw := row["hash"]
	
	// Convert version to uint64
	if f, ok := versionRaw.(float64); ok {
		version = uint64(f)
	} else {
		return 0, [128]byte{}, errors.New("invalid version type from D1")
	}
	
	// Convert hash with D1's BLOB handling
	if hashBytes, ok := hashRaw.([]byte); ok {
		uncheckedHash = hashBytes
	} else if arr, ok := hashRaw.([]interface{}); ok {
		// D1 sometimes returns BLOB data as array of integers
		uncheckedHash = make([]byte, len(arr))
		for i, v := range arr {
			if f, ok := v.(float64); ok {
				uncheckedHash[i] = byte(f)
			} else {
				return 0, [128]byte{}, errors.New("invalid hash byte value from D1")
			}
		}
	} else {
		return 0, [128]byte{}, errors.New("invalid hash type from D1")
	}
	
	if len(uncheckedHash) != 128 {
		return 0, [128]byte{}, ErrInvalidLength
	}
	
	// Convert hash slice to array
	hash = *(*[128]byte)(uncheckedHash)
	return version, hash, nil
}

func (s *D1Store) DeleteAppStateVersion(ctx context.Context, name string) error {
	_, err := s.db().Exec(ctx, deleteAppStateVersionQuery, s.JID, name)
	return err
}

func (s *D1Store) putAppStateMutationMACs(ctx context.Context, name string, version uint64, mutations []store.AppStateMutationMAC) error {
	values := make([]any, 3+len(mutations)*2)
	queryParts := make([]string, len(mutations))
	values[0] = s.JID
	values[1] = name
	values[2] = version
	placeholderSyntax := "(?, ?, ?, ?, ?)"
	for i, mutation := range mutations {
		baseIndex := 3 + i*2
		values[baseIndex] = mutation.IndexMAC
		values[baseIndex+1] = mutation.ValueMAC
		queryParts[i] = placeholderSyntax
	}
	query := putAppStateMutationMACsQuery + strings.Join(queryParts, ",")

	// Build the full parameter list for the query
	finalValues := make([]any, len(mutations)*5)
	for i := 0; i < len(mutations); i++ {
		baseIndex := i * 5
		finalValues[baseIndex] = s.JID
		finalValues[baseIndex+1] = name
		finalValues[baseIndex+2] = version
		finalValues[baseIndex+3] = mutations[i].IndexMAC
		finalValues[baseIndex+4] = mutations[i].ValueMAC
	}

	_, err := s.db().Exec(ctx, query, finalValues...)
	return err
}

const mutationBatchSize = 400

func (s *D1Store) PutAppStateMutationMACs(ctx context.Context, name string, version uint64, mutations []store.AppStateMutationMAC) error {
	if len(mutations) == 0 {
		return nil
	}
	return s.db().DoTxn(ctx, nil, func(ctx context.Context) error {
		for i := 0; i < len(mutations); i += mutationBatchSize {
			end := i + mutationBatchSize
			if end > len(mutations) {
				end = len(mutations)
			}
			slice := mutations[i:end]
			err := s.putAppStateMutationMACs(ctx, name, version, slice)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *D1Store) DeleteAppStateMutationMACs(ctx context.Context, name string, indexMACs [][]byte) (err error) {
	if len(indexMACs) == 0 {
		return
	}
	args := make([]any, 2+len(indexMACs))
	args[0] = s.JID
	args[1] = name
	queryParts := make([]string, len(indexMACs))
	for i, item := range indexMACs {
		args[2+i] = item
		queryParts[i] = "?"
	}
	query := deleteAppStateMutationMACsQueryGeneric + "(" + strings.Join(queryParts, ",") + ")"
	_, err = s.db().Exec(ctx, query, args...)
	return
}

func (s *D1Store) GetAppStateMutationMAC(ctx context.Context, name string, indexMAC []byte) (valueMAC []byte, err error) {
	err = s.db().QueryRow(ctx, getAppStateMutationMACQuery, s.JID, name, indexMAC).Scan(&valueMAC)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
