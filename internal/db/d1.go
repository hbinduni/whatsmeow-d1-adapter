package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/util/dbutil"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// D1Database represents a connection to Cloudflare D1 via REST API
type D1Database struct {
	endpoint  string
	apiToken  string
	accountID string
	dbID      string
	log       waLog.Logger
	client    *http.Client
	
	// Performance optimizations
	retryConfig RetryConfig
	batchBuffer []D1Request
	batchMutex  sync.Mutex
	batchTimer  *time.Timer
	
	// Warning rate limiting
	transactionWarningShown bool
	warningMutex           sync.Mutex
}

// D1Config holds configuration for D1 connection
type D1Config struct {
	APIToken     string
	AccountID    string
	DatabaseID   string
	Logger       waLog.Logger
	RetryConfig  *RetryConfig  // Optional retry configuration
	BatchConfig  *BatchConfig  // Optional batch configuration
	ClientConfig *ClientConfig // Optional HTTP client configuration
}

// RetryConfig holds retry configuration
type RetryConfig struct {
	MaxRetries      int           // Maximum number of retries (default: 3)
	InitialDelay    time.Duration // Initial delay between retries (default: 100ms)
	MaxDelay        time.Duration // Maximum delay between retries (default: 5s)
	BackoffFactor   float64       // Backoff multiplier (default: 2.0)
	RetryableErrors []string      // List of retryable error patterns
}

// BatchConfig holds batch operation configuration
type BatchConfig struct {
	Enabled      bool          // Enable batch processing (default: false)
	MaxSize      int           // Maximum batch size (default: 50)
	FlushTimeout time.Duration // Auto-flush timeout (default: 100ms)
}

// ClientConfig holds HTTP client configuration
type ClientConfig struct {
	Timeout            time.Duration // Request timeout (default: 30s)
	MaxIdleConns       int           // Max idle connections (default: 100)
	MaxConnsPerHost    int           // Max connections per host (default: 10)
	IdleConnTimeout    time.Duration // Idle connection timeout (default: 90s)
	TLSHandshakeTimeout time.Duration // TLS handshake timeout (default: 10s)
}

// D1Request represents a D1 API request
type D1Request struct {
	SQL string `json:"sql"`
}

// D1BatchRequest represents a batch D1 API request
type D1BatchRequest struct {
	Queries []D1Request `json:"queries"`
}

// D1Response represents the outer D1 API response
type D1Response struct {
	Result   []D1ResultSet `json:"result"`
	Success  bool          `json:"success"`
	Errors   any           `json:"errors"`
	Messages []any         `json:"messages"`
}

// D1ResultSet represents a single result set within the D1 response
type D1ResultSet struct {
	Results []map[string]any `json:"results"`
	Success bool             `json:"success"`
	Meta    any              `json:"meta"`
}

// NewD1Database creates a new D1 database connection
func NewD1Database(cfg D1Config) (*D1Database, error) {
	if cfg.AccountID == "" || cfg.DatabaseID == "" || cfg.APIToken == "" {
		return nil, fmt.Errorf("AccountID, DatabaseID, and APIToken are all required for D1 connection")
	}

	endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/d1/database/%s/query",
		cfg.AccountID, cfg.DatabaseID)

	// Setup HTTP client with optimizations
	clientConfig := getDefaultClientConfig()
	if cfg.ClientConfig != nil {
		clientConfig = *cfg.ClientConfig
	}
	
	transport := &http.Transport{
		MaxIdleConns:        clientConfig.MaxIdleConns,
		MaxConnsPerHost:     clientConfig.MaxConnsPerHost,
		IdleConnTimeout:     clientConfig.IdleConnTimeout,
		TLSHandshakeTimeout: clientConfig.TLSHandshakeTimeout,
		DisableKeepAlives:   false,
	}
	
	client := &http.Client{
		Timeout:   clientConfig.Timeout,
		Transport: transport,
	}

	// Setup retry configuration
	retryConfig := getDefaultRetryConfig()
	if cfg.RetryConfig != nil {
		retryConfig = *cfg.RetryConfig
	}

	db := &D1Database{
		endpoint:    endpoint,
		apiToken:    "Bearer " + cfg.APIToken,
		accountID:   cfg.AccountID,
		dbID:        cfg.DatabaseID,
		log:         cfg.Logger,
		client:      client,
		retryConfig: retryConfig,
		batchBuffer: make([]D1Request, 0),
	}

	// Setup batch processing if enabled
	if cfg.BatchConfig != nil && cfg.BatchConfig.Enabled {
		db.startBatchProcessor(*cfg.BatchConfig)
	}

	return db, nil
}

// getDefaultClientConfig returns default HTTP client configuration
func getDefaultClientConfig() ClientConfig {
	return ClientConfig{
		Timeout:             30 * time.Second,
		MaxIdleConns:        100,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// getDefaultRetryConfig returns default retry configuration
func getDefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:    3,
		InitialDelay:  100 * time.Millisecond,
		MaxDelay:      5 * time.Second,
		BackoffFactor: 2.0,
		RetryableErrors: []string{
			"connection reset",
			"timeout",
			"temporary failure",
			"rate limit",
			"503",
			"502",
			"504",
		},
	}
}

// QueryForResults executes a query and returns raw D1 results
func (d *D1Database) QueryForResults(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	sqlQuery := d.interpolateSQL(query, args...)
	return d.executeSQL(ctx, sqlQuery)
}

// QueryRowDirect executes a query that returns at most one row as map
func (d *D1Database) QueryRowDirect(ctx context.Context, query string, args ...any) (map[string]any, error) {
	results, err := d.QueryForResults(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, sql.ErrNoRows
	}

	return results[0], nil
}

// D1RowsInterface defines the interface expected by the store
type D1RowsInterface interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
}

// Query executes a query and returns compatible Rows interface
func (d *D1Database) Query(ctx context.Context, query string, args ...any) (D1RowsInterface, error) {
	results, err := d.QueryForResults(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	return &D1Rows{
		results: results,
		current: -1,
	}, nil
}

// QueryRow executes a query that returns at most one row (compatibility method)
func (d *D1Database) QueryRow(ctx context.Context, query string, args ...any) dbutil.Scannable {
	row, err := d.QueryRowDirect(ctx, query, args...)
	if err != nil {
		return &D1Row{err: err}
	}

	return &D1Row{row: row}
}

// D1Rows implements D1RowsInterface for multiple rows
type D1Rows struct {
	results []map[string]any
	current int
}

func (r *D1Rows) Next() bool {
	r.current++
	return r.current < len(r.results)
}

func (r *D1Rows) Scan(dest ...any) error {
	if r.current < 0 || r.current >= len(r.results) {
		return fmt.Errorf("no current row")
	}

	row := r.results[r.current]
	return scanRowValues(row, dest...)
}

func (r *D1Rows) Close() error {
	return nil
}

// D1Row implements dbutil.Scannable for single row
type D1Row struct {
	row map[string]any
	err error
}

func (r *D1Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}

	return scanRowValues(r.row, dest...)
}

// scanRowValues scans D1 result values into destination variables
// Uses a predefined column order to ensure consistent scanning
func scanRowValues(row map[string]any, dest ...any) error {
	// Define expected column order based on common SQL SELECT patterns
	// This helps ensure consistent scanning when column count matches
	commonColumns := []string{
		"jid", "their_id", "our_jid", "lid", "identity", "key", "session", "sender_key",
		"key_id", "key_data", "timestamp", "fingerprint", "name", "version", "hash",
		"index_mac", "value_mac", "first_name", "full_name", "push_name", "business_name",
		"chat_jid", "muted_until", "pinned", "archived", "sender_jid", "message_id",
		"their_jid", "token", "pn", "ciphertext_hash", "plaintext", "server_timestamp",
		"insert_timestamp", "facebook_uuid", "registration_id", "noise_key", "identity_key",
		"signed_pre_key", "signed_pre_key_id", "signed_pre_key_sig", "adv_key", "adv_details",
		"adv_account_sig", "adv_account_sig_key", "adv_device_sig", "platform", "lid_migration_ts",
		"uploaded", "chat_id", "sender_id",
	}

	// Get column names from the row in consistent order
	columns := make([]string, 0, len(row))
	
	// For simple queries (2-3 columns), try to preserve natural SQL order
	if len(row) <= 3 {
		// Define common 2-3 column patterns to preserve order
		commonPatterns := map[int][]string{
			2: {"key_id", "key", "token", "timestamp"},
			3: {"key_data", "timestamp", "fingerprint"},
		}
		
		if pattern, exists := commonPatterns[len(row)]; exists {
			// Try to match known patterns first
			for _, col := range pattern {
				if _, exists := row[col]; exists {
					columns = append(columns, col)
				}
			}
		}
		
		// Add any remaining columns not in patterns
		for col := range row {
			found := false
			for _, existingCol := range columns {
				if col == existingCol {
					found = true
					break
				}
			}
			if !found {
				columns = append(columns, col)
			}
		}
	} else {
		// For complex queries, use predefined order
		for _, col := range commonColumns {
			if _, exists := row[col]; exists {
				columns = append(columns, col)
			}
		}
		
		// Add any remaining columns not in the common list
		for col := range row {
			found := false
			for _, existingCol := range columns {
				if col == existingCol {
					found = true
					break
				}
			}
			if !found {
				columns = append(columns, col)
			}
		}
	}

	// Allow scanning fewer destinations than columns (like standard SQL drivers)
	if len(dest) > len(columns) {
		return fmt.Errorf("too many scan destinations: expected max %d, got %d", len(columns), len(dest))
	}

	// Scan only the first N columns where N = len(dest)
	for i := 0; i < len(dest); i++ {
		if i >= len(columns) {
			return fmt.Errorf("not enough columns: need at least %d, got %d", i+1, len(columns))
		}
		
		col := columns[i]
		val := row[col]
		if err := convertValue(val, dest[i]); err != nil {
			return fmt.Errorf("scan error for column %s (value: %v, type: %T): %w", col, val, val, err)
		}
	}

	return nil
}

// Exec executes a query without returning rows
func (d *D1Database) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	sqlQuery := d.interpolateSQL(query, args...)

	_, err := d.executeSQL(ctx, sqlQuery)
	if err != nil {
		return nil, err
	}

	// Return a simple result (D1 doesn't provide detailed result info)
	return &d1Result{}, nil
}

// executeSQL executes SQL via D1 REST API with retry logic
func (d *D1Database) executeSQL(ctx context.Context, sql string) ([]map[string]any, error) {
	return d.executeSQLWithRetry(ctx, sql)
}

// executeSQLWithRetry executes SQL with exponential backoff retry
func (d *D1Database) executeSQLWithRetry(ctx context.Context, sql string) ([]map[string]any, error) {
	var lastErr error
	delay := d.retryConfig.InitialDelay

	for attempt := 0; attempt <= d.retryConfig.MaxRetries; attempt++ {
		if attempt > 0 {
			// Add jitter to prevent thundering herd
			jitter := time.Duration(rand.Float64() * float64(delay) * 0.1)
			sleep := delay + jitter
			
			if sleep > d.retryConfig.MaxDelay {
				sleep = d.retryConfig.MaxDelay
			}
			
			d.log.Debugf("D1 retry attempt %d/%d after %v (previous error: %v)", 
				attempt, d.retryConfig.MaxRetries, sleep, lastErr)
			
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleep):
			}
			
			delay = time.Duration(float64(delay) * d.retryConfig.BackoffFactor)
		}

		result, err := d.executeSQLOnce(ctx, sql)
		if err == nil {
			if attempt > 0 {
				d.log.Infof("D1 request succeeded on retry attempt %d", attempt)
			}
			return result, nil
		}

		lastErr = err

		// Check if error is retryable
		if !d.isRetryableError(err) {
			d.log.Debugf("D1 error not retryable: %v", err)
			return nil, err
		}

		if attempt == d.retryConfig.MaxRetries {
			d.log.Warnf("D1 request failed after %d retries: %v", d.retryConfig.MaxRetries, err)
		}
	}

	return nil, fmt.Errorf("D1 request failed after %d retries: %w", d.retryConfig.MaxRetries, lastErr)
}

// executeSQLOnce executes SQL once without retry
func (d *D1Database) executeSQLOnce(ctx context.Context, sql string) ([]map[string]any, error) {
	reqBody, err := json.Marshal(D1Request{SQL: sql})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", d.endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", d.apiToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "whatsmeow-d1-adapter/1.0")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("D1 API error (status %d): %s", resp.StatusCode, string(body))
	}

	var d1Resp D1Response
	if err := json.Unmarshal(body, &d1Resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if !d1Resp.Success {
		return nil, fmt.Errorf("D1 query failed: %v", d1Resp.Errors)
	}

	// Extract actual results from the first result set
	if len(d1Resp.Result) == 0 {
		return []map[string]any{}, nil
	}

	firstResultSet := d1Resp.Result[0]
	if !firstResultSet.Success {
		return nil, fmt.Errorf("D1 result set failed")
	}

	return firstResultSet.Results, nil
}

// isRetryableError checks if an error is retryable
func (d *D1Database) isRetryableError(err error) bool {
	errStr := strings.ToLower(err.Error())
	for _, pattern := range d.retryConfig.RetryableErrors {
		if strings.Contains(errStr, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// interpolateSQL interpolates arguments into SQL string
// WARNING: This is not secure against SQL injection - use only with trusted input
func (d *D1Database) interpolateSQL(query string, args ...any) string {
	if len(args) == 0 {
		return query
	}

	// Replace ? placeholders with actual values
	result := query
	for i, arg := range args {
		placeholder := "?"
		var replacement string

		if arg == nil {
			replacement = "NULL"
		} else {
			switch v := arg.(type) {
			case string:
				// Escape single quotes and wrap in quotes
				replacement = "'" + strings.ReplaceAll(v, "'", "''") + "'"
			case int, int8, int16, int32, int64:
				replacement = fmt.Sprintf("%d", v)
			case uint, uint8, uint16, uint32, uint64:
				replacement = fmt.Sprintf("%d", v)
			case float32, float64:
				replacement = fmt.Sprintf("%f", v)
			case bool:
				if v {
					replacement = "1"
				} else {
					replacement = "0"
				}
			case []byte:
				if len(v) == 0 {
					replacement = "NULL"
				} else {
					// Use hex format for all BLOB data
					replacement = "X'" + fmt.Sprintf("%X", v) + "'"
				}
			default:
				// Convert to string and escape
				str := fmt.Sprintf("%v", v)
				replacement = "'" + strings.ReplaceAll(str, "'", "''") + "'"
			}
		}

		// Replace first occurrence of ?
		if idx := strings.Index(result, placeholder); idx != -1 {
			result = result[:idx] + replacement + result[idx+1:]
		} else {
			d.log.Warnf("D1 adapter: No placeholder found for argument %d", i)
		}
	}

	return result
}

// convertValue converts D1 JSON values to Go types
func convertValue(src any, dest any) error {
	if src == nil {
		// Handle NULL values - set to zero value or NULL state
		switch d := dest.(type) {
		case *string:
			*d = ""
		case *[]byte:
			*d = nil
		case *int:
			*d = 0
		case *int32:
			*d = 0
		case *int64:
			*d = 0
		case *uint32:
			*d = 0
		case *uint64:
			*d = 0
		case *bool:
			*d = false
		case *float64:
			*d = 0
		case *sql.NullInt32:
			*d = sql.NullInt32{Valid: false}
		case *sql.NullInt64:
			*d = sql.NullInt64{Valid: false}
		case *sql.NullString:
			*d = sql.NullString{Valid: false}
		case *sql.NullBool:
			*d = sql.NullBool{Valid: false}
		case *sql.NullFloat64:
			*d = sql.NullFloat64{Valid: false}
		default:
			return fmt.Errorf("unsupported destination type for NULL: %T", dest)
		}
		return nil
	}

	switch d := dest.(type) {
	case *string:
		*d = fmt.Sprintf("%v", src)
	case *[]byte:
		if s, ok := src.(string); ok {
			// D1 returns BLOB data as hex strings
			if decoded, err := hex.DecodeString(s); err == nil {
				*d = decoded
			} else {
				// If not hex, use as raw bytes
				*d = []byte(s)
			}
		} else if arr, ok := src.([]interface{}); ok {
			// D1 sometimes returns BLOB data as array of integers
			bytes := make([]byte, len(arr))
			for i, v := range arr {
				if f, ok := v.(float64); ok {
					bytes[i] = byte(f)
				} else {
					return fmt.Errorf("invalid byte value in BLOB array: %T %v", v, v)
				}
			}
			*d = bytes
		} else {
			return fmt.Errorf("unsupported BLOB source type: %T", src)
		}
	case *int:
		if f, ok := src.(float64); ok {
			*d = int(f)
		} else {
			return fmt.Errorf("cannot convert %T to int", src)
		}
	case *int32:
		if f, ok := src.(float64); ok {
			*d = int32(f)
		} else {
			return fmt.Errorf("cannot convert %T to int32", src)
		}
	case *int64:
		if f, ok := src.(float64); ok {
			*d = int64(f)
		} else {
			return fmt.Errorf("cannot convert %T to int64", src)
		}
	case *uint32:
		if f, ok := src.(float64); ok {
			*d = uint32(f)
		} else {
			return fmt.Errorf("cannot convert %T to uint32", src)
		}
	case *uint64:
		if f, ok := src.(float64); ok {
			*d = uint64(f)
		} else {
			return fmt.Errorf("cannot convert %T to uint64", src)
		}
	case *bool:
		if b, ok := src.(bool); ok {
			*d = b
		} else if f, ok := src.(float64); ok {
			*d = f != 0
		} else {
			return fmt.Errorf("cannot convert %T to bool", src)
		}
	case *float64:
		if f, ok := src.(float64); ok {
			*d = f
		} else {
			return fmt.Errorf("cannot convert %T to float64", src)
		}
	case *sql.NullInt32:
		if f, ok := src.(float64); ok {
			*d = sql.NullInt32{Int32: int32(f), Valid: true}
		} else {
			return fmt.Errorf("cannot convert %T to sql.NullInt32", src)
		}
	case *sql.NullInt64:
		if f, ok := src.(float64); ok {
			*d = sql.NullInt64{Int64: int64(f), Valid: true}
		} else {
			return fmt.Errorf("cannot convert %T to sql.NullInt64", src)
		}
	case *sql.NullString:
		*d = sql.NullString{String: fmt.Sprintf("%v", src), Valid: true}
	case *sql.NullBool:
		if b, ok := src.(bool); ok {
			*d = sql.NullBool{Bool: b, Valid: true}
		} else if f, ok := src.(float64); ok {
			*d = sql.NullBool{Bool: f != 0, Valid: true}
		} else {
			return fmt.Errorf("cannot convert %T to sql.NullBool", src)
		}
	case *sql.NullFloat64:
		if f, ok := src.(float64); ok {
			*d = sql.NullFloat64{Float64: f, Valid: true}
		} else {
			return fmt.Errorf("cannot convert %T to sql.NullFloat64", src)
		}
	default:
		return fmt.Errorf("unsupported destination type: %T", dest)
	}

	return nil
}

// d1Result implements sql.Result for D1 responses
type d1Result struct{}

func (r *d1Result) LastInsertId() (int64, error) {
	return 0, nil
}

func (r *d1Result) RowsAffected() (int64, error) {
	return 0, nil
}

// startBatchProcessor starts the batch processing goroutine
func (d *D1Database) startBatchProcessor(config BatchConfig) {
	go func() {
		for {
			time.Sleep(config.FlushTimeout)
			d.flushBatch()
		}
	}()
}

// addToBatch adds a query to the batch buffer
func (d *D1Database) addToBatch(sql string) {
	d.batchMutex.Lock()
	defer d.batchMutex.Unlock()

	d.batchBuffer = append(d.batchBuffer, D1Request{SQL: sql})

	// Auto-flush if batch is full
	if len(d.batchBuffer) >= 50 { // Default max batch size
		go d.flushBatch()
	}
}

// flushBatch flushes the current batch
func (d *D1Database) flushBatch() {
	d.batchMutex.Lock()
	if len(d.batchBuffer) == 0 {
		d.batchMutex.Unlock()
		return
	}

	batch := make([]D1Request, len(d.batchBuffer))
	copy(batch, d.batchBuffer)
	d.batchBuffer = d.batchBuffer[:0] // Clear buffer
	d.batchMutex.Unlock()

	if len(batch) == 1 {
		// Single query, use regular execution
		_, err := d.executeSQLOnce(context.Background(), batch[0].SQL)
		if err != nil {
			d.log.Warnf("Batch query failed: %v", err)
		}
	} else if len(batch) > 1 {
		// Multiple queries, use batch execution
		err := d.executeBatch(context.Background(), batch)
		if err != nil {
			d.log.Warnf("Batch execution failed: %v", err)
		}
	}
}

// executeBatch executes multiple queries in a single request
func (d *D1Database) executeBatch(ctx context.Context, queries []D1Request) error {
	// For now, execute queries sequentially since D1 REST API doesn't support true batching
	// This could be optimized with parallel execution in the future
	for i, query := range queries {
		_, err := d.executeSQLOnce(ctx, query.SQL)
		if err != nil {
			return fmt.Errorf("batch query %d failed: %w", i, err)
		}
	}
	return nil
}

// ExecuteAsync executes a query asynchronously (useful for INSERT/UPDATE/DELETE)
func (d *D1Database) ExecuteAsync(ctx context.Context, query string, args ...any) {
	sqlQuery := d.interpolateSQL(query, args...)
	go func() {
		_, err := d.executeSQLWithRetry(context.Background(), sqlQuery)
		if err != nil {
			d.log.Warnf("Async query failed: %v", err)
		}
	}()
}

// Close closes the database connection
func (d *D1Database) Close() error {
	// Flush any remaining batch operations
	d.flushBatch()
	
	// Stop batch timer if it exists
	if d.batchTimer != nil {
		d.batchTimer.Stop()
	}
	
	// HTTP client doesn't need explicit closing
	return nil
}

// DoTxn executes a function within a transaction
// NOTE: D1 doesn't support transactions via REST API, so this executes without transaction
func (d *D1Database) DoTxn(ctx context.Context, opts *sql.TxOptions, fn func(context.Context) error) error {
	// Show transaction warning only once to avoid log spam
	d.warningMutex.Lock()
	if !d.transactionWarningShown {
		d.log.Warnf("D1 adapter: Transactions not supported via REST API, executing without transaction (this warning will only be shown once)")
		d.transactionWarningShown = true
	}
	d.warningMutex.Unlock()
	
	return fn(ctx)
}

var (
	d1InstanceOnce sync.Once
	d1Instance     *D1Database
	d1InstanceErr  error
	d1InstanceCfg  D1Config
)

// GetD1Instance returns a singleton D1 database instance using sync.Once for better performance
func GetD1Instance(cfg D1Config) (*D1Database, error) {
	d1InstanceOnce.Do(func() {
		d1InstanceCfg = cfg
		d1Instance, d1InstanceErr = NewD1Database(cfg)
	})

	// Check if configuration changed (requires new instance)
	if d1Instance != nil && !configEqual(d1InstanceCfg, cfg) {
		// Reset for new configuration
		d1InstanceOnce = sync.Once{}
		return GetD1Instance(cfg)
	}

	return d1Instance, d1InstanceErr
}

// configEqual compares two D1Config instances
func configEqual(a, b D1Config) bool {
	return a.AccountID == b.AccountID &&
		a.DatabaseID == b.DatabaseID &&
		a.APIToken == b.APIToken
}

// Performance monitoring and metrics
type D1Metrics struct {
	TotalRequests     int64
	SuccessfulRequests int64
	FailedRequests    int64
	RetryAttempts     int64
	AverageLatency    time.Duration
	mutex             sync.RWMutex
}

// GetMetrics returns current performance metrics
func (d *D1Database) GetMetrics() D1Metrics {
	// This would need to be implemented with actual metrics collection
	// For now, return empty metrics
	return D1Metrics{}
}
