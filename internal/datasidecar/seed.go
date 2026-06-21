package datasidecar

import (
	"context"
	"crypto/sha1" //nolint:gosec // deterministic id derivation only, not security
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// Frozen identifiers for the embedded-local live-test data (issue #225).
// These match the fixtures the brief pins so a frozen scene's db.query nodes
// resolve against the same ids whether the data comes from the antenna or the
// local mirror.
const (
	lckMatchID = "10eba940-0000-4000-8000-000000000001" // LCK Finals G1 — HLE vs Gen.G
	lecMatchID = "48a9e1b4-0000-4000-8000-000000000002" // LEC Finals G1 — Movistar KOI vs G2
	splitID    = "53465f19-0000-4000-8000-000000000003" // ZabRanking split "Draft Test 2025"
)

// scoreCycle is verbatim the bulk-rating cycle from
// ZabTruth/scripts/seed_draft_test_data.sh (rate_match): the i-th player of a
// match gets cycle[i%10]. Reproduced so player_scores.score matches the seed.
var scoreCycle = []float64{9.5, 8.0, 7.5, 6.0, 4.5, 9.0, 5.5, 3.5, 8.5, 7.0}

// rosterEntry is one player line of a seeded game (the 5 blue + 5 red of a
// single League game). side/role/champion mirror what the Leaguepedia import
// would write into match_players; summoner_name is the NOT NULL player handle.
type rosterEntry struct {
	Summoner string
	Side     string // blue | red
	Role     string
	Champion string
}

// lckRoster — HLE vs Gen.G, LCK 2025 Season Playoffs Finals game 1.
var lckRoster = []rosterEntry{
	{"Zeus", "blue", "top", "Aatrox"},
	{"Peanut", "blue", "jungle", "Sejuani"},
	{"Zeka", "blue", "mid", "Azir"},
	{"Viper", "blue", "bot", "Kaisa"},
	{"Delight", "blue", "support", "Rell"},
	{"Kiin", "red", "top", "Renekton"},
	{"Canyon", "red", "jungle", "Vi"},
	{"Chovy", "red", "mid", "Hwei"},
	{"Ruler", "red", "bot", "Varus"},
	{"Duro", "red", "support", "Nautilus"},
}

// lecRoster — Movistar KOI vs G2, LEC 2025 Summer Playoffs Finals game 1.
var lecRoster = []rosterEntry{
	{"Myrwn", "blue", "top", "Gnar"},
	{"Elyoya", "blue", "jungle", "Maokai"},
	{"Jojopyun", "blue", "mid", "Orianna"},
	{"Supa", "blue", "bot", "Ezreal"},
	{"Alvaro", "blue", "support", "Braum"},
	{"BrokenBlade", "red", "top", "KSante"},
	{"SkewMond", "red", "jungle", "Skarner"},
	{"Caps", "red", "mid", "Sylas"},
	{"Hans Sama", "red", "bot", "Jhin"},
	{"Mikyx", "red", "support", "Leona"},
}

const seedTime = "2025-08-31T18:00:00+00:00"

// detUUID derives a stable RFC-4122-shaped lowercase-hex UUID from a label so
// players/scores get deterministic ids across boots (the mirror is rebuilt
// in-memory every start; ids must not drift between runs).
func detUUID(label string) string {
	h := sha1.Sum([]byte("zab-datasidecar:" + label)) //nolint:gosec
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5-ish nibble
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	s := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[0:8], s[8:12], s[12:16], s[16:20], s[20:32])
}

// seedAll populates the truth + ranking mirrors with the live-test data
// (LCK HLE vs Gen.G, LEC MKOI vs G2, the split + per-player scores). It is
// the embedded form of ZabTruth/scripts/seed_draft_test_data.sh: same teams,
// same score cycle, but the data is FROZEN in-repo (no Leaguepedia import on
// loopback — embedded-local has zero outbound infra).
func seedAll(ctx context.Context, dbs map[string]*sql.DB) error {
	truth := dbs["truth"]
	ranking := dbs["ranking"]

	if err := seedGame(ctx, truth, lckMatchID, "LCK", "HLE", "Gen.G", lckRoster); err != nil {
		return err
	}
	if err := seedGame(ctx, truth, lecMatchID, "LEC", "Movistar KOI", "G2 Esports", lecRoster); err != nil {
		return err
	}

	// ZabRanking split.
	if err := insertRow(ctx, ranking, "splits", map[string]any{
		"id": splitID, "name": "Draft Test 2025",
		"tournament":  "LCK/LEC 2025 Playoffs (draft overlay test)",
		"is_active":   1,
		"description": "Seed split for embedded-local draft overlay test (#225)",
		"created_at":  seedTime, "updated_at": seedTime,
	}); err != nil {
		return err
	}
	// Per-player scores, score cycle per match (mirrors rate_match).
	if err := seedScores(ctx, ranking, lckMatchID, lckRoster); err != nil {
		return err
	}
	if err := seedScores(ctx, ranking, lecMatchID, lecRoster); err != nil {
		return err
	}
	return nil
}

func seedGame(ctx context.Context, truth *sql.DB, matchID, league, blueTeam, redTeam string, roster []rosterEntry) error {
	if err := insertRow(ctx, truth, "matches", map[string]any{
		"id": matchID, "league": league,
		"tournament": league + " 2025 Season Playoffs",
		"split":      "Playoffs",
		"blue_team":  blueTeam, "red_team": redTeam,
		"winner_side": "blue", "played_at": seedTime,
		"patch":      "15.18",
		"created_at": seedTime, "updated_at": seedTime,
	}); err != nil {
		return fmt.Errorf("seed match %s: %w", league, err)
	}
	for _, e := range roster {
		playerID := detUUID(e.Summoner)
		if err := insertRow(ctx, truth, "players", map[string]any{
			"id": playerID, "summoner_name": e.Summoner,
			"primary_role": e.Role, "team": teamOf(e.Side, blueTeam, redTeam),
			"created_at": seedTime, "updated_at": seedTime,
		}); err != nil {
			// players are shared handles; ignore a duplicate insert across
			// games (same player can appear twice — UNIQUE id PK guards it).
			if !isDupPK(err) {
				return fmt.Errorf("seed player %s: %w", e.Summoner, err)
			}
		}
		if err := insertRow(ctx, truth, "match_players", map[string]any{
			"id": detUUID(matchID + ":" + e.Summoner), "match_id": matchID,
			"player_id": playerID, "side": e.Side, "role": e.Role,
			"champion": e.Champion, "win": boolInt(e.Side == "blue"),
			"kills": 3, "deaths": 2, "assists": 5, "cs": 250, "gold": 12000,
			"damage_dealt": 18000, "damage_taken": 15000,
		}); err != nil {
			return fmt.Errorf("seed match_player %s: %w", e.Summoner, err)
		}
	}
	return nil
}

func seedScores(ctx context.Context, ranking *sql.DB, matchID string, roster []rosterEntry) error {
	for i, e := range roster {
		if err := insertRow(ctx, ranking, "player_scores", map[string]any{
			"id": detUUID("score:" + matchID + ":" + e.Summoner), "split_id": splitID,
			"match_id": matchID, "player_id": detUUID(e.Summoner),
			"score":      scoreCycle[i%len(scoreCycle)],
			"comment":    fmt.Sprintf("%s %s %s", e.Side, e.Role, e.Champion),
			"created_at": seedTime, "updated_at": seedTime,
		}); err != nil {
			return fmt.Errorf("seed score %s: %w", e.Summoner, err)
		}
	}
	return nil
}

func teamOf(side, blueTeam, redTeam string) string {
	if side == "blue" {
		return blueTeam
	}
	return redTeam
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isDupPK reports whether the error is a SQLite UNIQUE/PK violation (a player
// seeded by an earlier game). modernc.org/sqlite surfaces it as a message.
func isDupPK(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
