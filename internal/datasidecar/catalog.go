// Package datasidecar is the embedded-local data sidecar (ADR 016 §3.2,
// issue #225). It answers POST /<svc>/api/v1/_query against SQLite mirrors
// of ZabTruth/ZabRanking BYTE-IDENTICALLY to the way ZabGate→those services
// answer it on the antenna, so Orion's db.query hot path is the same whether
// it runs against the network or against this loopback sidecar bundled in
// Prism.
//
// The contract is frozen in docs/contracts/embedded-local-contracts.md §A
// (Conduit, #221). The wire shape is fully specified there; this package
// reproduces the SQLite↔Postgres semantic gaps the contract flags as the
// mandatory parity work (§A.6 #1-4): scalar coercion, LIKE case-sensitivity,
// Numeric→float typing, and Postgres NULL ordering.
//
// This is a sidecar, NOT on Orion's hot path: it substitutes the transport,
// never the shape. It runs inside Prism's trust boundary (loopback only).
package datasidecar

// columnType mirrors queryme.schema.ColumnType (the closed set declared by
// each service's db_catalog.py). It drives both the SQLite column affinity
// and the scalar coercion on the way out.
type columnType string

const (
	typeString   columnType = "string"
	typeText     columnType = "text"
	typeInteger  columnType = "integer"
	typeFloat    columnType = "float"
	typeBoolean  columnType = "boolean"
	typeUUID     columnType = "uuid"
	typeDateTime columnType = "datetime"
	typeDate     columnType = "date"
	typeJSON     columnType = "json"
)

// column is one declared, query-exposed column. Mirrors queryme.ColumnDef:
// only name/type/nullable matter to the wire compiler.
type column struct {
	Name     string
	Type     columnType
	Nullable bool
	Primary  bool
}

// table is one query-exposed table. Mirrors queryme.TableDef.
type table struct {
	Name    string
	Columns []column
}

func (t table) col(name string) (column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return column{}, false
}

// catalog is the per-service query-exposed schema. It is the SINGLE source
// of truth for validation, SQL compilation and scalar coercion, exactly as
// queryme's SchemaDescriptor is on the antenna side. The two catalogs below
// are transcribed verbatim from the live db_catalog.py of each service.
type catalog struct {
	Service string
	Tables  []table
}

func (c catalog) table(name string) (table, bool) {
	for _, t := range c.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return table{}, false
}

// truthCatalog is a verbatim transcription of
// ZabTruth/src/zabtruth/services/db_catalog.py (origin/main). Only the
// columns the service exposes to _query are listed; adding a model column
// does NOT expose it here (parity with the hand-authored Postgres catalog).
var truthCatalog = catalog{
	Service: "truth",
	Tables: []table{
		{Name: "players", Columns: []column{
			{Name: "id", Type: typeUUID, Primary: true},
			{Name: "summoner_name", Type: typeString},
			{Name: "riot_tag", Type: typeString, Nullable: true},
			{Name: "display_name", Type: typeString, Nullable: true},
			{Name: "leaguepedia_link", Type: typeString, Nullable: true},
			{Name: "primary_role", Type: typeString, Nullable: true},
			{Name: "team", Type: typeString, Nullable: true},
			{Name: "country", Type: typeString, Nullable: true},
			{Name: "notes", Type: typeText, Nullable: true},
			{Name: "created_at", Type: typeDateTime},
			{Name: "updated_at", Type: typeDateTime},
		}},
		{Name: "matches", Columns: []column{
			{Name: "id", Type: typeUUID, Primary: true},
			{Name: "riot_match_id", Type: typeString, Nullable: true},
			{Name: "leaguepedia_game_id", Type: typeString, Nullable: true},
			{Name: "leaguepedia_match_id", Type: typeString, Nullable: true},
			{Name: "overview_page", Type: typeString, Nullable: true},
			{Name: "league", Type: typeString, Nullable: true},
			{Name: "tournament", Type: typeString, Nullable: true},
			{Name: "split", Type: typeString, Nullable: true},
			{Name: "patch", Type: typeString, Nullable: true},
			{Name: "blue_team", Type: typeString, Nullable: true},
			{Name: "red_team", Type: typeString, Nullable: true},
			{Name: "winner_side", Type: typeString, Nullable: true},
			{Name: "duration_seconds", Type: typeInteger, Nullable: true},
			{Name: "played_at", Type: typeDateTime, Nullable: true},
			{Name: "notes", Type: typeText, Nullable: true},
			{Name: "created_at", Type: typeDateTime},
			{Name: "updated_at", Type: typeDateTime},
		}},
		{Name: "draft_picks", Columns: []column{
			{Name: "id", Type: typeUUID, Primary: true},
			{Name: "match_id", Type: typeUUID},
			{Name: "phase", Type: typeString},
			{Name: "side", Type: typeString},
			{Name: "pick_order", Type: typeInteger},
			{Name: "champion", Type: typeString},
			{Name: "role", Type: typeString, Nullable: true},
			{Name: "player_id", Type: typeUUID, Nullable: true},
		}},
		{Name: "match_players", Columns: []column{
			{Name: "id", Type: typeUUID, Primary: true},
			{Name: "match_id", Type: typeUUID},
			{Name: "player_id", Type: typeUUID},
			{Name: "side", Type: typeString},
			{Name: "role", Type: typeString},
			{Name: "champion", Type: typeString},
			{Name: "win", Type: typeBoolean},
			{Name: "kills", Type: typeInteger},
			{Name: "deaths", Type: typeInteger},
			{Name: "assists", Type: typeInteger},
			{Name: "cs", Type: typeInteger},
			{Name: "cs_per_min", Type: typeFloat, Nullable: true},
			{Name: "gold", Type: typeInteger},
			{Name: "gold_per_min", Type: typeInteger, Nullable: true},
			{Name: "damage_dealt", Type: typeInteger},
			{Name: "damage_taken", Type: typeInteger},
			{Name: "damage_to_objectives", Type: typeInteger, Nullable: true},
			{Name: "vision_score", Type: typeInteger, Nullable: true},
			{Name: "wards_placed", Type: typeInteger, Nullable: true},
			{Name: "wards_killed", Type: typeInteger, Nullable: true},
			{Name: "control_wards_bought", Type: typeInteger, Nullable: true},
			{Name: "kill_participation", Type: typeFloat, Nullable: true},
			{Name: "damage_share", Type: typeFloat, Nullable: true},
			{Name: "gold_share", Type: typeFloat, Nullable: true},
			{Name: "double_kills", Type: typeInteger, Nullable: true},
			{Name: "triple_kills", Type: typeInteger, Nullable: true},
			{Name: "quadra_kills", Type: typeInteger, Nullable: true},
			{Name: "penta_kills", Type: typeInteger, Nullable: true},
			{Name: "first_blood", Type: typeBoolean, Nullable: true},
		}},
	},
}

// rankingCatalog is a verbatim transcription of
// ZabRanking/src/zabranking/services/db_catalog.py (origin/main). NB:
// player_scores.score is Postgres Numeric(4,2) → Decimal → JSON float; the
// catalog types it "float" and the coercion must emit a JSON number, never
// a string (contract §A.6 #3).
var rankingCatalog = catalog{
	Service: "ranking",
	Tables: []table{
		{Name: "splits", Columns: []column{
			{Name: "id", Type: typeUUID, Primary: true},
			{Name: "name", Type: typeString},
			{Name: "tournament", Type: typeString, Nullable: true},
			{Name: "starts_on", Type: typeDate, Nullable: true},
			{Name: "ends_on", Type: typeDate, Nullable: true},
			{Name: "is_active", Type: typeBoolean},
			{Name: "description", Type: typeText, Nullable: true},
			{Name: "created_at", Type: typeDateTime},
			{Name: "updated_at", Type: typeDateTime},
		}},
		{Name: "player_scores", Columns: []column{
			{Name: "id", Type: typeUUID, Primary: true},
			{Name: "split_id", Type: typeUUID},
			{Name: "match_id", Type: typeUUID},
			{Name: "player_id", Type: typeUUID},
			{Name: "score", Type: typeFloat},
			{Name: "comment", Type: typeText, Nullable: true},
			{Name: "rated_by", Type: typeUUID, Nullable: true},
			{Name: "created_at", Type: typeDateTime},
			{Name: "updated_at", Type: typeDateTime},
		}},
	},
}

// catalogs maps the ZabGate <svc> prefix to its query-exposed schema. These
// are the only two services the embedded-local profile mirrors (#225).
var catalogs = map[string]catalog{
	"truth":   truthCatalog,
	"ranking": rankingCatalog,
}
