package datasidecar

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo; distroless-safe)
)

// Server is the embedded-local data sidecar. It owns one in-memory SQLite
// mirror per mirrored service and serves the frozen _query contract on
// loopback. It substitutes the transport, never the shape.
//
// Auth stance (contract §A.5): OPTION (a) no-auth loopback. The sidecar runs
// inside Prism's trust boundary (loopback only, not network-reachable), so it
// ignores Authorization and serves every _query — matching Orion's
// tokenFn=="" path (no header sent in embedded-local). The 200/400 surfaces
// stay byte-identical, which is the hot-path requirement. This is the Bastion
// touch-point flagged in §A.5; the no-auth choice is stated in #225.
type Server struct {
	dbs map[string]*sql.DB // svc → mirror
	log *slog.Logger
}

// NewServer opens an in-memory SQLite mirror for each mirrored service,
// applies the schema + LIKE case-sensitivity pragma, and seeds the live-test
// data (LCK HLE vs Gen.G, LEC MKOI vs G2, the ZabRanking split + scores).
func NewServer(log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{dbs: map[string]*sql.DB{}, log: log}
	for svc, cat := range catalogs {
		// A distinct shared-cache in-memory DB per service so the two
		// mirrors stay isolated (a query can never cross a service
		// boundary — gateway-first invariant, even on loopback).
		dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", svc)
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		// One open connection kept for the lifetime so the in-memory DB
		// (which lives per-connection) is not torn down between calls.
		db.SetMaxOpenConns(1)
		if err := applyMirror(context.Background(), db, cat); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sidecar: mirror %s: %w", svc, err)
		}
		s.dbs[svc] = db
	}
	if err := seedAll(context.Background(), s.dbs); err != nil {
		s.Close()
		return nil, fmt.Errorf("sidecar: seed: %w", err)
	}
	return s, nil
}

// Close releases every mirror.
func (s *Server) Close() {
	for _, db := range s.dbs {
		_ = db.Close()
	}
}

// Handler returns the http.Handler serving POST /<svc>/api/v1/_query for
// every mirrored service (the full path Orion writes — no prefix strip in
// embedded-local, contract §A.1).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for svc := range s.dbs {
		mux.HandleFunc("POST /"+svc+"/api/v1/_query", func(w http.ResponseWriter, r *http.Request) {
			s.handleQuery(w, r, svc)
		})
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request, svc string) {
	cat := catalogs[svc]
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		writeIssues(w, http.StatusBadRequest, []validationIssue{{
			Code: "invalid_body", Message: err.Error(), Path: "body",
		}})
		return
	}

	d, err := decodeDescriptor(raw)
	if err != nil {
		// Descriptor shape failure → 400 with the issues envelope so the
		// editor/error-port reads it (contract §A.4: body MUST carry "issues").
		writeIssues(w, http.StatusBadRequest, []validationIssue{{
			Code: "invalid_descriptor", Message: err.Error(), Path: "body",
		}})
		return
	}

	if issues := cat.validate(d); len(issues) > 0 {
		writeIssues(w, http.StatusBadRequest, issues)
		return
	}

	comp, err := cat.compile(d)
	if err != nil {
		writeIssues(w, http.StatusBadRequest, []validationIssue{{
			Code: "compilation_error", Message: err.Error(), Path: "table",
		}})
		return
	}

	start := time.Now()
	rows, err := s.dbs[svc].QueryContext(r.Context(), comp.sql, comp.args...)
	if err != nil {
		s.log.Error("sidecar query failed", "svc", svc, "err", err)
		http.Error(w, fmt.Sprintf(`{"error":"QUERY_FAILED","detail":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	outRows, err := scanRows(rows, comp)
	if err != nil {
		s.log.Error("sidecar scan failed", "svc", svc, "err", err)
		http.Error(w, fmt.Sprintf(`{"error":"SCAN_FAILED","detail":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	elapsedMS := int(time.Since(start).Milliseconds())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"rows":       outRows,
		"count":      len(outRows),
		"elapsed_ms": elapsedMS,
	})
}

// writeIssues emits the 400 envelope the antenna emits and the contract locks:
// {"detail":{"issues":[...]}} (FastAPI wraps HTTPException(detail=...) under
// "detail"; the service raises detail={"issues":[...]}).
func writeIssues(w http.ResponseWriter, status int, issues []validationIssue) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"detail": map[string]any{"issues": issues},
	})
}
