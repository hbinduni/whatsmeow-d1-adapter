package d1

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	mathRand "math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/random"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/hbinduni/whatsmeow-d1-adapter/internal/db"
)

//go:embed schema.sql
var schemaFS embed.FS

// Container manages D1 database connections and device storage
type Container struct {
	db         *db.D1Database
	log        waLog.Logger
	dialect    dbutil.Dialect
	deviceLock sync.RWMutex
	devices    map[types.JID]*store.Device
}

// D1ContainerConfig holds configuration for D1 container
type D1ContainerConfig struct {
	APIToken           string                // Cloudflare API token
	AccountID          string                // Cloudflare account ID
	DatabaseID         string                // D1 database ID
	Logger             waLog.Logger
	EnableOptimizations bool                 // Enable performance optimizations (default: true)
	RetryConfig        *db.RetryConfig       // Optional retry configuration
	BatchConfig        *db.BatchConfig       // Optional batch configuration  
	ClientConfig       *db.ClientConfig      // Optional HTTP client configuration
}

// NewContainer creates a new D1 container
func NewContainer(config D1ContainerConfig) (*Container, error) {
	// Set up optimized D1 configuration
	d1Config := db.D1Config{
		APIToken:   config.APIToken,
		AccountID:  config.AccountID,
		DatabaseID: config.DatabaseID,
		Logger:     config.Logger,
	}
	
	// Apply performance optimizations if enabled (default: true)
	if config.EnableOptimizations || (!config.EnableOptimizations && config.RetryConfig == nil && config.BatchConfig == nil && config.ClientConfig == nil) {
		// Set default optimized configurations
		if config.RetryConfig == nil {
			d1Config.RetryConfig = &db.RetryConfig{
				MaxRetries:    3,
				InitialDelay:  100 * time.Millisecond,
				MaxDelay:      5 * time.Second,
				BackoffFactor: 2.0,
			}
		} else {
			d1Config.RetryConfig = config.RetryConfig
		}
		
		if config.BatchConfig == nil {
			d1Config.BatchConfig = &db.BatchConfig{
				Enabled:      true,
				MaxSize:      50,
				FlushTimeout: 100 * time.Millisecond,
			}
		} else {
			d1Config.BatchConfig = config.BatchConfig
		}
		
		if config.ClientConfig == nil {
			d1Config.ClientConfig = &db.ClientConfig{
				Timeout:             30 * time.Second,
				MaxIdleConns:        100,
				MaxConnsPerHost:     10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			}
		} else {
			d1Config.ClientConfig = config.ClientConfig
		}
	} else {
		// Use provided configurations
		d1Config.RetryConfig = config.RetryConfig
		d1Config.BatchConfig = config.BatchConfig
		d1Config.ClientConfig = config.ClientConfig
	}
	
	d1db, err := db.NewD1Database(d1Config)
	if err != nil {
		return nil, fmt.Errorf("failed to create D1 database: %w", err)
	}

	container := &Container{
		db:      d1db,
		log:     config.Logger,
		dialect: dbutil.SQLite,
		devices: make(map[types.JID]*store.Device),
	}

	// Initialize database schema
	if err := container.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return container, nil
}

// initSchema initializes the database schema
func (c *Container) initSchema() error {
	schemaSQL, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("failed to read schema file: %w", err)
	}

	_, err = c.db.Exec(context.Background(), string(schemaSQL))
	if err != nil {
		return fmt.Errorf("failed to execute schema: %w", err)
	}

	c.log.Infof("D1 schema initialized successfully")
	return nil
}

// GetAllDevices returns all devices from the database
func (c *Container) GetAllDevices() ([]*store.Device, error) {
	c.deviceLock.RLock()
	defer c.deviceLock.RUnlock()

	devices := make([]*store.Device, 0, len(c.devices))
	for _, device := range c.devices {
		devices = append(devices, device)
	}

	if len(devices) == 0 {
		// Load devices from database
		results, err := c.db.QueryForResults(context.Background(), `SELECT jid FROM whatsmeow_device`)
		if err != nil {
			return nil, fmt.Errorf("failed to query devices: %w", err)
		}

		for _, row := range results {
			jidStr, ok := row["jid"].(string)
			if !ok {
				c.log.Warnf("Invalid JID type in database result")
				continue
			}

			jid, err := types.ParseJID(jidStr)
			if err != nil {
				c.log.Warnf("Failed to parse JID %s: %v", jidStr, err)
				continue
			}

			device, err := c.GetDevice(jid)
			if err != nil {
				c.log.Warnf("Failed to load device %s: %v", jid, err)
				continue
			}

			devices = append(devices, device)
		}
	}

	return devices, nil
}

// GetDevice gets a device from the database or creates a new one
func (c *Container) GetDevice(jid types.JID) (*store.Device, error) {
	c.deviceLock.RLock()
	device, exists := c.devices[jid]
	c.deviceLock.RUnlock()

	if exists {
		return device, nil
	}

	// Load device from database
	device, err := c.loadDevice(jid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Device doesn't exist, create new one
			return c.NewDevice(), nil
		}
		return nil, fmt.Errorf("failed to load device: %w", err)
	}

	c.deviceLock.Lock()
	c.devices[jid] = device
	c.deviceLock.Unlock()

	return device, nil
}

// NewDevice creates a new device
func (c *Container) NewDevice() *store.Device {
	d1store := NewD1Store(c, types.EmptyJID)
	device := &store.Device{
		Log:       c.log,
		Container: c,

		// Generate cryptographic keys
		NoiseKey:       keys.NewKeyPair(),
		IdentityKey:    keys.NewKeyPair(),
		RegistrationID: generateRegistrationID(),
		AdvSecretKey:   generateAdvSecretKey(),
	}

	// Create signed pre-key using identity key
	device.SignedPreKey = device.IdentityKey.CreateSignedPreKey(1)

	// Initialize stores
	c.initializeDevice(device, d1store)

	return device
}

// generateRegistrationID generates a random registration ID
func generateRegistrationID() uint32 {
	return mathRand.Uint32()
}

// generateAdvSecretKey generates a 32-byte secret key
func generateAdvSecretKey() []byte {
	return random.Bytes(32)
}

// initializeDevice initializes the device with store references
func (c *Container) initializeDevice(device *store.Device, d1store *D1Store) {
	device.Identities = d1store
	device.Sessions = d1store
	device.PreKeys = d1store
	device.SenderKeys = d1store
	device.AppStateKeys = d1store
	device.AppState = d1store
	device.Contacts = d1store
	device.ChatSettings = d1store
	device.MsgSecrets = d1store
	device.PrivacyTokens = d1store
	device.EventBuffer = d1store
	device.LIDs = d1store
}

// PutDevice saves a device to the database
func (c *Container) PutDevice(ctx context.Context, device *store.Device) error {
	jid := device.GetJID()
	if jid.IsEmpty() {
		return errors.New("device JID is empty")
	}

	query := `
		INSERT INTO whatsmeow_device (
			jid, lid, facebook_uuid, registration_id, noise_key, identity_key,
			signed_pre_key, signed_pre_key_id, signed_pre_key_sig,
			adv_key, adv_details, adv_account_sig, adv_account_sig_key, adv_device_sig,
			platform, business_name, push_name, lid_migration_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			lid = excluded.lid,
			facebook_uuid = excluded.facebook_uuid,
			registration_id = excluded.registration_id,
			noise_key = excluded.noise_key,
			identity_key = excluded.identity_key,
			signed_pre_key = excluded.signed_pre_key,
			signed_pre_key_id = excluded.signed_pre_key_id,
			signed_pre_key_sig = excluded.signed_pre_key_sig,
			adv_key = excluded.adv_key,
			adv_details = excluded.adv_details,
			adv_account_sig = excluded.adv_account_sig,
			adv_account_sig_key = excluded.adv_account_sig_key,
			adv_device_sig = excluded.adv_device_sig,
			platform = excluded.platform,
			business_name = excluded.business_name,
			push_name = excluded.push_name,
			lid_migration_ts = excluded.lid_migration_ts
	`

	var fbUUID sql.NullString
	if device.FacebookUUID != uuid.Nil {
		fbUUID.String = device.FacebookUUID.String()
		fbUUID.Valid = true
	}

	var advAccountSig, advAccountSigKey, advDeviceSig []byte
	var advDetails []byte
	if device.Account != nil {
		advAccountSig = device.Account.AccountSignature
		advAccountSigKey = device.Account.AccountSignatureKey
		advDeviceSig = device.Account.DeviceSignature
		advDetails = device.Account.GetDetails()
	}

	_, err := c.db.Exec(ctx, query,
		jid.String(),
		device.LID.String(),
		fbUUID,
		device.RegistrationID,
		device.NoiseKey.Priv[:],
		device.IdentityKey.Priv[:],
		device.SignedPreKey.Priv[:],
		device.SignedPreKey.KeyID,
		device.SignedPreKey.Signature[:],
		device.AdvSecretKey,
		advDetails,
		advAccountSig,
		advAccountSigKey,
		advDeviceSig,
		device.Platform,
		device.BusinessName,
		device.PushName,
		device.LIDMigrationTimestamp,
	)

	if err != nil {
		return fmt.Errorf("failed to insert/update device: %w", err)
	}

	c.deviceLock.Lock()
	c.devices[jid] = device
	c.deviceLock.Unlock()

	return nil
}

// DeleteDevice removes a device from the database
func (c *Container) DeleteDevice(ctx context.Context, device *store.Device) error {
	jid := device.GetJID()
	if jid.IsEmpty() {
		return errors.New("device JID is empty")
	}

	_, err := c.db.Exec(ctx, `DELETE FROM whatsmeow_device WHERE jid = ?`, jid.String())
	if err != nil {
		return fmt.Errorf("failed to delete device: %w", err)
	}

	c.deviceLock.Lock()
	delete(c.devices, jid)
	c.deviceLock.Unlock()

	return nil
}

// loadDevice loads a device from the database
func (c *Container) loadDevice(jid types.JID) (*store.Device, error) {
	var device store.Device
	var noiseKey, identityKey, signedPreKey, signedPreKeySig []byte
	var advKey, advDetails, advAccountSig, advAccountSigKey, advDeviceSig []byte
	var fbUUID sql.NullString
	var lid string
	var signedPreKeyID uint32

	query := `
		SELECT lid, facebook_uuid, registration_id, noise_key, identity_key,
			   signed_pre_key, signed_pre_key_id, signed_pre_key_sig,
			   adv_key, adv_details, adv_account_sig, adv_account_sig_key, adv_device_sig,
			   platform, business_name, push_name, lid_migration_ts
		FROM whatsmeow_device WHERE jid = ?
	`

	row, err := c.db.QueryRowDirect(context.Background(), query, jid.String())
	if err != nil {
		return nil, err
	}

	// Extract values from the row map
	lid = row["lid"].(string)
	if fbUUIDVal, ok := row["facebook_uuid"]; ok && fbUUIDVal != nil {
		fbUUID.String = fbUUIDVal.(string)
		fbUUID.Valid = true
	}
	device.RegistrationID = uint32(row["registration_id"].(float64))
	noiseKey = row["noise_key"].([]byte)
	identityKey = row["identity_key"].([]byte)
	signedPreKey = row["signed_pre_key"].([]byte)
	signedPreKeyID = uint32(row["signed_pre_key_id"].(float64))
	signedPreKeySig = row["signed_pre_key_sig"].([]byte)
	advKey = row["adv_key"].([]byte)
	advDetails = row["adv_details"].([]byte)
	advAccountSig = row["adv_account_sig"].([]byte)
	advAccountSigKey = row["adv_account_sig_key"].([]byte)
	advDeviceSig = row["adv_device_sig"].([]byte)
	device.Platform = row["platform"].(string)
	device.BusinessName = row["business_name"].(string)
	device.PushName = row["push_name"].(string)
	device.LIDMigrationTimestamp = int64(row["lid_migration_ts"].(float64))

	// Parse JIDs
	device.ID = &jid
	if lid != "" {
		device.LID, _ = types.ParseJID(lid)
	}

	// Parse UUID
	if fbUUID.Valid {
		device.FacebookUUID, _ = uuid.Parse(fbUUID.String)
	}

	// Parse keys
	device.NoiseKey = keys.NewKeyPairFromPrivateKey(*(*[32]byte)(noiseKey))
	device.IdentityKey = keys.NewKeyPairFromPrivateKey(*(*[32]byte)(identityKey))
	var sig [64]byte
	copy(sig[:], signedPreKeySig)

	device.SignedPreKey = &keys.PreKey{
		KeyPair:   *keys.NewKeyPairFromPrivateKey(*(*[32]byte)(signedPreKey)),
		KeyID:     signedPreKeyID,
		Signature: &sig,
	}

	device.AdvSecretKey = advKey

	// Parse account info
	if len(advDetails) > 0 {
		device.Account = &waAdv.ADVSignedDeviceIdentity{
			Details:             advDetails,
			AccountSignature:    advAccountSig,
			AccountSignatureKey: advAccountSigKey,
			DeviceSignature:     advDeviceSig,
		}
	}

	device.Initialized = true
	device.Log = c.log
	device.Container = c

	// Initialize stores
	d1store := NewD1Store(c, jid)
	c.initializeDevice(&device, d1store)

	return &device, nil
}

// Close closes the database connection and flushes all pending operations
func (c *Container) Close() error {
	// Flush all stores before closing
	c.deviceLock.RLock()
	for _, device := range c.devices {
		if d1store, ok := device.Identities.(*D1Store); ok {
			d1store.FlushAll()
		}
	}
	c.deviceLock.RUnlock()
	
	return c.db.Close()
}

// GetPerformanceStats returns performance statistics for monitoring
func (c *Container) GetPerformanceStats() map[string]interface{} {
	stats := make(map[string]interface{})
	
	// Add database metrics
	stats["db_metrics"] = c.db.GetMetrics()
	
	// Add device count
	c.deviceLock.RLock()
	stats["device_count"] = len(c.devices)
	c.deviceLock.RUnlock()
	
	// Add cache stats for each device
	deviceStats := make(map[string]map[string]int)
	c.deviceLock.RLock()
	for jid, device := range c.devices {
		if d1store, ok := device.Identities.(*D1Store); ok {
			deviceStats[jid.String()] = d1store.GetCacheStats()
		}
	}
	c.deviceLock.RUnlock()
	stats["device_cache_stats"] = deviceStats
	
	return stats
}
