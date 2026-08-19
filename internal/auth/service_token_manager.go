package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const serviceTokenAAD = "orion.service_refresh_token.v1"

var errServiceTokenNotFound = errors.New("service token: durable state not found")

type ServiceTokenState string

const (
	ServiceTokenArmed    ServiceTokenState = "armed"
	ServiceTokenDegraded ServiceTokenState = "degraded"
	ServiceTokenStatic   ServiceTokenState = "static"
)

type serviceTokenStore interface {
	Get(ctx context.Context) ([]byte, error)
	Put(ctx context.Context, ciphertext []byte) error
}

type pgServiceTokenStore struct{ pool *pgxpool.Pool }

func (s *pgServiceTokenStore) Get(ctx context.Context) ([]byte, error) {
	var ciphertext []byte
	err := s.pool.QueryRow(ctx,
		`SELECT refresh_token_enc FROM service_token_state WHERE id = 1`,
	).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) || len(ciphertext) == 0 {
		return nil, errServiceTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read durable service token: %w", err)
	}
	return ciphertext, nil
}

func (s *pgServiceTokenStore) Put(ctx context.Context, ciphertext []byte) error {
	if len(ciphertext) == 0 {
		return errors.New("write durable service token: empty ciphertext")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_token_state (id, refresh_token_enc, rotated_at)
		VALUES (1, $1, now())
		ON CONFLICT (id) DO UPDATE SET
			refresh_token_enc = EXCLUDED.refresh_token_enc,
			rotated_at = now()`, ciphertext)
	if err != nil {
		return fmt.Errorf("write durable service token: %w", err)
	}
	return nil
}

func ensureServiceTokenSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS service_token_state (
			id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			refresh_token_enc bytea NOT NULL,
			rotated_at timestamptz NOT NULL DEFAULT now()
		)`)
	return err
}

type durableServiceTokenRecord struct {
	RefreshToken string `json:"rt"`
	Rotating     bool   `json:"rotating,omitempty"`
}

type serviceTokenBundle struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type terminalServiceTokenError struct{ err error }

func (e terminalServiceTokenError) Error() string { return e.err.Error() }
func (e terminalServiceTokenError) Unwrap() error { return e.err }

// ServiceTokenManager owns durable refresh-token possession used by Engine B.
// It never sends the family bearer to truth/ranking: callers use the current
// access token only to exchange for exact route-scoped tokens.
type ServiceTokenManager struct {
	store       serviceTokenStore
	pool        *pgxpool.Pool
	refreshURL  string
	seed        string
	staticToken string
	box         *serviceTokenBox
	boxErr      error
	client      *http.Client
	loggerValue *slog.Logger

	mu        sync.RWMutex
	access    string
	refresh   string
	expiresAt time.Time
	state     ServiceTokenState
	started   bool
	stop      chan struct{}
	wg        sync.WaitGroup
}

func NewServiceTokenManager(ctx context.Context, databaseURL, refreshURL, seed, encryptionKey, staticToken string, logger *slog.Logger) (*ServiceTokenManager, error) {
	m := &ServiceTokenManager{
		refreshURL:  strings.TrimRight(refreshURL, "/"),
		seed:        seed,
		staticToken: staticToken,
		client:      &http.Client{Timeout: 10 * time.Second},
		loggerValue: logger,
		state:       ServiceTokenDegraded,
	}
	if encryptionKey != "" {
		m.box, m.boxErr = newServiceTokenBox(encryptionKey)
	}
	if databaseURL == "" {
		return m, nil
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open service-token state database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping service-token state database: %w", err)
	}
	if err := ensureServiceTokenSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ensure service-token state schema: %w", err)
	}
	m.pool = pool
	m.store = &pgServiceTokenStore{pool: pool}
	return m, nil
}

func newTestServiceTokenManager(store serviceTokenStore, refreshURL, seed, encryptionKey string, client *http.Client, logger *slog.Logger) (*ServiceTokenManager, error) {
	box, err := newServiceTokenBox(encryptionKey)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &ServiceTokenManager{
		store: store, refreshURL: strings.TrimRight(refreshURL, "/"), seed: seed,
		box: box, client: client, loggerValue: logger, state: ServiceTokenDegraded,
	}, nil
}

// ServiceTokenRefreshURL maps /auth/api/v1/tokens to the possession-only
// refresh endpoint exposed by ZabAuth through the same gateway.
func ServiceTokenRefreshURL(validateURL string) string {
	base := strings.TrimSuffix(strings.TrimRight(validateURL, "/"), "/tokens")
	return base + "/service-tokens/refresh"
}

func (m *ServiceTokenManager) Token() string {
	if m == nil {
		return ""
	}
	if m.store == nil {
		return m.staticToken
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.access
}

func (m *ServiceTokenManager) State() ServiceTokenState {
	if m == nil {
		return ServiceTokenDegraded
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil && m.staticToken != "" {
		return ServiceTokenStatic
	}
	return m.state
}

// Start resolves the persisted refresh token, or the one-shot bootstrap seed,
// rotates it once, and then refreshes before the access token expires.
func (m *ServiceTokenManager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()
	if m.store == nil {
		m.setState(ServiceTokenStatic)
		return nil
	}
	if m.boxErr != nil || m.box == nil {
		m.logError("durable service token disabled: encryption key is invalid", m.boxErr)
		m.setState(ServiceTokenDegraded)
		return nil
	}

	enc, err := m.store.Get(ctx)
	if err != nil && !errors.Is(err, errServiceTokenNotFound) {
		m.logError("durable service token disabled: state read failed", err)
		m.setState(ServiceTokenDegraded)
		return nil
	}
	refresh := m.seed
	if err == nil {
		plain, openErr := m.box.open(enc)
		if openErr != nil {
			m.logError("durable service token disabled: state cannot be decrypted", openErr)
			m.setState(ServiceTokenDegraded)
			return nil
		}
		var record durableServiceTokenRecord
		if json.Unmarshal(plain, &record) != nil || record.RefreshToken == "" || record.Rotating {
			m.logError("durable service token disabled: state is malformed or marked rotating", nil)
			m.setState(ServiceTokenDegraded)
			return nil
		}
		refresh = record.RefreshToken
	}
	if refresh == "" {
		m.logError("durable service token disabled: no persisted refresh token or bootstrap seed", nil)
		m.setState(ServiceTokenDegraded)
		return nil
	}
	m.mu.Lock()
	m.refresh = refresh
	m.mu.Unlock()
	if err := m.rotate(ctx); err != nil {
		m.logError("durable service token boot rotation failed", err)
		return nil
	}
	m.stop = make(chan struct{})
	m.wg.Add(1)
	go m.refreshLoop()
	return nil
}

func (m *ServiceTokenManager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	started := m.started
	stop := m.stop
	if stop != nil {
		close(stop)
	}
	m.mu.Unlock()
	if started {
		m.wg.Wait()
	}
	m.mu.Lock()
	if m.stop == stop {
		m.stop = nil
	}
	m.mu.Unlock()
	if m.pool != nil {
		m.pool.Close()
		m.pool = nil
	}
}

func (m *ServiceTokenManager) rotate(ctx context.Context) error {
	m.mu.RLock()
	refresh := m.refresh
	m.mu.RUnlock()
	if refresh == "" {
		return errors.New("no refresh token in memory")
	}
	marker, err := m.box.seal(durableServiceTokenRecord{RefreshToken: refresh, Rotating: true})
	if err != nil {
		return err
	}
	if err := m.store.Put(ctx, marker); err != nil {
		return fmt.Errorf("persist rotation marker: %w", err)
	}
	bundle, err := m.refreshOnce(ctx, refresh)
	if err != nil {
		var terminal terminalServiceTokenError
		if errors.As(err, &terminal) {
			m.mu.Lock()
			m.access, m.refresh = "", ""
			m.state = ServiceTokenDegraded
			m.mu.Unlock()
		}
		return err
	}
	if bundle.AccessToken == "" || bundle.RefreshToken == "" || bundle.ExpiresAt.IsZero() {
		return errors.New("refresh response missing token material or expiry")
	}
	successor, err := m.box.seal(durableServiceTokenRecord{RefreshToken: bundle.RefreshToken})
	if err != nil {
		return err
	}
	if err := m.store.Put(ctx, successor); err != nil {
		m.mu.Lock()
		m.access, m.refresh = "", ""
		m.state = ServiceTokenDegraded
		m.mu.Unlock()
		return fmt.Errorf("persist rotated refresh token: %w", err)
	}
	m.mu.Lock()
	m.access = bundle.AccessToken
	m.refresh = bundle.RefreshToken
	m.expiresAt = bundle.ExpiresAt
	m.state = ServiceTokenArmed
	m.mu.Unlock()
	m.logger().Info("durable service token rotated", "expires_at", bundle.ExpiresAt)
	return nil
}

func (m *ServiceTokenManager) refreshLoop() {
	defer m.wg.Done()
	for {
		m.mu.RLock()
		expiresAt, stop := m.expiresAt, m.stop
		m.mu.RUnlock()
		wait := time.Until(expiresAt) - 5*time.Minute
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-stop:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := m.rotate(ctx)
		cancel()
		if err != nil {
			m.mu.RLock()
			terminal := m.refresh == ""
			m.mu.RUnlock()
			if terminal {
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(30 * time.Second):
			}
		}
	}
}

func (m *ServiceTokenManager) refreshOnce(ctx context.Context, refresh string) (serviceTokenBundle, error) {
	body, err := json.Marshal(map[string]string{"refresh_token": refresh})
	if err != nil {
		return serviceTokenBundle{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.refreshURL, bytes.NewReader(body))
	if err != nil {
		return serviceTokenBundle{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return serviceTokenBundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		e := fmt.Errorf("refresh endpoint returned HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return serviceTokenBundle{}, terminalServiceTokenError{err: e}
		}
		return serviceTokenBundle{}, e
	}
	var bundle serviceTokenBundle
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return serviceTokenBundle{}, fmt.Errorf("decode refresh response: %w", err)
	}
	return bundle, nil
}

func (m *ServiceTokenManager) setState(state ServiceTokenState) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (m *ServiceTokenManager) logger() *slog.Logger {
	if m.loggerValue != nil {
		return m.loggerValue
	}
	return slog.Default()
}

func (m *ServiceTokenManager) logError(message string, err error) {
	if err != nil {
		m.logger().Error(message, "err", err)
	} else {
		m.logger().Error(message)
	}
}

type serviceTokenBox struct{ aead cipher.AEAD }

func newServiceTokenBox(keyB64 string) (*serviceTokenBox, error) {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid ORION_ENCRYPTION_KEY: expected base64-encoded 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &serviceTokenBox{aead: aead}, nil
}

func (b *serviceTokenBox) seal(value durableServiceTokenRecord) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, []byte(serviceTokenAAD)), nil
}

func (b *serviceTokenBox) open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < b.aead.NonceSize()+b.aead.Overhead() {
		return nil, errors.New("durable service token ciphertext is too short")
	}
	plain, err := b.aead.Open(nil, ciphertext[:b.aead.NonceSize()], ciphertext[b.aead.NonceSize():], []byte(serviceTokenAAD))
	if err != nil {
		return nil, errors.New("durable service token decryption failed")
	}
	return plain, nil
}
