package obs

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// PrismRequestID returns the propagated request id or creates one for direct
// Orion callers. The same id is used in the response and in the structured
// event so Prism can correlate an error without parsing prose.
func PrismRequestID(w http.ResponseWriter) string {
	rid := w.Header().Get("X-Request-Id")
	if rid == "" {
		rid = uuid.NewString()
		w.Header().Set("X-Request-Id", rid)
	}
	return rid
}

// PrismEvent is the language-neutral event subset that a service can emit in
// its local structured log. Prism adds id/occurred/state/dedupe metadata when
// it promotes a response to the canonical journal.
func PrismEvent(status int, code, message, source, domain, requestID string, details map[string]any) map[string]any {
	severity := "info"
	if status >= 500 {
		severity = "error"
	} else if status >= 400 {
		severity = "warning"
	}
	if code == "" {
		if status >= 500 {
			code = "ACTION_FAILED"
		} else if status >= 400 {
			code = "ACTION_REFUSED"
		} else {
			code = "ACTION_SUCCEEDED"
		}
	}
	if details == nil {
		details = map[string]any{}
	}
	return map[string]any{
		"schemaVersion": 1,
		"severity":      severity,
		"domain":        domain,
		"source":        source,
		"code":          code,
		"message":       message,
		"context":       map[string]any{"requestId": requestID},
		"details":       details,
		"requestId":     requestID,
	}
}

func LogPrismEvent(status int, code, message, source, domain, requestID string, details map[string]any) {
	event := PrismEvent(status, code, message, source, domain, requestID, details)
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	} else if status >= 400 {
		level = slog.LevelWarn
	}
	slog.Default().LogAttrs(context.Background(), level, message, slog.Any("prism", event))
}

// WritePrismHTTPError is the small boundary helper for handlers that do not
// live in the API package (for example the embedded-local loopback guard).
func WritePrismHTTPError(w http.ResponseWriter, status int, code, message, source string) {
	rid := PrismRequestID(w)
	event := PrismEvent(status, code, message, source, "service", rid, nil)
	body := map[string]any{
		"schemaVersion": 1,
		"type":          "https://cyell.pro/problems/" + code,
		"title":         http.StatusText(status),
		"status":        status,
		"detail":        message,
		"code":          code,
		"message":       message,
		"severity":      event["severity"],
		"domain":        "service",
		"source":        source,
		"context":       event["context"],
		"details":       map[string]any{},
		"requestId":     rid,
		"legacy":        map[string]any{"detail": message},
	}
	LogPrismEvent(status, code, message, source, "service", rid, nil)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
