// Package workload is Orion's client for the ZabGate workload surface
// (ADR-BLUE-012 §4.7): minting a short-lived Canvas delegation from the
// opaque `zabgate-auth-context.v1` ticket Prism relayed, proxying exactly
// one Canvas fetch through it, and reconciling an ambiguous mint after a
// timeout/crash. Orion never calls admit — the ticket IS the operator's
// consent, obtained by Prism with the operator's own JWT (Bastion,
// WORKLOAD-PROTOCOL-ALIGN-M3: an Orion-side admit was a confused deputy).
// Orion never talks to ZabAuth directly and never re-asserts the client
// context — every call rides the mTLS workload identity the deployment
// substrate issues (Amendment 2: short-lived rotated mTLS, no TPM node
// identity in Orion's own code path).
package workload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Code is one of the ten fail-closed refusal codes ZabGate/ZabAuth return
// on the workload surface (ADR-BLUE-012 §4.7). Every other/unknown
// response is treated as a failure too (fail-closed default) — Code is
// never left empty on an error path.
type Code string

const (
	CodeAuthContextUnavailable             Code = "AUTH_CONTEXT_UNAVAILABLE"
	CodeAuthContextExpired                 Code = "AUTH_CONTEXT_EXPIRED"
	CodeAuthContextMismatch                Code = "AUTH_CONTEXT_MISMATCH"
	CodeAuthContextReplayed                Code = "AUTH_CONTEXT_REPLAYED"
	CodeWorkloadIdentityInvalid            Code = "WORKLOAD_IDENTITY_INVALID"
	CodeDelegationScopeDenied              Code = "DELEGATION_SCOPE_DENIED"
	CodeDelegationRevoked                  Code = "DELEGATION_REVOKED"
	CodeDelegationAlreadyUsed              Code = "DELEGATION_ALREADY_USED"
	CodeDelegationRetryRequiresReadmission Code = "DELEGATION_RETRY_REQUIRES_READMISSION"
	CodeDelegationReconciliationPending    Code = "DELEGATION_RECONCILIATION_PENDING"
)

// knownCodes bounds Code to exactly the §4.7 minimal refusal set — an
// unrecognized code from Gate/Auth is never trusted as one of these ten;
// it surfaces as a generic *Error with an empty Code plus the raw string
// preserved in Raw, so a caller can never accidentally treat an unknown
// server response as a specific, recoverable refusal.
var knownCodes = map[string]Code{
	string(CodeAuthContextUnavailable):             CodeAuthContextUnavailable,
	string(CodeAuthContextExpired):                 CodeAuthContextExpired,
	string(CodeAuthContextMismatch):                CodeAuthContextMismatch,
	string(CodeAuthContextReplayed):                CodeAuthContextReplayed,
	string(CodeWorkloadIdentityInvalid):            CodeWorkloadIdentityInvalid,
	string(CodeDelegationScopeDenied):              CodeDelegationScopeDenied,
	string(CodeDelegationRevoked):                  CodeDelegationRevoked,
	string(CodeDelegationAlreadyUsed):              CodeDelegationAlreadyUsed,
	string(CodeDelegationRetryRequiresReadmission): CodeDelegationRetryRequiresReadmission,
	string(CodeDelegationReconciliationPending):    CodeDelegationReconciliationPending,
}

// Error wraps a fail-closed refusal from the workload surface. Code is
// empty when the server returned something outside the known ten — the
// caller must still treat it as fail-closed, never as success.
type Error struct {
	Code       Code
	Raw        string
	Message    string
	HTTPStatus int
}

func (e *Error) Error() string {
	if e.Code != "" {
		if e.Message != "" {
			return fmt.Sprintf("workload: %s: %s (http %d)", e.Code, e.Message, e.HTTPStatus)
		}
		return fmt.Sprintf("workload: %s (http %d)", e.Code, e.HTTPStatus)
	}
	if e.Message != "" {
		return fmt.Sprintf("workload: unrecognized refusal %q: %s (http %d)", e.Raw, e.Message, e.HTTPStatus)
	}
	return fmt.Sprintf("workload: unrecognized refusal %q (http %d)", e.Raw, e.HTTPStatus)
}

// Identity is Orion's mTLS workload identity as presented to ZabGate.
// The client certificate itself carries the SPIFFE SAN
// (spiffe://zab/workload/orion/<environment>/<instance_id>, §4.7); San
// and CertSHA256 are surfaced ADDITIONALLY as the explicit
// x-workload-san / x-workload-certificate-sha256 headers Conduit's
// cahier des charges requires, so Gate does not have to reparse the peer
// cert out of the TLS layer on every call.
type Identity struct {
	San        string
	CertSHA256 string // hex-encoded SHA-256 of the leaf cert DER
}

// FingerprintCert computes CertSHA256 from a DER-encoded certificate.
func FingerprintCert(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Client talks to ZabGate's `/internal/v1/workload/*` surface over mTLS.
// It never contacts ZabAuth directly (§4.7: "Orion ne contacte jamais
// ZabAuth directement") and never sees a client JWT — only the opaque
// `zabgate-auth-context.v1` ticket Prism relayed through the intent.
type Client struct {
	baseURL  string
	identity Identity
	http     *http.Client
}

// NewClient builds a Client. httpClient must already be configured with
// the workload mTLS client certificate (tls.Config.Certificates) and a
// trust anchor pinned to ZabGate's server certificate — this package does
// not construct the TLS material itself (deployment-substrate-owned key,
// §4.7: "clé non exportable générée/détenue par le substrate de
// déploiement, jamais par le code Orion").
func NewClient(baseURL string, identity Identity, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		return nil, errors.New("workload: httpClient with mTLS credentials is required")
	}
	if identity.San == "" || identity.CertSHA256 == "" {
		return nil, errors.New("workload: identity.San and identity.CertSHA256 are required")
	}
	t, ok := httpClient.Transport.(*http.Transport)
	if !ok || t == nil || t.TLSClientConfig == nil || len(t.TLSClientConfig.Certificates) == 0 {
		return nil, errors.New("workload: httpClient transport carries no client certificate")
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		identity: identity,
		http:     httpClient,
	}, nil
}

// Delegation is Gate's `DelegationIssueResponse` — the ephemeral Canvas
// delegation handle (§6.10), mono-intention, mono-action, never refreshed
// or transferred. Orion holds it in memory only, until a terminal result
// or deadline (§4.7). AccessToken is a secret: it is never logged.
type Delegation struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	JTI              string `json:"jti"`
	Status           string `json:"status"`
	ExpiresAt        string `json:"expires_at"`
	CanvasRequestKey string `json:"canvas_request_key"`
	RetrySequence    int    `json:"retry_sequence"`
	GateRequestID    string `json:"gate_request_id"`
}

type inlineAdmissionRequest struct {
	Ticket string          `json:"ticket"`
	Intent json.RawMessage `json:"intent"`
}

type inlineAdmissionResponse struct {
	Status   string `json:"status"`
	IntentID string `json:"intent_id"`
}

// mintRequest is Gate's `DelegationMintRequest`: the opaque ticket plus
// the FULL intent, relayed VERBATIM as the raw bytes Prism posted. Gate
// re-validates the intent against the ticket bindings
// (`_assert_ticket_matches` — `deadline` among them), so any
// reconstruction or re-serialization of the intent on Orion's side is an
// AUTH_CONTEXT_MISMATCH waiting to fire. json.RawMessage embeds the
// caller's bytes untouched at the value level.
type mintRequest struct {
	Ticket string          `json:"ticket"`
	Intent json.RawMessage `json:"intent"`
}

// MintDelegation requests the short-lived Canvas delegation from the
// opaque ticket Prism relayed and the intent bytes it relayed with it —
// Orion never parses or verifies the ticket itself (§4.7: "Gate transmet
// uniquement ce ticket opaque à Orion") and treats the intent as an
// opaque blob. A non-2xx response is decoded into one of the ten §4.7
// refusal codes and returned as *Error — never as a usable Delegation.
func (c *Client) MintDelegation(ctx context.Context, ticket string, intent json.RawMessage) (*Delegation, error) {
	var out Delegation
	if err := c.post(ctx, "/internal/v1/workload/delegations/mint",
		mintRequest{Ticket: ticket, Intent: intent}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AdmitInline asks Gate to revalidate the mTLS Orion identity, Gate ticket,
// Canvas reference and inline artifact digests without issuing a consumable
// Canvas delegation. The scene-intent handler uses this only when no Canvas
// fetch can occur because the validated capsule is already complete.
func (c *Client) AdmitInline(ctx context.Context, ticket string, intent json.RawMessage) error {
	var out inlineAdmissionResponse
	if err := c.post(ctx, "/internal/v1/workload/delegations/admit-inline", inlineAdmissionRequest{Ticket: ticket, Intent: intent}, &out); err != nil {
		return err
	}
	if out.Status != "admitted" || out.IntentID == "" {
		return &Error{Raw: "unexpected inline admission status " + out.Status, HTTPStatus: http.StatusOK}
	}
	return nil
}

// proxyRequest is Gate's `DelegationProxyRequest` (`schemas.py:299-303`)
// — the body BOTH /canvas and /reconcile require. Provenance of every
// field is fixed by the flow, never invented here: AccessToken and
// GateRequestID come from the mint's DelegationIssueResponse; Ticket and
// Intent are the same opaque ticket and raw intent bytes Prism relayed
// (the ones the mint itself was scoped to). Gate re-validates the intent
// against the ticket bindings on each call, so the intent stays a
// verbatim blob — same M3 invariant as the mint.
type proxyRequest struct {
	AccessToken   string          `json:"access_token"`
	Ticket        string          `json:"ticket"`
	Intent        json.RawMessage `json:"intent"`
	GateRequestID string          `json:"gate_request_id"`
}

// canvasResult is `_bounded_canvas_result`'s shape inside the proxy and
// reconciliation responses: the Canvas HTTP outcome Gate observed,
// bounded and JSON-parsed on the Gate side.
type canvasResult struct {
	StatusCode  int             `json:"status_code"`
	ContentType string          `json:"content_type"`
	Body        json.RawMessage `json:"body"`
}

// proxyResponse is Gate's `DelegationProxyResponse`/`ReconciliationResponse`
// (identical fields; result is optional on reconciliation): a 200 is
// always `status: "consumed"` — every other delegation state surfaces as
// a typed 409 refusal, never a 200.
type proxyResponse struct {
	JTI              string       `json:"jti"`
	Status           string       `json:"status"`
	CanvasRequestKey string       `json:"canvas_request_key"`
	Result           canvasResult `json:"result"`
}

// CanvasArtifact is the single Canvas response Gate proxied under the
// delegation's `canvas_request_key` (§4.7) — exact bytes, unmodified.
// Status/Body are the CANVAS outcome (`result.status_code`/`result.body`
// of the proxy response), not the Gate transport status.
type CanvasArtifact struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// FetchCanvas proxies exactly one Canvas request through the reserved
// delegation, posting the full DelegationProxyRequest Gate requires.
// Per §4.7, Orion never remints or retries from this call on an
// ambiguous outcome — a CodeDelegationReconciliationPending or
// CodeDelegationRetryRequiresReadmission error must route to Reconcile or
// back to a fresh admission respectively, never to a second FetchCanvas
// on the same jti.
func (c *Client) FetchCanvas(ctx context.Context, delegation *Delegation, ticket string, intent json.RawMessage) (*CanvasArtifact, error) {
	out, err := c.postProxy(ctx, "canvas", delegation, ticket, intent)
	if err != nil {
		return nil, err
	}
	return &CanvasArtifact{Status: out.Result.StatusCode, Body: out.Result.Body}, nil
}

// ReconcileResult reports the terminal state of a delegation whose
// FetchCanvas outcome was ambiguous (timeout or crash after reservation,
// §4.7). A non-error result is always terminal-consumed; `reserved` and
// `revoked` states surface as typed refusals from Gate, never here.
type ReconcileResult struct {
	JTI              string
	CanvasRequestKey string
	// Result is the observed Canvas outcome; may be empty when Gate's
	// reconciliation response carried no result payload.
	Result json.RawMessage
}

// Reconcile observes the delegation's terminal state through Gate's
// Canvas global lookup without a second Canvas effect. Same body and
// provenance as FetchCanvas. Orion calls this — never a fresh mint —
// whenever a predecessor is `reserved` or indeterminate (§4.7).
func (c *Client) Reconcile(ctx context.Context, delegation *Delegation, ticket string, intent json.RawMessage) (*ReconcileResult, error) {
	out, err := c.postProxy(ctx, "reconcile", delegation, ticket, intent)
	if err != nil {
		return nil, err
	}
	return &ReconcileResult{JTI: out.JTI, CanvasRequestKey: out.CanvasRequestKey, Result: out.Result.Body}, nil
}

// postProxy posts the shared DelegationProxyRequest to /canvas or
// /reconcile and fails closed on any 200 whose status is not the
// contract's only success state ("consumed").
func (c *Client) postProxy(ctx context.Context, leg string, delegation *Delegation, ticket string, intent json.RawMessage) (*proxyResponse, error) {
	if delegation == nil || delegation.JTI == "" || delegation.AccessToken == "" || delegation.GateRequestID == "" {
		return nil, errors.New("workload: delegation with jti, access_token and gate_request_id is required")
	}
	var out proxyResponse
	if err := c.post(ctx, "/internal/v1/workload/delegations/"+pathEscape(delegation.JTI)+"/"+leg,
		proxyRequest{
			AccessToken:   delegation.AccessToken,
			Ticket:        ticket,
			Intent:        intent,
			GateRequestID: delegation.GateRequestID,
		}, &out); err != nil {
		return nil, err
	}
	if out.Status != "consumed" {
		// Never trust an unexpected 200 as success — same fail-closed
		// posture as an unknown refusal code.
		return nil, &Error{Raw: "unexpected delegation status " + out.Status, HTTPStatus: http.StatusOK}
	}
	return &out, nil
}

func pathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

const maxWorkloadResponse = 1 << 20

func (c *Client) post(ctx context.Context, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("workload: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("workload: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Explicit workload identity headers (Conduit cahier des charges),
	// IN ADDITION to the mTLS handshake itself — Gate is never asked to
	// trust these over the peer certificate; they are a convenience
	// Gate is expected to cross-check against the handshake, never a
	// substitute for it. C2 (open, ZabGate side): Gate today gates on
	// peer IP, not on certificate verification — see AGENT_REPORT.
	req.Header.Set("x-workload-san", c.identity.San)
	req.Header.Set("x-workload-certificate-sha256", c.identity.CertSHA256)

	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Code: CodeAuthContextUnavailable, Raw: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWorkloadResponse))
	if err != nil {
		return fmt.Errorf("workload: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("workload: decode response: %w", err)
	}
	return nil
}

func decodeError(status int, raw []byte) error {
	// Gate/Auth render refusals as `{"detail": {"code", "message"}}`
	// (`routes.py::workload_error_handler` — "the same stable typed shape
	// as ZabAuth"). The bare `{"error": ...}` spelling is kept as a
	// fallback so an intermediary error page still fails closed with its
	// raw string preserved.
	var body struct {
		Detail struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"detail"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	observed := body.Detail.Code
	message := body.Detail.Message
	if observed == "" {
		observed = body.Error
	}
	code, known := knownCodes[observed]
	if !known {
		return &Error{Raw: strings.TrimSpace(observed), Message: message, HTTPStatus: status}
	}
	return &Error{Code: code, Raw: observed, Message: message, HTTPStatus: status}
}

// NewMTLSHTTPClient builds an *http.Client whose transport presents cert
// as the workload client identity and pins roots as the sole trust anchor
// for the ZabGate server certificate. The private key backing cert must
// come from the deployment substrate's non-exportable keystore (§4.7) —
// this function only wires whatever tls.Certificate the caller already
// obtained from it.
func NewMTLSHTTPClient(cert tls.Certificate, roots *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      roots,
				MinVersion:   tls.VersionTLS13,
			},
		},
	}
}
