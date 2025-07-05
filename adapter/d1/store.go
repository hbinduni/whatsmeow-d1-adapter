package d1

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/exsync"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/hbinduni/whatsmeow-d1-adapter/internal/db"
)

// ErrInvalidLength is returned by some database getters if the database returned a byte array with an unexpected length.
var ErrInvalidLength = errors.New("database returned byte array with illegal length")

// D1Store implements all Whatsmeow store interfaces for Cloudflare D1
type D1Store struct {
	container *Container
	JID       string
	log       waLog.Logger

	preKeyLock sync.Mutex

	contactCache     map[types.JID]*types.ContactInfo
	contactCacheLock sync.Mutex

	migratedPNSessionsCache *exsync.Set[string]
	
	// Performance optimizations
	identityCache     map[string][32]byte // Cache for identity keys
	identityCacheLock sync.RWMutex
	sessionCache      map[string][]byte // Cache for sessions
	sessionCacheLock  sync.RWMutex
	batchWriteQueue   []batchOperation // Queue for batch writes
	batchWriteLock    sync.Mutex
}

// batchOperation represents a batched database operation
type batchOperation struct {
	query string
	args  []any
	type_ string // "insert", "update", "delete"
}

// NewD1Store creates a new D1Store with the given container and user JID
func NewD1Store(container *Container, jid types.JID) *D1Store {
	store := &D1Store{
		container:    container,
		JID:          jid.String(),
		log:          container.log,
		contactCache: make(map[types.JID]*types.ContactInfo),

		migratedPNSessionsCache: exsync.NewSet[string](),
		
		// Initialize performance optimization caches
		identityCache:   make(map[string][32]byte),
		sessionCache:    make(map[string][]byte),
		batchWriteQueue: make([]batchOperation, 0),
	}
	
	// Start periodic batch flush
	go store.batchFlushWorker()
	
	return store
}

// batchFlushWorker periodically flushes batched operations
func (s *D1Store) batchFlushWorker() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for range ticker.C {
		s.flushBatchOperations()
	}
}

// flushBatchOperations flushes pending batch operations
func (s *D1Store) flushBatchOperations() {
	s.batchWriteLock.Lock()
	if len(s.batchWriteQueue) == 0 {
		s.batchWriteLock.Unlock()
		return
	}
	
	operations := make([]batchOperation, len(s.batchWriteQueue))
	copy(operations, s.batchWriteQueue)
	s.batchWriteQueue = s.batchWriteQueue[:0] // Clear queue
	s.batchWriteLock.Unlock()
	
	// Execute operations
	for _, op := range operations {
		_, err := s.db().Exec(context.Background(), op.query, op.args...)
		if err != nil {
			s.log.Warnf("Batch operation failed: %v", err)
		}
	}
}

// addToBatch adds an operation to the batch queue
func (s *D1Store) addToBatch(query string, args []any, opType string) {
	s.batchWriteLock.Lock()
	defer s.batchWriteLock.Unlock()
	
	s.batchWriteQueue = append(s.batchWriteQueue, batchOperation{
		query: query,
		args:  args,
		type_: opType,
	})
	
	// Auto-flush if queue is getting large
	if len(s.batchWriteQueue) >= 20 {
		go s.flushBatchOperations()
	}
}

// FlushAll flushes all pending operations and clears caches
func (s *D1Store) FlushAll() error {
	// Flush batch operations
	s.flushBatchOperations()
	
	// Clear caches (optional - can help with memory usage)
	s.identityCacheLock.Lock()
	s.identityCache = make(map[string][32]byte)
	s.identityCacheLock.Unlock()
	
	s.sessionCacheLock.Lock()
	s.sessionCache = make(map[string][]byte)
	s.sessionCacheLock.Unlock()
	
	s.contactCacheLock.Lock()
	s.contactCache = make(map[types.JID]*types.ContactInfo)
	s.contactCacheLock.Unlock()
	
	return nil
}

// GetCacheStats returns cache statistics for monitoring
func (s *D1Store) GetCacheStats() map[string]int {
	s.identityCacheLock.RLock()
	identityCount := len(s.identityCache)
	s.identityCacheLock.RUnlock()
	
	s.sessionCacheLock.RLock()
	sessionCount := len(s.sessionCache)
	s.sessionCacheLock.RUnlock()
	
	s.contactCacheLock.Lock()
	contactCount := len(s.contactCache)
	s.contactCacheLock.Unlock()
	
	s.batchWriteLock.Lock()
	batchCount := len(s.batchWriteQueue)
	s.batchWriteLock.Unlock()
	
	return map[string]int{
		"identity_cache": identityCount,
		"session_cache":  sessionCount,
		"contact_cache":  contactCount,
		"batch_queue":    batchCount,
	}
}

// Verify that D1Store implements all required interfaces
var _ store.AllSessionSpecificStores = (*D1Store)(nil)
var _ store.AllGlobalStores = (*D1Store)(nil)

// Database access helper
func (s *D1Store) db() *db.D1Database {
	return s.container.db
}

// IdentityStore implementation

const (
	putIdentityQuery = `
		INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity) VALUES (?, ?, ?)
		ON CONFLICT (our_jid, their_id) DO UPDATE SET identity=excluded.identity
	`
	deleteAllIdentitiesQuery = `DELETE FROM whatsmeow_identity_keys WHERE our_jid=? AND their_id LIKE ?`
	deleteIdentityQuery      = `DELETE FROM whatsmeow_identity_keys WHERE our_jid=? AND their_id=?`
	getIdentityQuery         = `SELECT identity FROM whatsmeow_identity_keys WHERE our_jid=? AND their_id=?`
)

func (s *D1Store) PutIdentity(ctx context.Context, address string, key [32]byte) error {
	// Update cache first
	cacheKey := s.JID + ":" + address
	s.identityCacheLock.Lock()
	s.identityCache[cacheKey] = key
	s.identityCacheLock.Unlock()
	
	// Add to batch queue for async database update
	s.addToBatch(putIdentityQuery, []any{s.JID, address, key[:]}, "upsert")
	return nil
}

func (s *D1Store) DeleteAllIdentities(ctx context.Context, phone string) error {
	_, err := s.db().Exec(ctx, deleteAllIdentitiesQuery, s.JID, phone+":%")
	return err
}

func (s *D1Store) DeleteIdentity(ctx context.Context, address string) error {
	cacheKey := s.JID + ":" + address
	
	// Remove from cache
	s.identityCacheLock.Lock()
	delete(s.identityCache, cacheKey)
	s.identityCacheLock.Unlock()
	
	// Add to batch queue for async database update
	s.addToBatch(deleteIdentityQuery, []any{s.JID, address}, "delete")
	return nil
}

func (s *D1Store) IsTrustedIdentity(ctx context.Context, address string, key [32]byte) (bool, error) {
	cacheKey := s.JID + ":" + address
	
	// Check cache first
	s.identityCacheLock.RLock()
	if cachedKey, exists := s.identityCache[cacheKey]; exists {
		s.identityCacheLock.RUnlock()
		return cachedKey == key, nil
	}
	s.identityCacheLock.RUnlock()
	
	// Cache miss, query database
	var existingIdentity []byte
	err := s.db().QueryRow(ctx, getIdentityQuery, s.JID, address).Scan(&existingIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		// Trust if not known, it'll be saved automatically later
		return true, nil
	} else if err != nil {
		return false, err
	} else if len(existingIdentity) != 32 {
		return false, ErrInvalidLength
	}
	
	// Update cache
	existingKey := *(*[32]byte)(existingIdentity)
	s.identityCacheLock.Lock()
	s.identityCache[cacheKey] = existingKey
	s.identityCacheLock.Unlock()
	
	return existingKey == key, nil
}

// SessionStore implementation

const (
	getSessionQuery = `SELECT session FROM whatsmeow_sessions WHERE our_jid=? AND their_id=?`
	hasSessionQuery = `SELECT true FROM whatsmeow_sessions WHERE our_jid=? AND their_id=?`
	putSessionQuery = `
		INSERT INTO whatsmeow_sessions (our_jid, their_id, session) VALUES (?, ?, ?)
		ON CONFLICT (our_jid, their_id) DO UPDATE SET session=excluded.session
	`
	deleteAllSessionsQuery = `DELETE FROM whatsmeow_sessions WHERE our_jid=? AND their_id LIKE ?`
	deleteSessionQuery     = `DELETE FROM whatsmeow_sessions WHERE our_jid=? AND their_id=?`

	migratePNToLIDSessionsQuery = `
		INSERT INTO whatsmeow_sessions (our_jid, their_id, session)
		SELECT our_jid, replace(their_id, ?, ?), session
		FROM whatsmeow_sessions
		WHERE our_jid=? AND their_id LIKE ? || ':%'
		ON CONFLICT (our_jid, their_id) DO UPDATE SET session=excluded.session
	`
	deleteAllIdentityKeysQuery      = `DELETE FROM whatsmeow_identity_keys WHERE our_jid=? AND their_id LIKE ?`
	migratePNToLIDIdentityKeysQuery = `
		INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity)
		SELECT our_jid, replace(their_id, ?, ?), identity
		FROM whatsmeow_identity_keys
		WHERE our_jid=? AND their_id LIKE ? || ':%'
		ON CONFLICT (our_jid, their_id) DO UPDATE SET identity=excluded.identity
	`
	deleteAllSenderKeysQuery      = `DELETE FROM whatsmeow_sender_keys WHERE our_jid=? AND sender_id LIKE ?`
	migratePNToLIDSenderKeysQuery = `
		INSERT INTO whatsmeow_sender_keys (our_jid, chat_id, sender_id, sender_key)
		SELECT our_jid, chat_id, replace(sender_id, ?, ?), sender_key
		FROM whatsmeow_sender_keys
		WHERE our_jid=? AND sender_id LIKE ? || ':%'
		ON CONFLICT (our_jid, chat_id, sender_id) DO UPDATE SET sender_key=excluded.sender_key
	`
)

func (s *D1Store) GetSession(ctx context.Context, address string) (session []byte, err error) {
	cacheKey := s.JID + ":" + address
	
	// Check cache first
	s.sessionCacheLock.RLock()
	if cachedSession, exists := s.sessionCache[cacheKey]; exists {
		s.sessionCacheLock.RUnlock()
		return cachedSession, nil
	}
	s.sessionCacheLock.RUnlock()
	
	// Cache miss, query database
	err = s.db().QueryRow(ctx, getSessionQuery, s.JID, address).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	
	// Update cache
	s.sessionCacheLock.Lock()
	s.sessionCache[cacheKey] = session
	s.sessionCacheLock.Unlock()
	
	return session, nil
}

func (s *D1Store) HasSession(ctx context.Context, address string) (has bool, err error) {
	err = s.db().QueryRow(ctx, hasSessionQuery, s.JID, address).Scan(&has)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *D1Store) PutSession(ctx context.Context, address string, session []byte) error {
	cacheKey := s.JID + ":" + address
	
	// Update cache first
	s.sessionCacheLock.Lock()
	s.sessionCache[cacheKey] = session
	s.sessionCacheLock.Unlock()
	
	// Add to batch queue for async database update
	s.addToBatch(putSessionQuery, []any{s.JID, address, session}, "upsert")
	return nil
}

func (s *D1Store) DeleteAllSessions(ctx context.Context, phone string) error {
	return s.deleteAllSessions(ctx, phone)
}

func (s *D1Store) deleteAllSessions(ctx context.Context, phone string) error {
	_, err := s.db().Exec(ctx, deleteAllSessionsQuery, s.JID, phone+":%")
	return err
}

func (s *D1Store) deleteAllSenderKeys(ctx context.Context, phone string) error {
	_, err := s.db().Exec(ctx, deleteAllSenderKeysQuery, s.JID, phone+":%")
	return err
}

func (s *D1Store) deleteAllIdentityKeys(ctx context.Context, phone string) error {
	_, err := s.db().Exec(ctx, deleteAllIdentityKeysQuery, s.JID, phone+":%")
	return err
}

func (s *D1Store) DeleteSession(ctx context.Context, address string) error {
	cacheKey := s.JID + ":" + address
	
	// Remove from cache
	s.sessionCacheLock.Lock()
	delete(s.sessionCache, cacheKey)
	s.sessionCacheLock.Unlock()
	
	// Add to batch queue for async database update
	s.addToBatch(deleteSessionQuery, []any{s.JID, address}, "delete")
	return nil
}

func (s *D1Store) MigratePNToLID(ctx context.Context, pn, lid types.JID) error {
	pnSignal := pn.SignalAddressUser()
	if !s.migratedPNSessionsCache.Add(pnSignal) {
		return nil
	}
	var sessionsUpdated, identityKeysUpdated, senderKeysUpdated int64
	lidSignal := lid.SignalAddressUser()
	err := s.db().DoTxn(ctx, nil, func(ctx context.Context) error {
		res, err := s.db().Exec(ctx, migratePNToLIDSessionsQuery, pnSignal, lidSignal, s.JID, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to migrate sessions: %w", err)
		}
		sessionsUpdated, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected for sessions: %w", err)
		}
		err = s.deleteAllSessions(ctx, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to delete extra sessions: %w", err)
		}

		res, err = s.db().Exec(ctx, migratePNToLIDIdentityKeysQuery, pnSignal, lidSignal, s.JID, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to migrate identity keys: %w", err)
		}
		identityKeysUpdated, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected for identity keys: %w", err)
		}
		err = s.deleteAllIdentityKeys(ctx, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to delete extra identity keys: %w", err)
		}

		res, err = s.db().Exec(ctx, migratePNToLIDSenderKeysQuery, pnSignal, lidSignal, s.JID, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to migrate sender keys: %w", err)
		}
		senderKeysUpdated, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("failed to get rows affected for sender keys: %w", err)
		}
		err = s.deleteAllSenderKeys(ctx, pnSignal)
		if err != nil {
			return fmt.Errorf("failed to delete extra sender keys: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sessionsUpdated > 0 || senderKeysUpdated > 0 || identityKeysUpdated > 0 {
		s.log.Infof("Migrated %d sessions, %d identity keys and %d sender keys from %s to %s", sessionsUpdated, identityKeysUpdated, senderKeysUpdated, pnSignal, lidSignal)
	} else {
		s.log.Debugf("No sessions or sender keys found to migrate from %s to %s", pnSignal, lidSignal)
	}
	return nil
}

// PreKeyStore implementation

const (
	getLastPreKeyIDQuery        = `SELECT MAX(key_id) FROM whatsmeow_pre_keys WHERE jid=?`
	insertPreKeyQuery           = `INSERT INTO whatsmeow_pre_keys (jid, key_id, key, uploaded) VALUES (?, ?, ?, ?)`
	getUnuploadedPreKeysQuery   = `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=? AND uploaded=false ORDER BY key_id LIMIT ?`
	getPreKeyQuery              = `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=? AND key_id=?`
	deletePreKeyQuery           = `DELETE FROM whatsmeow_pre_keys WHERE jid=? AND key_id=?`
	markPreKeysAsUploadedQuery  = `UPDATE whatsmeow_pre_keys SET uploaded=true WHERE jid=? AND key_id<=?`
	getUploadedPreKeyCountQuery = `SELECT COUNT(*) FROM whatsmeow_pre_keys WHERE jid=? AND uploaded=true`
)

func (s *D1Store) genOnePreKey(ctx context.Context, id uint32, markUploaded bool) (*keys.PreKey, error) {
	key := keys.NewPreKey(id)
	_, err := s.db().Exec(ctx, insertPreKeyQuery, s.JID, key.KeyID, key.Priv[:], markUploaded)
	return key, err
}

func (s *D1Store) getNextPreKeyID(ctx context.Context) (uint32, error) {
	var lastKeyID sql.NullInt32
	err := s.db().QueryRow(ctx, getLastPreKeyIDQuery, s.JID).Scan(&lastKeyID)
	if err != nil {
		return 0, fmt.Errorf("failed to query next prekey ID: %w", err)
	}
	return uint32(lastKeyID.Int32) + 1, nil
}

func (s *D1Store) GenOnePreKey(ctx context.Context) (*keys.PreKey, error) {
	s.preKeyLock.Lock()
	defer s.preKeyLock.Unlock()
	nextKeyID, err := s.getNextPreKeyID(ctx)
	if err != nil {
		return nil, err
	}
	return s.genOnePreKey(ctx, nextKeyID, true)
}

func (s *D1Store) GetOrGenPreKeys(ctx context.Context, count uint32) ([]*keys.PreKey, error) {
	s.preKeyLock.Lock()
	defer s.preKeyLock.Unlock()

	res, err := s.db().Query(ctx, getUnuploadedPreKeysQuery, s.JID, count)
	if err != nil {
		return nil, fmt.Errorf("failed to query existing prekeys: %w", err)
	}
	newKeys := make([]*keys.PreKey, count)
	var existingCount uint32
	for res.Next() {
		var key *keys.PreKey
		key, err = scanPreKey(res)
		if err != nil {
			return nil, err
		} else if key != nil {
			newKeys[existingCount] = key
			existingCount++
		}
	}

	if existingCount < uint32(len(newKeys)) {
		var nextKeyID uint32
		nextKeyID, err = s.getNextPreKeyID(ctx)
		if err != nil {
			return nil, err
		}
		for i := existingCount; i < count; i++ {
			newKeys[i], err = s.genOnePreKey(ctx, nextKeyID, false)
			if err != nil {
				return nil, fmt.Errorf("failed to generate prekey: %w", err)
			}
			nextKeyID++
		}
	}

	return newKeys, nil
}

func scanPreKey(row dbutil.Scannable) (*keys.PreKey, error) {
	var priv []byte
	var id uint32
	err := row.Scan(&id, &priv)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else if len(priv) != 32 {
		return nil, ErrInvalidLength
	}
	return &keys.PreKey{
		KeyPair: *keys.NewKeyPairFromPrivateKey(*(*[32]byte)(priv)),
		KeyID:   id,
	}, nil
}

func (s *D1Store) GetPreKey(ctx context.Context, id uint32) (*keys.PreKey, error) {
	return scanPreKey(s.db().QueryRow(ctx, getPreKeyQuery, s.JID, id))
}

func (s *D1Store) RemovePreKey(ctx context.Context, id uint32) error {
	_, err := s.db().Exec(ctx, deletePreKeyQuery, s.JID, id)
	return err
}

func (s *D1Store) MarkPreKeysAsUploaded(ctx context.Context, upToID uint32) error {
	_, err := s.db().Exec(ctx, markPreKeysAsUploadedQuery, s.JID, upToID)
	return err
}

func (s *D1Store) UploadedPreKeyCount(ctx context.Context) (count int, err error) {
	err = s.db().QueryRow(ctx, getUploadedPreKeyCountQuery, s.JID).Scan(&count)
	return
}

// SenderKeyStore implementation

const (
	getSenderKeyQuery = `SELECT sender_key FROM whatsmeow_sender_keys WHERE our_jid=? AND chat_id=? AND sender_id=?`
	putSenderKeyQuery = `
		INSERT INTO whatsmeow_sender_keys (our_jid, chat_id, sender_id, sender_key) VALUES (?, ?, ?, ?)
		ON CONFLICT (our_jid, chat_id, sender_id) DO UPDATE SET sender_key=excluded.sender_key
	`
)

func (s *D1Store) PutSenderKey(ctx context.Context, group, user string, session []byte) error {
	_, err := s.db().Exec(ctx, putSenderKeyQuery, s.JID, group, user, session)
	return err
}

func (s *D1Store) GetSenderKey(ctx context.Context, group, user string) (key []byte, err error) {
	err = s.db().QueryRow(ctx, getSenderKeyQuery, s.JID, group, user).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
