package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZabLaboratory/Orion/internal/secretbox"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// ServiceTokenBundle mirrors ZabAuth's /service-tokens response shape.
// Source: ZabAuth/src/zabauth/schemas/tokens.py::TokenIssueResponse.
type ServiceTokenBundle struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

// ServiceTokenState is the operator-visible state word published on
// `GET /api/v1/ready` and declared in `.health.json`
// (ADR ZabAuth 003 Amendment 3 § A3.3 part 5, § A3.6 R21, RC 51).
//
// It is a state word and nothing else: it never carries token material, a
// family id, or an expiry. A deliberate degradation nobody can see is not a
// fail-soft, it is a silent outage — hence the surface.
type ServiceTokenState string

const (
	// ServiceTokenArmed — a token is held and the durable credential behind it
	// is persisted. Nominal.
	ServiceTokenArmed ServiceTokenState = "armed"
	// ServiceTokenDegraded — no token: nothing was resolvable at boot, the
	// refresh was rejected terminally, the profile is not antenne, or the
	// rotation advisory lock was not taken (#305). Outbound token-bearing calls
	// fail closed.
	ServiceTokenDegraded ServiceTokenState = "degraded"
	// ServiceTokenUnpersisted — a rotation succeeded server-side but its
	// successor could not be written to Postgres. The process keeps serving on
	// the in-memory value and retries the persist on its own bounded backoff.
	ServiceTokenUnpersisted ServiceTokenState = "unpersisted"
)

// ServiceTokenStore is the slice of store.Store the durable manager needs:
// the encrypted refresh token, opaque bytes on both sides
// (ADR ZabAuth 003 Amendment 3 § A3.3 part 2, delivered by #303).
type ServiceTokenStore interface {
	GetServiceRefreshToken(ctx context.Context) ([]byte, error)
	PutServiceRefreshToken(ctx context.Context, enc []byte) error
}

// durableRecord is what the encrypted column actually holds. The store sees
// opaque ciphertext; this layer owns the format.
//
// Rotating is the fail-closed marker of § A3.3 part 5. It is written BEFORE a
// refresh call goes out and cleared only by the successful persist of the
// successor. A boot that finds it set knows the stored value may already have
// been consumed server-side, and refuses to replay it — replaying a consumed
// generation trips `reuse` and revokes the whole family (§ 5 R8). The marker
// lives inside the sealed blob rather than in a column so the store surface
// #303 delivered stays exactly as specified: bytes in, bytes out.
type durableRecord struct {
	RefreshToken string `json:"rt"`
	Rotating     bool   `json:"rotating,omitempty"`
}

// ServiceTokenManager holds Orion's outbound service token by POSSESSION of a
// durable refresh token (ADR ZabAuth 003 Amendment 3 § A3.3 parts 1 and 5).
//
// It does not mint. Orion holds no operator credential and no access token at
// rest: the persisted value is a *refresh* token, so the first act of Start()
// is a rotation, and adoption (generation 0 → 1) happens on the first boot by
// construction.
//
// Two operating modes:
//
//   - **Durable mode** — Store and Box are set (antenne profile). Boot resolves
//     the persisted refresh token, else the ORION_SERVICE_REFRESH_TOKEN seed,
//     rotates once, and then rotates RefreshLead before every expiry. Every
//     rotation is persist-before-swap.
//   - **Static mode** — Store or Box is nil. Token() returns StaticToken
//     (ORION_SERVICE_TOKEN). Dev/test only: the antenne profile refuses to use
//     it (§ A3.3 part 5, RC 47), because falling back on a standing credential
//     is the exact shape this ADR retires.
//
// A token problem NEVER fails the boot and never takes the show off the air
// (§ A3.3 part 4, § A3.4 (f)): the process boots, the scene stays on air, and
// only token-bearing outbound calls fail — closed.
type ServiceTokenManager struct {
	// RefreshURL is ZabAuth's /service-tokens/refresh endpoint via ZabGate.
	RefreshURL string
	// Seed is the étage-1 bootstrap refresh token
	// (ORION_SERVICE_REFRESH_TOKEN). It is an amorce: resolved only when the
	// database holds nothing, consumed by the first rotation, and never
	// rewritten by the service.
	Seed string
	// StaticToken is the dev/test-only fallback (ORION_SERVICE_TOKEN). Used
	// only in static mode; the antenne wiring leaves it empty.
	StaticToken string
	// Store persists the encrypted refresh token. Nil ⇒ static mode.
	Store ServiceTokenStore
	// Box encrypts at rest (AES-256-GCM under ORION_ENCRYPTION_KEY). Nil ⇒
	// static mode. There is no plaintext fallback: a malformed key means the
	// durable manager does not arm, never that it writes plaintext.
	Box *secretbox.Box
	// RefreshLead is how long before expiry the manager rotates. Default 5 min.
	RefreshLead time.Duration
	// TransportRetry bounds the retry after a *transport* failure. Default 30 s.
	// It applies to transport alone: a refresh ZabAuth REJECTS is terminal.
	TransportRetry time.Duration
	// PersistRetryMin / PersistRetryMax bound the unpersisted-state backoff
	// (§ A3.3 part 5, R20 / RC 50). Defaults 1 s → 30 s. The retry re-writes
	// the same ciphertext (idempotent single-row write) and issues NO refresh
	// call; it does not wait for the next hourly rotation.
	PersistRetryMin time.Duration
	PersistRetryMax time.Duration
	// HTTPClient is reused for every refresh. Defaults to a 10 s timeout.
	HTTPClient *http.Client
	// Logger — required in durable mode.
	Logger *slog.Logger

	mu        sync.RWMutex
	access    string
	refresh   string
	expiresAt time.Time
	state     ServiceTokenState
	// pending is the ciphertext a failed persist still owes the database.
	pending []byte

	wake    chan struct{}
	stop    chan struct{}
	wg      sync.WaitGroup
	started bool
}

// durable reports whether the manager runs the durable model. Both fields are
// set at construction and never mutated, so this needs no lock.
func (m *ServiceTokenManager) durable() bool { return m.Store != nil && m.Box != nil }

// Token returns the current access token, or "" when the manager holds none —
// callers present it as a Bearer and the call fails closed at ZabGate. There is
// no fallback to any standing credential.
func (m *ServiceTokenManager) Token() string {
	if !m.durable() {
		return m.StaticToken
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.access
}

// State returns the operator-visible state word (RC 51). Safe on a nil
// receiver so the readiness handler can report it unconditionally.
func (m *ServiceTokenManager) State() ServiceTokenState {
	if m == nil {
		return ServiceTokenDegraded
	}
	if !m.durable() {
		if m.StaticToken != "" {
			return ServiceTokenArmed
		}
		return ServiceTokenDegraded
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.state == "" {
		return ServiceTokenDegraded
	}
	return m.state
}

// Start resolves the durable credential and performs the boot rotation, then
// launches the refresh and persist-retry loops. Idempotent.
//
// It returns an error only for a programming-level misconfiguration. A missing
// credential, an unreachable ZabAuth, a refused refresh or an unreadable
// database all leave the manager degraded and return nil: Orion boots and airs
// regardless (§ A3.3 part 4, RC 48).
func (m *ServiceTokenManager) Start(ctx context.Context) error {
	if m.started {
		return nil
	}
	m.applyDefaults()
	if !m.durable() {
		return nil
	}
	if m.RefreshURL == "" {
		return errors.New("service token: durable mode requires RefreshURL")
	}
	m.started = true
	m.stop = make(chan struct{})
	m.wake = make(chan struct{}, 1)

	rt, ok := m.resolve(ctx)
	if !ok {
		return nil // degraded; no credential to rotate, so no loops
	}
	m.mu.Lock()
	m.refresh = rt
	m.mu.Unlock()

	// The boot gesture is a REFRESH, never a mint.
	if m.rotate(ctx) == rotateTerminal {
		return nil
	}
	m.wg.Add(2)
	go m.refreshLoop()
	go m.persistLoop()
	return nil
}

// Stop signals the background loops and waits for them to exit. Safe when
// Start was never called or when in static mode.
func (m *ServiceTokenManager) Stop() {
	if !m.started {
		return
	}
	close(m.stop)
	m.wg.Wait()
	m.started = false
	m.stop = nil
}

func (m *ServiceTokenManager) applyDefaults() {
	if m.RefreshLead <= 0 {
		m.RefreshLead = 5 * time.Minute
	}
	if m.TransportRetry <= 0 {
		m.TransportRetry = 30 * time.Second
	}
	if m.PersistRetryMin <= 0 {
		m.PersistRetryMin = time.Second
	}
	if m.PersistRetryMax < m.PersistRetryMin {
		m.PersistRetryMax = 30 * time.Second
	}
	if m.HTTPClient == nil {
		m.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
}

// resolve applies the boot resolution order of § A3.3 part 1: the persisted
// value if present, else the seed. It returns ok=false — degraded, no rotation
// attempted — whenever replaying what it found could revoke the family.
func (m *ServiceTokenManager) resolve(ctx context.Context) (string, bool) {
	enc, err := m.Store.GetServiceRefreshToken(ctx)
	switch {
	case err == nil:
		plain, oerr := m.Box.Open(enc)
		if oerr != nil {
			m.logger().Error("durable service token: the persisted credential does not decrypt; refusing to arm (no plaintext fallback)", "err", oerr)
			return "", false
		}
		var rec durableRecord
		if jerr := json.Unmarshal(plain, &rec); jerr != nil || rec.RefreshToken == "" {
			m.logger().Error("durable service token: the persisted credential is malformed; refusing to arm")
			return "", false
		}
		if rec.Rotating {
			// Fail closed (§ A3.3 part 5, § 5 R8). "Fails closed" means no
			// token — NOT a refused boot: the process runs, the show airs.
			m.logger().Error("durable service token: the persisted credential is marked rotating — a previous rotation advanced the family server-side without recording its successor. Refusing to replay it (that would trip reuse and revoke the family). Orion airs with NO service token; an operator must re-mint and re-seed (runbook service-token-lifecycle.md)")
			return "", false
		}
		return rec.RefreshToken, true
	case errors.Is(err, store.ErrNotFound):
		if m.Seed == "" {
			m.logger().Error("durable service token: nothing persisted and no ORION_SERVICE_REFRESH_TOKEN seed; token-bearing outbound calls fail closed")
			return "", false
		}
		m.logger().Info("durable service token: nothing persisted; resolving the boot seed (consumed by the first rotation)")
		return m.Seed, true
	default:
		m.logger().Error("durable service token: reading the persisted credential failed; refusing to arm rather than replay a seed", "err", err)
		return "", false
	}
}

type rotateOutcome int

const (
	rotateOK rotateOutcome = iota
	// rotateTransport — the chain may not have advanced, or advanced without
	// a usable answer. Retry the same value on TransportRetry.
	rotateTransport
	// rotateTerminal — ZabAuth rejected this value (reuse / revoked / expired).
	// No retry, no re-mint (Orion holds no minting credential): the manager
	// goes token-less until an operator re-seeds.
	rotateTerminal
)

// rotate is the persist-before-swap rotation of § A3.3 part 5:
//
//	mark rotating → refresh → persist the successor → swap in memory
//
// The marker goes first because the two writes bracket the only window where
// the family can be lost: between ZabAuth advancing the chain and Orion
// recording the successor. Marking before the call means a crash inside that
// window is recoverable-by-refusal (a boot that finds the marker fails closed)
// instead of silently replaying a consumed generation.
//
// A crash between the marker and a refresh that never reached ZabAuth costs a
// re-seed for a credential that was in fact still good. That asymmetry is
// accepted deliberately: in the other ordering the same crash replays a
// consumed generation, which revokes the live family AND writes a record that
// is indistinguishable from a theft in ZabAuth's audit trail (§ 5 R8). Either
// way the operator re-mints; only one of the two also cries wolf.
func (m *ServiceTokenManager) rotate(ctx context.Context) rotateOutcome {
	m.mu.RLock()
	rt := m.refresh
	m.mu.RUnlock()
	if rt == "" {
		return rotateTerminal
	}

	marker, err := m.seal(durableRecord{RefreshToken: rt, Rotating: true})
	if err != nil {
		m.logger().Error("durable service token: sealing the rotation marker failed; skipping this rotation", "err", err)
		return rotateTransport
	}
	if err := m.Store.PutServiceRefreshToken(ctx, marker); err != nil {
		// The chain has NOT advanced. Skipping is strictly safer than
		// advancing a chain we would be unable to record.
		m.logger().Error("durable service token: could not record the rotation marker; skipping this rotation rather than advancing a chain we cannot persist", "err", err)
		return rotateTransport
	}

	bundle, err := m.refreshOnce(ctx, rt)
	if err != nil {
		var terminal terminalRejection
		if errors.As(err, &terminal) {
			m.mu.Lock()
			m.access = ""
			m.refresh = ""
			m.state = ServiceTokenDegraded
			m.mu.Unlock()
			m.logger().Error("durable service token: ZabAuth rejected the refresh — terminal. No retry on this value and no re-mint (Orion holds no minting credential); an operator must re-seed", "err", err)
			return rotateTerminal
		}
		m.logger().Warn("durable service token: refresh failed on transport; will retry the same value", "err", err)
		return rotateTransport
	}

	successor, serr := m.seal(durableRecord{RefreshToken: bundle.RefreshToken})
	if serr != nil {
		// Cannot even produce ciphertext. Serve on the in-memory value and let
		// the persist loop retry once a successor can be sealed — which it
		// cannot, so this degrades to unpersisted and says so.
		m.logger().Error("durable service token: sealing the rotated credential failed", "err", serr)
	}
	perr := errors.New("service token: successor not sealed")
	if serr == nil {
		perr = m.Store.PutServiceRefreshToken(ctx, successor)
	}

	m.mu.Lock()
	m.access = bundle.AccessToken
	m.refresh = bundle.RefreshToken
	m.expiresAt = bundle.ExpiresAt
	if perr == nil {
		m.state = ServiceTokenArmed
		m.pending = nil
	} else {
		m.state = ServiceTokenUnpersisted
		m.pending = successor
	}
	m.mu.Unlock()

	if perr != nil {
		m.logger().Error("durable service token: rotation persisted NOTHING — the chain advanced server-side and the in-memory value is the only copy. Still serving; state unpersisted; retrying the persist on its own backoff", "err", perr)
		m.kickPersist()
		return rotateOK
	}
	m.logger().Info("durable service token rotated", "expires_at", bundle.ExpiresAt)
	return rotateOK
}

// refreshLoop rotates RefreshLead before expiry, and bounds the retry after a
// transport failure. A terminal rejection ends the loop: there is nothing left
// to retry with.
func (m *ServiceTokenManager) refreshLoop() {
	defer m.wg.Done()
	for {
		m.mu.RLock()
		expiresAt := m.expiresAt
		m.mu.RUnlock()
		// A zero expiry means the boot rotation never landed a bundle. Do not
		// compute a lead against it: time.Until(time.Time{}) overflows the
		// int64 duration and wraps to a positive ~292-year sleep, which would
		// silently retire the retry the transport branch depends on.
		wait := time.Second
		if !expiresAt.IsZero() {
			if d := time.Until(expiresAt) - m.RefreshLead; d > time.Second {
				wait = d
			}
		}
		select {
		case <-m.stop:
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		outcome := m.rotate(ctx)
		cancel()
		switch outcome {
		case rotateTerminal:
			return
		case rotateTransport:
			select {
			case <-m.stop:
				return
			case <-time.After(m.TransportRetry):
			}
		case rotateOK:
		}
	}
}

// persistLoop is the R20 / RC 50 condition: the unpersisted state retries the
// persist on its OWN bounded backoff and clears the flag the moment the
// database answers. It never issues a refresh call — re-writing the same
// ciphertext is a single-row write, not a rotation, so it carries no reuse
// risk. Waiting for the next hourly rotation instead would leave a full hour
// during which a crash costs the family for a database blip of seconds.
func (m *ServiceTokenManager) persistLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		case <-m.wake:
		}
		backoff := m.PersistRetryMin
		for {
			m.mu.RLock()
			enc := m.pending
			m.mu.RUnlock()
			if enc == nil {
				break
			}
			select {
			case <-m.stop:
				return
			case <-time.After(backoff):
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := m.Store.PutServiceRefreshToken(ctx, enc)
			cancel()
			if err == nil {
				m.mu.Lock()
				if bytes.Equal(m.pending, enc) {
					m.pending = nil
					m.state = ServiceTokenArmed
				}
				m.mu.Unlock()
				m.logger().Info("durable service token: re-persisted after an earlier failure; state armed")
				break
			}
			m.logger().Warn("durable service token: persist retry failed; backing off", "err", err)
			backoff *= 2
			if backoff > m.PersistRetryMax {
				backoff = m.PersistRetryMax
			}
		}
	}
}

func (m *ServiceTokenManager) kickPersist() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *ServiceTokenManager) seal(rec durableRecord) ([]byte, error) {
	plain, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return m.Box.Seal(plain)
}

// terminalRejection marks a refresh ZabAuth decided against: reuse, revoked,
// expired, malformed. Retrying the same value cannot change the answer.
type terminalRejection struct{ err error }

func (t terminalRejection) Error() string { return t.err.Error() }
func (t terminalRejection) Unwrap() error { return t.err }

// refreshOnce rotates one generation. Per ADR 003 § 3.1 the presented
// refresh_token IS the credential: the route carries no operator gate and
// ZabAuth ignores any bearer, so no Authorization header is set — under the
// durable model Orion holds no operator credential to set one with.
func (m *ServiceTokenManager) refreshOnce(ctx context.Context, rt string) (ServiceTokenBundle, error) {
	body, err := json.Marshal(map[string]string{"refresh_token": rt})
	if err != nil {
		return ServiceTokenBundle{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.refreshEndpoint(), bytes.NewReader(body))
	if err != nil {
		return ServiceTokenBundle{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.HTTPClient.Do(req)
	if err != nil {
		return ServiceTokenBundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		status := fmt.Errorf("zabauth status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
		if isTerminalStatus(resp.StatusCode) {
			return ServiceTokenBundle{}, terminalRejection{status}
		}
		return ServiceTokenBundle{}, status
	}
	var out ServiceTokenBundle
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ServiceTokenBundle{}, fmt.Errorf("decode: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return ServiceTokenBundle{}, errors.New("zabauth returned an incomplete token bundle")
	}
	return out, nil
}

// isTerminalStatus classifies a rejection. Every 4xx is ZabAuth's decision
// about THIS value — reuse detection answers 401 — except the two that mean
// "ask again later" (408 timeout, 429 the per-IP failure budget of § 3.7).
// 5xx is the far side being unwell, i.e. transport.
func isTerminalStatus(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

func (m *ServiceTokenManager) refreshEndpoint() string {
	url := strings.TrimRight(m.RefreshURL, "/")
	if !strings.HasSuffix(url, "/refresh") {
		url += "/refresh"
	}
	return url
}

func (m *ServiceTokenManager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}
