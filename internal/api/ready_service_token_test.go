package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// stubServiceTokens pins a state word so the readiness surface can be asserted
// in each of the three states (RC 51) without a ZabAuth and a Postgres. The
// state machine itself is proven in internal/auth/service_token_test.go.
type stubServiceTokens struct {
	token string
	state auth.ServiceTokenState
}

func (s stubServiceTokens) Token() string                 { return s.token }
func (s stubServiceTokens) State() auth.ServiceTokenState { return s.state }

func readyDeps(src ServiceTokenSource) PublicDeps {
	return PublicDeps{
		Show:          runtime.NewShow(runtime.NewComputeRegistry(), testLogger()),
		ServiceTokens: src,
	}
}

// RC 51: `service_token` is published on /ready in each of its three states,
// the response stays HTTP 200 in all three (an operator signal, not a liveness
// verdict), and it carries a state word and nothing else.
func TestReady_PublishesServiceTokenStateAlways200(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state auth.ServiceTokenState
	}{
		{"nominal", auth.ServiceTokenArmed},
		{"no token (lock not taken, or refresh terminal)", auth.ServiceTokenDegraded},
		{"persist failed", auth.ServiceTokenUnpersisted},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			deps := readyDeps(stubServiceTokens{
				token: "eyJhbGciOiJIUzI1NiJ9.SECRET-ACCESS-TOKEN.sig",
				state: tc.state,
			})
			w := httptest.NewRecorder()
			ready(deps)(w, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — service_token is never a liveness verdict", w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := body["service_token"]; got != string(tc.state) {
				t.Fatalf("service_token = %v, want %q", got, tc.state)
			}
			// No token material, no family id, no expiry — a state word only.
			raw := w.Body.String()
			for _, forbidden := range []string{"SECRET-ACCESS-TOKEN", "eyJ", "family_id", "expires", "refresh"} {
				if strings.Contains(raw, forbidden) {
					t.Fatalf("readiness payload leaks %q: %s", forbidden, raw)
				}
			}
		})
	}
}

// A missing manager reads as degraded rather than panicking or omitting the
// field: an operator polling /ready always gets an answer.
func TestReady_ServiceTokenDefaultsToDegraded(t *testing.T) {
	w := httptest.NewRecorder()
	ready(readyDeps(nil))(w, httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body["service_token"]; got != string(auth.ServiceTokenDegraded) {
		t.Fatalf("service_token = %v, want degraded", got)
	}
}

// The second half of RC 51: `.health.json` is the other place the state is
// published, and it must declare exactly the three words the code can emit —
// a mirror that drifts is worse than no mirror.
func TestHealthJSON_DeclaresTheServiceTokenStates(t *testing.T) {
	raw, err := os.ReadFile("../../.health.json")
	if err != nil {
		t.Fatalf("read .health.json: %v", err)
	}
	var doc struct {
		Checks []struct {
			Name   string   `json:"name"`
			Field  string   `json:"field"`
			States []string `json:"states"`
			Expect int      `json:"expect_status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse .health.json: %v", err)
	}
	want := []string{
		string(auth.ServiceTokenArmed),
		string(auth.ServiceTokenDegraded),
		string(auth.ServiceTokenUnpersisted),
	}
	for _, c := range doc.Checks {
		if c.Name != "service_token" {
			continue
		}
		if c.Field != "service_token" {
			t.Fatalf(".health.json service_token check reads field %q", c.Field)
		}
		if c.Expect != http.StatusOK {
			t.Fatalf(".health.json service_token expect_status = %d, want 200", c.Expect)
		}
		if strings.Join(c.States, ",") != strings.Join(want, ",") {
			t.Fatalf(".health.json states = %v, want %v", c.States, want)
		}
		return
	}
	t.Fatal(".health.json declares no service_token check (RC 51)")
}
