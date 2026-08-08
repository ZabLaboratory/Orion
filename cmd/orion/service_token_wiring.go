package main

import (
	"log/slog"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/secretbox"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// wireServiceTokens decides whether the durable service-token model arms, and
// returns the manager either way. It never returns nil and never fails: a
// token problem must not take the antenna off the air (§ A3.3 part 4,
// § A3.4 (f)).
//
// The durable model arms under exactly TWO conditions, both of which must hold
// (ADR ZabAuth 003 Amendment 3 § A3.3 parts 3 and 4):
//
//  1. **The profile is antenne.** The dangerous topology is not the replicas,
//     it is the embedded-local sidecars running inside Prism on operator
//     laptops. A sidecar that resolved the production seed would rotate the
//     antenna's family, and the next rotation on air would answer `reuse` and
//     revoke the family — the show goes down from a laptop. So under
//     embedded-local the manager is left on the dev/test static posture, no
//     refresh call is ever emitted, and a seed found in the environment is
//     refused LOUDLY rather than quietly dropped: a credential that is present
//     but ignored is an operator error to surface, not a default to honour.
//
//  2. **This process holds the rotation advisory lock.** `container_name:
//     orion` already makes `--scale orion=2` a compose error, but § 3.4 refuses
//     to hold an invariant by composition. A process that does not get the lock
//     does not arm: it logs the refusal and serves with NO service token.
//
// When the model does not arm, Store and Box stay nil. That is the whole
// mechanism: the manager is then in static mode, and on antenne StaticToken is
// empty (ORION_SERVICE_TOKEN is refused there), so Token() returns "", State()
// reports degraded, and Start() emits ZERO refresh calls. Fail-closed on the
// token, never on the process.
func wireServiceTokens(cfg config.Config, st store.Store, lockHeld bool, logger *slog.Logger) *auth.ServiceTokenManager {
	authBase := strings.TrimSuffix(cfg.ZabAuthValidateURL, "/tokens")
	m := &auth.ServiceTokenManager{
		RefreshURL: authBase + "/service-tokens/refresh",
		Logger:     logger,
	}

	if !cfg.Profile.IsAntenne() {
		// Guard 1. Refuse the durable material loudly, then keep the static
		// dev/test posture that embedded-local has always had.
		if refused := durableVarsPresent(cfg); len(refused) > 0 {
			logger.Error("durable service-token material is present under ORION_PROFILE="+string(cfg.Profile)+" and is IGNORED — the durable model is antenne-only (ADR ZabAuth 003 Am.3 § A3.3 part 3). A sidecar that rotated the production family would revoke it and take the show off the air. Remove these from the local .env.orion; they are never delivered to Prism.",
				"ignored", strings.Join(refused, ","))
		}
		m.StaticToken = cfg.ServiceToken
		return m
	}

	// § A3.3 part 5 / RC 47: ORION_SERVICE_TOKEN is the standing credential
	// this ADR retires. On antenne it is refused, not honoured — leaving it
	// wired would make the boot silently fall back onto it.
	if cfg.ServiceToken != "" {
		logger.Error("ORION_SERVICE_TOKEN is set but REFUSED on the antenne profile (ADR ZabAuth 003 Am.3 § A3.3 part 5): the durable refresh model is the only credential path. Remove it from the environment.")
	}

	// Guard 2.
	if !lockHeld {
		logger.Error("durable service token NOT armed: this process does not hold the rotation advisory lock — another Orion is already rotating the family against this database (ADR ZabAuth 003 Am.3 § A3.3 part 4). Orion stays on air with NO service token; zero refresh calls will be emitted and token-bearing outbound calls fail closed.")
		return m
	}

	m.Seed = cfg.ServiceRefreshToken
	box, boxErr := secretbox.New(cfg.EncryptionKey)
	if boxErr != nil {
		// No plaintext fallback, and no failed boot either: Orion airs
		// without a service token and says so on /ready.
		logger.Error("ORION_ENCRYPTION_KEY unusable — the durable service token cannot arm; Orion airs with no service token and token-bearing outbound calls fail closed", "err", boxErr)
		return m
	}
	m.Store = st
	m.Box = box
	return m
}

// durableVarsPresent names the antenne-only credential material found in a
// non-antenne environment. Names only — never values, which are secrets.
func durableVarsPresent(cfg config.Config) []string {
	var found []string
	if cfg.ServiceRefreshToken != "" {
		found = append(found, "ORION_SERVICE_REFRESH_TOKEN")
	}
	if cfg.EncryptionKey != "" {
		found = append(found, "ORION_ENCRYPTION_KEY")
	}
	return found
}
