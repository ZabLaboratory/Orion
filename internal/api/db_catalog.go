package api

import (
	"net/http"
	"sort"

	"github.com/ZabLaboratory/Orion/internal/effects"
)

// DB catalog surface (ADR Blue 008 §3.4, issue #211). Makes the
// platform DBs (ZabTruth, ZabRanking, …) introspectable so a cockpit can
// generate a `db.table` selector. STRICTLY READ-ONLY: every entry is
// derived from the owning service's `GET /_schema` — the same catalog its
// query validator whitelists against. No query is executed here and no
// table/column outside that whitelist is reachable; this adds no access
// beyond the read-schema surface already granted to Orion's service token.
//
// `{service}` is the logical datasource name from ORION_DATASOURCES
// (the prefix authored in `core.db.query@1`); an undeclared name yields
// DATASOURCE_NOT_DECLARED — never a wider lookup than the allowlist.

// catalogTable is the per-table shape both surfaces emit.
type catalogTable struct {
	Name              string            `json:"name"`
	SearchableColumns []string          `json:"searchable_columns"`
	ResultShape       map[string]string `json:"result_shape"`
}

// datasourceEntry is one `list_datasources` row of kind db.table.
type datasourceEntry struct {
	Kind              string            `json:"kind"` // always "db.table"
	Service           string            `json:"service"`
	Table             string            `json:"table"`
	SearchableColumns []string          `json:"searchable_columns"`
	ResultShape       map[string]string `json:"result_shape"`
}

// declaredDataSources returns the ORION_DATASOURCES allowlist as
// effects.DataSource, sorted by logical name for deterministic output.
func declaredDataSources(deps PublicDeps) []effects.DataSource {
	out := make([]effects.DataSource, 0, len(deps.Config.DataSources))
	for name, svc := range deps.Config.DataSources {
		out = append(out, effects.DataSource{Name: name, Svc: svc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func tablesFrom(schema *effects.Schema) []catalogTable {
	tables := make([]catalogTable, 0, len(schema.Tables))
	for _, t := range schema.Tables {
		tables = append(tables, catalogTable{
			Name:              t.Name,
			SearchableColumns: t.SearchableColumns(),
			ResultShape:       t.ResultShape(),
		})
	}
	return tables
}

// getDBSchema serves GET /api/v1/db/{service}/schema.
func getDBSchema(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("service")
		svc, declared := deps.Config.DataSources[name]
		if !declared {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"code":       "DATASOURCE_NOT_DECLARED",
				"datasource": name,
			})
			return
		}
		if deps.SchemaClient == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"code": "DB_CATALOG_UNAVAILABLE",
			})
			return
		}
		schema, err := deps.SchemaClient.Schema(r.Context(), effects.DataSource{Name: name, Svc: svc})
		if err != nil {
			deps.Logger.Warn("db schema fetch failed", "datasource", name, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"code":       "DB_SCHEMA_UNREACHABLE",
				"datasource": name,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service": name,
			"tables":  tablesFrom(schema),
		})
	}
}

// listDatasources serves GET /api/v1/db/datasources — every whitelisted
// (datasource, table) pair as a `kind: db.table` entry the cockpit
// consumes for source selectors. A datasource whose `_schema` is
// momentarily unreachable is skipped (graceful degradation, ADR §3.4 /
// Blue #165) rather than failing the whole listing.
func listDatasources(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries := []datasourceEntry{}
		if deps.SchemaClient != nil {
			for _, ds := range declaredDataSources(deps) {
				schema, err := deps.SchemaClient.Schema(r.Context(), ds)
				if err != nil {
					deps.Logger.Warn("db catalog: datasource skipped", "datasource", ds.Name, "err", err)
					continue
				}
				for _, t := range schema.Tables {
					entries = append(entries, datasourceEntry{
						Kind:              "db.table",
						Service:           ds.Name,
						Table:             t.Name,
						SearchableColumns: t.SearchableColumns(),
						ResultShape:       t.ResultShape(),
					})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"datasources": entries,
		})
	}
}
