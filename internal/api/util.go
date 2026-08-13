package api

import "github.com/google/uuid"

// parseUUID parses s as a UUID, returning ok=false on any malformed
// input — the single validation seam every path/body id field goes
// through before use.
func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
}
