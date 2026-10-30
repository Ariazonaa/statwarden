package auth

// Role is a user's authorization level. The string values match the
// users.role CHECK constraint in the schema.
type Role string

// The three roles, ordered by privilege.
const (
	RoleViewer Role = "viewer" // read-only
	RoleUser   Role = "user"   // may create and edit monitors
	RoleAdmin  Role = "admin"  // full administration (users, settings)
)

// String returns the role's wire value.
func (r Role) String() string { return string(r) }

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleViewer, RoleUser, RoleAdmin:
		return true
	}
	return false
}

// rank orders roles for privilege comparisons.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleUser:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether r is at least as privileged as min. A viewer
// satisfies viewer; a user satisfies viewer and user; an admin satisfies all.
func (r Role) AtLeast(min Role) bool {
	return r.rank() >= min.rank()
}
