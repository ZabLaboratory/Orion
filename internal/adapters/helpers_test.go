package adapters

import "github.com/ZabLaboratory/Orion/internal/auth"

// identityForTest is a tiny constructor used by adapter tests; lives
// outside _test.go so it can be reused if more adapter tests land.
func identityForTest(role, userID string, paths []string) auth.Identity {
	return auth.Identity{
		UserID: userID,
		Role:   auth.Role(role),
		Paths:  paths,
	}
}
