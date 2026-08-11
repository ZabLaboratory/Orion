package lsdp

// Production CredsFetcher: resolve a camera `peer_label` to its live Meet room
// viewer credentials via ZabCam, through ZabGate (ADR Blue 009 §3.2, #261).
//
// Room-agnostic resolution (the whole point of the ADR: a scene stores a
// CAMERA `peer_label`, not a frozen room id which dies 404):
//
//	GET ${ZABGATE}/cam/api/v1/cam/cameras/{peer_label}/credentials
//
// ZabGate strips its `/cam` prefix; ZabCam mounts `cameras.router`
// (prefix `/cam/cameras`) under `/api/v1`, so the external shape is
// `/cam/api/v1/cam/cameras/{label}/credentials` (same convention as the
// `zabcam.slots.assign` curated template). The response is ZabCam's
// `CamRoomPeerCredentials` (`meet_room_id`, `meet_token`, `meet_ws_url`),
// which ZabCam self-heals against Meet (re-mint on signaling restart) so the
// pair is always live.
//
// Auth posture (R1, Bastion): the credentials read is a GET, so it is NOT a
// curated-egress WRITE route (ADR 002 §3.1 — reads are not egress). The fetch
// presents a service token scoped to EXACTLY `zabcam.rooms.credentials`
// (defence-in-depth, tightest scope). If no scoped token is available, the
// fetch is skipped: this client never emits an unauthenticated request, even
// though the `/cam` prefix is gateway-public for publisher onboarding. The
// returned `meet_token` is NEVER logged.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// credsTokenScope is the tightest path-scope minted for the credentials read.
// Mirrors the ADR §3.4 registry name `zabcam.rooms.credentials`.
const credsTokenScope = "zabcam.rooms.credentials"

// maxCredsResponse bounds the credentials read (small JSON object).
const maxCredsResponse = 1 << 16

// camPeerCreds is the subset of ZabCam's CamRoomPeerCredentials the viewer
// needs. `meet_token` is the receive-only join token (R1) — never logged.
type camPeerCreds struct {
	MeetRoomID string `json:"meet_room_id"`
	MeetToken  string `json:"meet_token"`
	MeetWsURL  string `json:"meet_ws_url"`
}

// zabcamCredsFetcher is the production CredsFetcher.
type zabcamCredsFetcher struct {
	base   string                      // ZabGate base URL (operator config)
	mint   func(paths []string) string // scoped service-token minter; "" ⇒ skip fetch
	client *http.Client
	logger *slog.Logger
}

// NewZabCamCredsFetcher builds the production viewer-credentials fetcher.
// gatewayURL is the ZabGate base; mint mints a service token scoped to the
// given paths ("" when unavailable, in which case no request is sent). A nil
// client uses a 10 s-timeout default.
func NewZabCamCredsFetcher(gatewayURL string, mint func(paths []string) string, logger *slog.Logger) CredsFetcher {
	if mint == nil {
		mint = func([]string) string { return "" }
	}
	return &zabcamCredsFetcher{
		base:   strings.TrimRight(gatewayURL, "/"),
		mint:   mint,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// FetchViewerCreds resolves peerLabel to its live room viewer credentials.
// Best-effort: any failure returns ok=false (the peer is skipped). The token
// is never logged — status + room id only.
func (f *zabcamCredsFetcher) FetchViewerCreds(ctx context.Context, peerLabel string) (ViewerRoom, bool) {
	// Never send this read anonymously. A missing scoped token is a local
	// capability failure, not permission to rely on a gateway-public route.
	tok := f.mint([]string{credsTokenScope})
	if tok == "" {
		return ViewerRoom{}, false
	}

	// peerLabel is path-escaped so it can never add a segment or traverse.
	u := f.base + "/cam/api/v1/cam/cameras/" + url.PathEscape(peerLabel) + "/credentials"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ViewerRoom{}, false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := f.client.Do(req)
	if err != nil {
		if f.logger != nil {
			f.logger.Warn("viewer creds fetch failed", "err", err)
		}
		return ViewerRoom{}, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCredsResponse))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if f.logger != nil {
			// Status + room only — never the token.
			f.logger.Warn("viewer creds not resolved", "status", resp.StatusCode)
		}
		return ViewerRoom{}, false
	}
	var c camPeerCreds
	if err := json.Unmarshal(body, &c); err != nil {
		return ViewerRoom{}, false
	}
	if c.MeetRoomID == "" || c.MeetWsURL == "" || c.MeetToken == "" {
		return ViewerRoom{}, false
	}
	return ViewerRoom{
		SignalingURL: c.MeetWsURL,
		RoomID:       c.MeetRoomID,
		JoinToken:    c.MeetToken,
	}, true
}
