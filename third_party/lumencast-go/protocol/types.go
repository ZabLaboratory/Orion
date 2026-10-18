package protocol

// Role is the connection-level authority assigned by token validation.
// Roles are enforced by the server kit ; runtimes never assume a role
// — they just react to the frames they receive.
type Role string

const (
	// RoleViewer receives snapshot/delta/scene_changed. Cannot send
	// input frames.
	RoleViewer Role = "viewer"

	// RoleOperator can send input frames in the __inputs.* namespace.
	RoleOperator Role = "operator"

	// RoleService can send input frames in __inputs.*, optionally
	// scoped further by a `paths` claim on the token.
	RoleService Role = "service"

	// RoleTest is for test-mode subscriptions. Can write __test.* and
	// (depending on server policy) __inputs.*.
	RoleTest Role = "test"
)

// IsValid reports whether r is one of the four LSDP/1 roles.
func (r Role) IsValid() bool {
	switch r {
	case RoleViewer, RoleOperator, RoleService, RoleTest:
		return true
	}
	return false
}

// CanWriteInputs reports whether the role is permitted to mutate the
// __inputs.* namespace. Servers MUST ALSO check the `paths` claim for
// service tokens — see server.Identity.
func (r Role) CanWriteInputs() bool {
	return r == RoleOperator || r == RoleService
}

// CanWriteTest reports whether the role is permitted to mutate the
// __test.* namespace. Only test sessions qualify.
func (r Role) CanWriteTest() bool {
	return r == RoleTest
}
