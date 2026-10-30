package api

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/push"
)

// MemStore is a concurrency-safe in-memory Store. It backs the handler tests
// and database-less DEV_MODE (so the API is fully exercisable without
// PostgreSQL); PgStore is the production implementation.
type MemStore struct {
	mu sync.Mutex

	seq         int64
	users       map[int64]*Credentials
	sessions    map[string]Session
	apiKeys     map[int64]APIKey
	badgeTokens map[int64]string // monitorID -> opaque badge token
	identities  map[int64]ExternalIdentity
	resets      map[int64]PasswordReset
	monitors    map[int64]domain.Monitor
	// binned holds soft-deleted monitors, MOVED out of `monitors` rather than
	// flagged in place. That is the whole trick: every existing read walks
	// `monitors`, so a binned monitor disappears from all of them without a
	// single one of those reads gaining a filter it could later forget. Its
	// heartbeats, tags and channel links stay exactly where they are, which is
	// what makes a restore restore the history too.
	binned       map[int64]domain.Monitor
	binnedAt     map[int64]time.Time
	monGroups    map[int64]MonitorGroup
	tags         map[int64]Tag
	notifs       map[int64]Notification
	maint        map[int64]MaintenanceWindow
	pages        map[int64]StatusPage
	pageGroups   map[int64][]StatusPageGroup        // pageID -> groups
	pageMonitors map[int64][]StatusPageMonitorEntry // pageID -> entries
	incidents    map[int64]Incident
	incUpdates   map[int64][]IncidentUpdate // incidentID -> updates
	subscribers  map[int64]Subscriber
	settings     map[string]Setting
	audit        []AuditEntry
	beats        map[int64][]HeartbeatView
	monTags      map[int64][]int64
	monNotifs    map[int64][]int64

	now func() time.Time
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{
		users:        map[int64]*Credentials{},
		sessions:     map[string]Session{},
		apiKeys:      map[int64]APIKey{},
		badgeTokens:  map[int64]string{},
		identities:   map[int64]ExternalIdentity{},
		resets:       map[int64]PasswordReset{},
		monitors:     map[int64]domain.Monitor{},
		binned:       map[int64]domain.Monitor{},
		binnedAt:     map[int64]time.Time{},
		monGroups:    map[int64]MonitorGroup{},
		tags:         map[int64]Tag{},
		notifs:       map[int64]Notification{},
		maint:        map[int64]MaintenanceWindow{},
		pages:        map[int64]StatusPage{},
		pageGroups:   map[int64][]StatusPageGroup{},
		pageMonitors: map[int64][]StatusPageMonitorEntry{},
		incidents:    map[int64]Incident{},
		incUpdates:   map[int64][]IncidentUpdate{},
		subscribers:  map[int64]Subscriber{},
		settings:     map[string]Setting{},
		beats:        map[int64][]HeartbeatView{},
		monTags:      map[int64][]int64{},
		monNotifs:    map[int64][]int64{},
		now:          time.Now,
	}
}

func (m *MemStore) nextID() int64 { m.seq++; return m.seq }

// --- users ---

func (m *MemStore) CountUsers(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

func (m *MemStore) CreateUser(_ context.Context, email, username, hash, displayName, role string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Mirror both unique indexes on users: lower(email) and lower(username).
	for _, c := range m.users {
		if strings.EqualFold(c.Email, email) || strings.EqualFold(c.Username, username) {
			return User{}, ErrConflict
		}
	}
	id := m.nextID()
	now := m.now()
	u := User{ID: id, Email: email, Username: username, DisplayName: displayName, Role: roleOf(role), IsActive: true, CreatedAt: now, UpdatedAt: now}
	m.users[id] = &Credentials{User: u, PasswordHash: hash}
	return u, nil
}

// StoredTOTPSecrets implements Store.
func (m *MemStore) StoredTOTPSecrets(_ context.Context) (map[int64]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int64]string{}
	for id, c := range m.users {
		if c.TOTPSecret != "" {
			out[id] = c.TOTPSecret
		}
	}
	return out, nil
}

// UpdateTOTPSecret implements Store.
func (m *MemStore) UpdateTOTPSecret(_ context.Context, userID int64, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[userID]
	if !ok {
		return ErrNotFound
	}
	c.TOTPSecret = secret
	c.UpdatedAt = m.now()
	return nil
}

// CreateUserWithIdentity implements Store. PgStore uses a transaction; the
// mutex held here is the equivalent, so a partial provisioning is impossible in
// both.
func (m *MemStore) CreateUserWithIdentity(
	ctx context.Context, email, username, displayName, role string, id ExternalIdentity,
) (User, error) {
	u, err := m.CreateUser(ctx, email, username, "", displayName, role)
	if err != nil {
		return User{}, err
	}
	id.UserID = u.ID
	if _, err := m.CreateIdentity(ctx, id); err != nil {
		// Undo the user, since there is no transaction to roll back. An account
		// with no way in is worse than no account: it holds the address.
		m.mu.Lock()
		delete(m.users, u.ID)
		m.mu.Unlock()
		return User{}, err
	}
	return u, nil
}

// CreateInitialAdmin creates the first-run admin atomically, mirroring
// PgStore.CreateInitialAdmin. The whole check-then-insert runs under the one
// mutex — this store's stand-in for the advisory-locked transaction — so two
// concurrent setups cannot both see an empty table and each create an admin;
// the loser gets ErrConflict.
func (m *MemStore) CreateInitialAdmin(_ context.Context, email, username, hash, displayName string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 {
		return User{}, ErrConflict
	}
	id := m.nextID()
	now := m.now()
	u := User{ID: id, Email: email, Username: username, DisplayName: displayName, Role: auth.RoleAdmin, IsActive: true, CreatedAt: now, UpdatedAt: now}
	m.users[id] = &Credentials{User: u, PasswordHash: hash}
	return u, nil
}

// activeAdminsAfterLocked counts the active admins the store would hold if user
// overrideID had role overrideRole and active overrideActive. It is the shared
// core of the last-active-admin guard; a delete is expressed by overriding the
// victim to a non-admin. The caller holds m.mu.
func (m *MemStore) activeAdminsAfterLocked(overrideID int64, overrideRole auth.Role, overrideActive bool) int {
	n := 0
	for uid, c := range m.users {
		role, active := c.Role, c.IsActive
		if uid == overrideID {
			role, active = overrideRole, overrideActive
		}
		if role == auth.RoleAdmin && active {
			n++
		}
	}
	return n
}

func (m *MemStore) GetUserByID(_ context.Context, id int64) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return c.User, nil
}

func (m *MemStore) GetCredentialsByEmail(_ context.Context, email string) (Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.users {
		if strings.EqualFold(c.Email, email) {
			return *c, nil
		}
	}
	return Credentials{}, ErrNotFound
}

// GetCredentialsByLogin resolves an account by e-mail OR username, matched
// case-insensitively, mirroring the PgStore's `lower(email)=lower($1) OR
// lower(username)=lower($1)`. Each column is uniquely indexed, so at most one
// row matches within a column; a cross-column tie (one user's e-mail equal to
// another's username) resolves to the first found, as QueryRow would.
func (m *MemStore) GetCredentialsByLogin(_ context.Context, login string) (Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.users {
		if strings.EqualFold(c.Email, login) || strings.EqualFold(c.Username, login) {
			return *c, nil
		}
	}
	return Credentials{}, ErrNotFound
}

func (m *MemStore) GetCredentialsByID(_ context.Context, id int64) (Credentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return *c, nil
}

func (m *MemStore) ListUsers(context.Context) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]User, 0, len(m.users))
	for _, c := range m.users {
		out = append(out, c.User)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateUser(_ context.Context, id int64, email, username, displayName, role string, isActive bool) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	for oid, other := range m.users {
		if oid != id && (strings.EqualFold(other.Email, email) || strings.EqualFold(other.Username, username)) {
			return User{}, ErrConflict
		}
	}
	c.Email, c.Username, c.DisplayName, c.Role, c.IsActive = email, username, displayName, roleOf(role), isActive
	c.UpdatedAt = m.now()
	return c.User, nil
}

// UpdateUserGuarded mirrors PgStore.UpdateUserGuarded: it applies the update
// only when at least one active admin would remain afterwards, all under the
// single mutex that serialises this store's writes. Returns ErrLastAdmin
// otherwise.
func (m *MemStore) UpdateUserGuarded(_ context.Context, id int64, email, username, displayName, role string, isActive bool) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	for oid, other := range m.users {
		if oid != id && (strings.EqualFold(other.Email, email) || strings.EqualFold(other.Username, username)) {
			return User{}, ErrConflict
		}
	}
	newRole := roleOf(role)
	if m.activeAdminsAfterLocked(id, newRole, isActive) == 0 {
		return User{}, ErrLastAdmin
	}
	c.Email, c.Username, c.DisplayName, c.Role, c.IsActive = email, username, displayName, newRole, isActive
	c.UpdatedAt = m.now()
	return c.User, nil
}

func (m *MemStore) UpdateUserPassword(_ context.Context, id int64, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	c.PasswordHash = hash
	c.UpdatedAt = m.now()
	return nil
}

func (m *MemStore) SetUserTOTP(_ context.Context, id int64, secret string, enabled bool, codes []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	c.TOTPSecret, c.TOTPEnabled, c.BackupCodes = secret, enabled, codes
	c.UpdatedAt = m.now()
	return nil
}

// SetUserAvatar updates only the avatar path and reports the path it replaced.
// It mirrors PgStore.SetUserAvatar and is the only writer of AvatarPath.
func (m *MemStore) SetUserAvatar(_ context.Context, id int64, avatarPath string) (User, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return User{}, "", ErrNotFound
	}
	// Read and write under the same lock, as PgStore does under the row lock, so
	// overlapping uploads each learn the path they actually displaced. The raw
	// stored value is returned: it names a file on disk.
	replaced := c.AvatarPath
	c.AvatarPath = avatarPath
	c.UpdatedAt = m.now()
	return c.User, replaced, nil
}

func (m *MemStore) DeleteUser(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteUserLocked(id)
}

// DeleteUserGuarded mirrors PgStore.DeleteUserGuarded: it removes the user only
// when at least one active admin survives, all under the store's single mutex.
// The victim is treated as already gone (a non-admin) for the count. Returns
// ErrLastAdmin otherwise.
func (m *MemStore) DeleteUserGuarded(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[id]; !ok {
		return ErrNotFound
	}
	if m.activeAdminsAfterLocked(id, auth.RoleViewer, false) == 0 {
		return ErrLastAdmin
	}
	return m.deleteUserLocked(id)
}

// deleteUserLocked removes a user and the rows the schema cascades. The caller
// holds m.mu.
func (m *MemStore) deleteUserLocked(id int64) error {
	if _, ok := m.users[id]; !ok {
		return ErrNotFound
	}
	delete(m.users, id)
	// The schema declares ON DELETE CASCADE for these, so a redeemable reset token
	// or a linked identity must not survive its owner here either.
	for iid, e := range m.identities {
		if e.UserID == id {
			delete(m.identities, iid)
		}
	}
	for rid, r := range m.resets {
		if r.UserID == id {
			delete(m.resets, rid)
		}
	}
	return nil
}

// --- sessions ---

func (m *MemStore) CreateSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *MemStore) GetSession(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return s, nil
}

func (m *MemStore) TouchSession(_ context.Context, id string, lastSeen, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	s.LastSeenAt, s.ExpiresAt = lastSeen, expires
	m.sessions[id] = s
	return nil
}

func (m *MemStore) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *MemStore) ListSessionsByUser(_ context.Context, userID int64) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Session{}
	for _, s := range m.sessions {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) DeleteSessionsByUser(_ context.Context, userID int64, exceptID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.UserID == userID && id != exceptID {
			delete(m.sessions, id)
		}
	}
	return nil
}

// ChangePassword implements Store. PgStore does the three writes in one
// transaction; here they are simply done together under the same lock, which is
// the closest equivalent and gives tests the same all-or-nothing behaviour.
func (m *MemStore) ChangePassword(_ context.Context, id int64, hash, keepSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	c.PasswordHash = hash
	c.UpdatedAt = m.now()
	m.users[id] = c
	for sid, sess := range m.sessions {
		if sess.UserID == id && sid != keepSessionID {
			delete(m.sessions, sid)
		}
	}
	// API keys too — see PgStore.ChangePassword. A key outlives every session by
	// design and its owner cannot see it, so leaving it behind makes "all your
	// other sessions were ended" a false reassurance.
	for kid, k := range m.apiKeys {
		if k.UserID == id {
			delete(m.apiKeys, kid)
		}
	}
	m.invalidateResetsLocked(id)
	return nil
}

// InvalidatePasswordResets implements Store.
func (m *MemStore) InvalidatePasswordResets(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalidateResetsLocked(userID)
	return nil
}

func (m *MemStore) invalidateResetsLocked(userID int64) {
	now := m.now()
	for rid, r := range m.resets {
		if r.UserID == userID && r.UsedAt == nil {
			r.UsedAt = &now
			m.resets[rid] = r
		}
	}
}

// --- api keys ---

func (m *MemStore) CreateAPIKey(_ context.Context, k APIKey) (APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k.ID = m.nextID()
	k.CreatedAt = m.now()
	m.apiKeys[k.ID] = k
	return k, nil
}

func (m *MemStore) GetAPIKeyByHash(_ context.Context, hash string) (APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.apiKeys {
		if k.KeyHash == hash {
			return k, nil
		}
	}
	return APIKey{}, ErrNotFound
}

func (m *MemStore) ListAPIKeys(_ context.Context, userID int64) ([]APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []APIKey{}
	for _, k := range m.apiKeys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) TouchAPIKey(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.apiKeys[id]
	if !ok {
		return ErrNotFound
	}
	t := at
	k.LastUsedAt = &t
	m.apiKeys[id] = k
	return nil
}

func (m *MemStore) DeleteAPIKey(_ context.Context, id, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.apiKeys[id]
	if !ok || k.UserID != userID {
		return ErrNotFound
	}
	delete(m.apiKeys, id)
	return nil
}

func (m *MemStore) DeleteAPIKeysByUser(_ context.Context, userID int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, k := range m.apiKeys {
		if k.UserID == userID {
			delete(m.apiKeys, id)
			n++
		}
	}
	return n, nil
}

// --- external identities ---

func (m *MemStore) GetIdentity(_ context.Context, issuer, subject string) (ExternalIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if issuer == "" || subject == "" {
		return ExternalIdentity{}, ErrNotFound
	}
	for _, e := range m.identities {
		if e.Issuer == issuer && e.Subject == subject {
			return e, nil
		}
	}
	return ExternalIdentity{}, ErrNotFound
}

func (m *MemStore) CreateIdentity(_ context.Context, in ExternalIdentity) (ExternalIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[in.UserID]; !ok {
		return ExternalIdentity{}, ErrNotFound
	}
	// Mirrors user_identities_issuer_subject_idx: one external account maps to at
	// most one local user, so a second link attempt is a conflict rather than a
	// silent move.
	for _, e := range m.identities {
		if e.Issuer == in.Issuer && e.Subject == in.Subject {
			return ExternalIdentity{}, ErrConflict
		}
	}
	in.ID = m.nextID()
	if in.CreatedAt.IsZero() {
		in.CreatedAt = m.now()
	}
	m.identities[in.ID] = in
	return in, nil
}

func (m *MemStore) ListIdentitiesByUser(_ context.Context, userID int64) ([]ExternalIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ExternalIdentity{}
	for _, e := range m.identities {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) TouchIdentity(_ context.Context, id int64, at time.Time, email, displayName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.identities[id]
	if !ok {
		return ErrNotFound
	}
	t := at
	e.LastLoginAt, e.Email, e.DisplayName = &t, email, displayName
	m.identities[id] = e
	return nil
}

func (m *MemStore) DeleteIdentity(_ context.Context, id, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.identities[id]
	if !ok || e.UserID != userID {
		return ErrNotFound
	}
	delete(m.identities, id)
	return nil
}

// --- password resets ---

func (m *MemStore) CreatePasswordReset(_ context.Context, r PasswordReset) (PasswordReset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.TokenHash == "" {
		return PasswordReset{}, ErrNotFound
	}
	for _, existing := range m.resets {
		if existing.TokenHash == r.TokenHash {
			return PasswordReset{}, ErrConflict
		}
	}
	r.ID = m.nextID()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = m.now()
	}
	m.resets[r.ID] = r
	return r, nil
}

func (m *MemStore) ConsumePasswordReset(_ context.Context, tokenHash, passwordHash string, now time.Time) (PasswordReset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tokenHash == "" {
		return PasswordReset{}, ErrNotFound
	}
	var found *PasswordReset
	for _, r := range m.resets {
		if r.TokenHash == tokenHash {
			found = &r
			break
		}
	}
	// Unknown, already-used and expired are one indistinguishable outcome, so the
	// handler cannot accidentally answer differently and reveal that a token
	// existed.
	if found == nil || found.UsedAt != nil || !found.ExpiresAt.After(now) {
		return PasswordReset{}, ErrNotFound
	}
	c, ok := m.users[found.UserID]
	if !ok {
		return PasswordReset{}, ErrNotFound
	}
	// The mutex is this store's equivalent of PgStore's transaction: marking the
	// token used and writing the password cannot interleave.
	t := now
	found.UsedAt = &t
	m.resets[found.ID] = *found
	c.PasswordHash = passwordHash
	c.UpdatedAt = now
	// A stale mail in an old inbox must not be replayable after recovery.
	for id, other := range m.resets {
		if other.UserID == found.UserID && other.UsedAt == nil {
			other.UsedAt = &t
			m.resets[id] = other
		}
	}
	return *found, nil
}

// --- monitors ---

func (m *MemStore) CreateMonitor(_ context.Context, in MonitorInput) (domain.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID()
	mon := monitorFromInput(id, in)
	mon.CreatedAt = m.now()
	// Mirror PgStore.CreateMonitor: a push monitor is minted a token as part of
	// the create, only its HASH is stored, and the plaintext is revealed exactly
	// once through this response. Without this the two stores disagreed about
	// the whole token lifecycle, and since every test runs on MemStore the
	// lifecycle had no coverage at all.
	hash, plain, err := reconcilePushToken(in.Type, nil)
	if err != nil {
		return domain.Monitor{}, err
	}
	mon.PushToken = strDeref(hash)
	m.monitors[id] = mon
	m.monTags[id] = append([]int64(nil), in.TagIDs...)
	m.monNotifs[id] = append([]int64(nil), in.NotificationIDs...)
	mon.PushToken = plain
	return mon, nil
}

func (m *MemStore) GetMonitor(_ context.Context, id int64) (domain.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.monitors[id]
	if !ok {
		return domain.Monitor{}, ErrNotFound
	}
	return mon, nil
}

func (m *MemStore) ListMonitors(_ context.Context, f MonitorFilter, p Page) ([]domain.Monitor, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []domain.Monitor
	for _, mon := range m.monitors {
		if f.Status != "" && string(mon.Status) != f.Status {
			continue
		}
		if f.Type != "" && mon.Type != f.Type {
			continue
		}
		if f.Search != "" && !strings.Contains(strings.ToLower(mon.Name), strings.ToLower(f.Search)) {
			continue
		}
		if f.TagID != nil && !containsID(m.monTags[mon.ID], *f.TagID) {
			continue
		}
		if f.GroupID != nil && !monitorInGroup(mon, *f.GroupID) {
			continue
		}
		all = append(all, mon)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	total := len(all)
	return paginate(all, p), total, nil
}

func (m *MemStore) UpdateMonitor(_ context.Context, id int64, in MonitorInput) (domain.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.monitors[id]
	if !ok {
		return domain.Monitor{}, ErrNotFound
	}
	// Mirror PgStore's in-transaction cycle check. Here the store's own mutex
	// is the serialisation the advisory lock provides there, so the two reach
	// the same guarantee by different means — and a MemStore-backed test can
	// still see ErrParentCycle, which the whole Go suite runs against.
	if in.ParentID != nil {
		if err := m.parentCycleLocked(id, *in.ParentID); err != nil {
			return domain.Monitor{}, err
		}
	}
	mon := monitorFromInput(id, in)
	// status follows active, mirroring PgStore's CASE on the same UPDATE.
	//
	// Keeping the previous status unconditionally is what made the PgStore
	// omission invisible: a monitor deactivated through a full-replace PUT kept
	// reporting "up" in both stores, so no test could tell them apart or catch
	// either one.
	mon.Status = existing.Status
	if !mon.Active {
		mon.Status = domain.StatusPaused
	}
	mon.CreatedAt = existing.CreatedAt
	// Mirror PgStore.UpdateMonitor: becoming a push monitor mints a token,
	// LEAVING the push type revokes it, and an existing one is kept. Keeping the
	// old value unconditionally (what this did) left a revoked credential live in
	// DEV_MODE and hid the revoke path from every test.
	hash, _, err := reconcilePushToken(in.Type, nilIfEmpty(existing.PushToken))
	if err != nil {
		return domain.Monitor{}, err
	}
	mon.PushToken = strDeref(hash)
	m.monitors[id] = mon
	m.monTags[id] = append([]int64(nil), in.TagIDs...)
	m.monNotifs[id] = append([]int64(nil), in.NotificationIDs...)
	return mon, nil
}

// DeleteMonitor moves a monitor to the bin. See the Store interface and
// migration 0018 for why deleting is reversible and why the monitor comes back
// paused.
//
// The tags, channel links and heartbeats are deliberately NOT dropped: they are
// what a restore restores, and dropping them would make this a hard delete with
// a delay. `active` and `status` are cleared to mirror PgStore, where those two
// columns are what actually removes the monitor from the scheduler.
func (m *MemStore) DeleteMonitor(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.monitors[id]
	if !ok {
		return ErrNotFound
	}
	mon.Active = false
	mon.Status = domain.StatusPaused
	delete(m.monitors, id)
	m.binned[id] = mon
	m.binnedAt[id] = m.now()
	return nil
}

func (m *MemStore) GetDeletedMonitor(_ context.Context, id int64) (domain.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.binned[id]
	if !ok {
		return domain.Monitor{}, ErrNotFound
	}
	return mon, nil
}

func (m *MemStore) ListDeletedMonitors(_ context.Context) ([]domain.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Monitor, 0, len(m.binned))
	for _, mon := range m.binned {
		out = append(out, mon)
	}
	// Most recently deleted first, matching PgStore's ORDER BY deleted_at DESC,
	// id DESC — with the id as the same tie-break, because a bulk delete bins a
	// whole selection on one clock reading.
	sort.Slice(out, func(i, j int) bool {
		ti, tj := m.binnedAt[out[i].ID], m.binnedAt[out[j].ID]
		if ti.Equal(tj) {
			return out[i].ID > out[j].ID
		}
		return ti.After(tj)
	})
	return out, nil
}

func (m *MemStore) RestoreMonitor(_ context.Context, id int64) (domain.Monitor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.binned[id]
	if !ok {
		return domain.Monitor{}, ErrNotFound
	}
	// Paused, always. Neither store can know what Active was before the delete.
	mon.Active = false
	mon.Status = domain.StatusPaused
	delete(m.binned, id)
	delete(m.binnedAt, id)
	m.monitors[id] = mon
	return mon, nil
}

// PurgeMonitor hard-deletes a BINNED monitor. Refusing a live one is the point:
// the only route to an irreversible delete runs through the bin.
func (m *MemStore) PurgeMonitor(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.binned[id]; !ok {
		return ErrNotFound
	}
	m.hardDeleteMonitorLocked(id)
	return nil
}

// hardDeleteMonitorLocked removes a binned monitor and everything hanging off
// it, standing in for the schema's ON DELETE CASCADE. The caller holds m.mu.
//
// The list has to be COMPLETE, and it was not. Postgres enforces its cascades;
// MemStore is the only store the test suite ever runs against, so anything left
// out here is a divergence that no Go test can see (CLAUDE.md). What was left
// behind: a status-page placement and a maintenance window pointing at a
// monitor that no longer exists — which is exactly the dangling reference the
// public status page used to render as a blank component — plus a badge token
// still resolving to a dead id, and a child monitor whose parent_id names a
// row that is gone.
func (m *MemStore) hardDeleteMonitorLocked(id int64) {
	delete(m.binned, id)
	delete(m.binnedAt, id)
	delete(m.monTags, id)
	delete(m.monNotifs, id)
	delete(m.beats, id)
	delete(m.badgeTokens, id)

	// monitors.parent_id is ON DELETE SET NULL, not CASCADE: destroying a
	// parent must not destroy the children that happened to be grouped under
	// it.
	for cid, child := range m.monitors {
		if child.ParentID != nil && *child.ParentID == id {
			child.ParentID = nil
			m.monitors[cid] = child
		}
	}
	for cid, child := range m.binned {
		if child.ParentID != nil && *child.ParentID == id {
			child.ParentID = nil
			m.binned[cid] = child
		}
	}

	// status_page_monitors, both projections. The placement outlives a SOFT
	// delete on purpose — that is what makes a restore put the monitor back on
	// its page — but a purge is the end of the monitor, so the placement goes
	// with it.
	for pid, entries := range m.pageMonitors {
		kept := entries[:0:0]
		for _, e := range entries {
			if e.MonitorID != id {
				kept = append(kept, e)
			}
		}
		m.pageMonitors[pid] = kept
		if p, ok := m.pages[pid]; ok {
			p.Monitors = append([]StatusPageMonitorEntry(nil), kept...)
			p.MonitorIDs = entryMonitorIDs(kept)
			m.pages[pid] = p
		}
	}

	// maintenance_monitors and incident_monitors.
	for wid, w := range m.maint {
		if next := withoutID(w.MonitorIDs, id); next != nil {
			w.MonitorIDs = next
			m.maint[wid] = w
		}
	}
	for iid, in := range m.incidents {
		if next := withoutID(in.MonitorIDs, id); next != nil {
			in.MonitorIDs = next
			m.incidents[iid] = in
		}
	}
}

// withoutID returns ids with one entry removed, or nil when it was not there —
// so a caller can skip writing back the overwhelmingly common no-op.
func withoutID(ids []int64, drop int64) []int64 {
	found := false
	out := make([]int64, 0, len(ids))
	for _, v := range ids {
		if v == drop {
			found = true
			continue
		}
		out = append(out, v)
	}
	if !found {
		return nil
	}
	return out
}

func (m *MemStore) SetMonitorActive(_ context.Context, id int64, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.monitors[id]
	if !ok {
		return ErrNotFound
	}
	mon.Active = active
	if active {
		mon.Status = domain.StatusPending
	} else {
		mon.Status = domain.StatusPaused
	}
	m.monitors[id] = mon
	return nil
}

func (m *MemStore) MonitorLinks(_ context.Context, ids []int64) (map[int64]MonitorLinks, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64]MonitorLinks, len(ids))
	for _, id := range ids {
		out[id] = MonitorLinks{
			TagIDs:          append([]int64{}, m.monTags[id]...),
			NotificationIDs: append([]int64{}, m.monNotifs[id]...),
		}
	}
	return out, nil
}

func (m *MemStore) MonitorStats(_ context.Context, ids []int64, now time.Time, scope StatsScope) (map[int64]MonitorStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64]MonitorStats, len(ids))
	for _, id := range ids {
		beats := m.beats[id]
		if len(beats) == 0 {
			continue
		}
		st := MonitorStats{Sparkline: []int{}}
		latest := beats[len(beats)-1]
		checked := latest.CheckedAt
		st.LastCheckedAt = &checked
		if latest.LatencyMs != nil {
			latency := *latest.LatencyMs
			st.LatencyMs = &latency
		}
		st.Uptime24h = uptimeSince(beats, now.Add(-24*time.Hour))
		st.Uptime30d = uptimeSince(beats, now.AddDate(0, 0, -30))
		// Mirrors PgStore: the list scope reports no yearly uptime, so a test
		// that asserts one in a list response fails here too rather than
		// passing against MemStore and surprising the browser.
		if scope == StatsDetail {
			st.Uptime1y = uptimeSince(beats, now.AddDate(-1, 0, 0))
		}
		from := len(beats) - sparklinePoints
		if from < 0 {
			from = 0
		}
		for _, hb := range beats[from:] {
			if hb.LatencyMs != nil {
				st.Sparkline = append(st.Sparkline, *hb.LatencyMs)
			}
		}
		from = len(beats) - recentBeatPoints
		if from < 0 {
			from = 0
		}
		st.RecentBeats = make([]string, 0, len(beats)-from)
		for _, hb := range beats[from:] {
			st.RecentBeats = append(st.RecentBeats, hb.Status)
		}
		out[id] = st
	}
	return out, nil
}

// uptimeSince is the up fraction of the conclusive (up/down) heartbeats at or
// after from, mirroring report.Uptime: pending and maintenance beats are not
// counted either way. It returns nil when the window holds no conclusive beat,
// which the API renders as a null rather than a misleading 0%.
func uptimeSince(beats []HeartbeatView, from time.Time) *float64 {
	up, total := 0, 0
	for _, hb := range beats {
		if hb.CheckedAt.Before(from) {
			continue
		}
		switch hb.Status {
		case string(domain.StatusUp):
			up++
			total++
		case string(domain.StatusDown):
			total++
		}
	}
	if total == 0 {
		return nil
	}
	ratio := float64(up) / float64(total)
	return &ratio
}

// monitorInGroup reports whether mon is filed under groupID, where
// GroupUnassigned means "filed under no folder".
func monitorInGroup(mon domain.Monitor, groupID int64) bool {
	if groupID == GroupUnassigned {
		return mon.GroupID == nil
	}
	return mon.GroupID != nil && *mon.GroupID == groupID
}

func (m *MemStore) SetMonitorGroup(_ context.Context, id int64, groupID *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.monitors[id]
	if !ok {
		return ErrNotFound
	}
	mon.GroupID = groupID
	m.monitors[id] = mon
	return nil
}

// AddMonitorTags is additive and idempotent, mirroring the PgStore's
// ON CONFLICT DO NOTHING insert.
func (m *MemStore) AddMonitorTags(_ context.Context, id int64, tagIDs []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.monitors[id]; !ok {
		return ErrNotFound
	}
	for _, tagID := range tagIDs {
		if !containsID(m.monTags[id], tagID) {
			m.monTags[id] = append(m.monTags[id], tagID)
		}
	}
	return nil
}

// AddMonitorNotifications is additive and idempotent, mirroring AddMonitorTags
// and the PgStore's ON CONFLICT DO NOTHING insert.
func (m *MemStore) AddMonitorNotifications(_ context.Context, id int64, notificationIDs []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.monitors[id]; !ok {
		return ErrNotFound
	}
	for _, nid := range notificationIDs {
		if !containsID(m.monNotifs[id], nid) {
			m.monNotifs[id] = append(m.monNotifs[id], nid)
		}
	}
	return nil
}

// SetMonitorSchedule mirrors PgStore.SetMonitorSchedule: it sets the interval
// and applies each optional override only when present, leaving every other
// field untouched. Durations are stored as time.Duration, so the incoming
// seconds are widened here exactly as scanMonitor widens the columns.
func (m *MemStore) SetMonitorSchedule(_ context.Context, id int64, sched MonitorSchedule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.monitors[id]
	if !ok {
		return ErrNotFound
	}
	mon.Interval = time.Duration(sched.Interval) * time.Second
	if sched.Timeout != nil {
		mon.Timeout = time.Duration(*sched.Timeout) * time.Second
	}
	if sched.MaxRetries != nil {
		mon.MaxRetries = *sched.MaxRetries
	}
	if sched.RetryInterval != nil {
		mon.RetryInterval = time.Duration(*sched.RetryInterval) * time.Second
	}
	m.monitors[id] = mon
	return nil
}

// --- badge tokens ---

func (m *MemStore) EnsureBadgeToken(_ context.Context, monitorID int64, candidate string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tok, ok := m.badgeTokens[monitorID]; ok {
		return tok, nil
	}
	m.badgeTokens[monitorID] = candidate
	return candidate, nil
}

func (m *MemStore) DeleteBadgeToken(_ context.Context, monitorID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.badgeTokens, monitorID)
	return nil
}

func (m *MemStore) MonitorIDByBadgeToken(_ context.Context, token string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, tok := range m.badgeTokens {
		if tok == token {
			return id, nil
		}
	}
	return 0, ErrNotFound
}

// SetPushToken implements pushTokenStore. Like the PgStore it stores only the
// token's hash, mirroring the password-like at-rest treatment so tests exercise
// the same redaction/lookup semantics as production.
func (m *MemStore) SetPushToken(_ context.Context, id int64, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mon, ok := m.monitors[id]
	if !ok {
		return ErrNotFound
	}
	mon.PushToken = push.HashToken(token)
	m.monitors[id] = mon
	return nil
}

// --- monitor groups ---

func (m *MemStore) CreateMonitorGroup(_ context.Context, name string, sortOrder int) (MonitorGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.monGroups {
		if g.Name == name {
			return MonitorGroup{}, ErrConflict
		}
	}
	id := m.nextID()
	g := MonitorGroup{ID: id, Name: name, SortOrder: sortOrder, CreatedAt: m.now()}
	m.monGroups[id] = g
	return g, nil
}

func (m *MemStore) ListMonitorGroups(context.Context) ([]MonitorGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MonitorGroup, 0, len(m.monGroups))
	for _, g := range m.monGroups {
		out = append(out, g)
	}
	// Display order, id as the tie-break — the same ORDER BY the PgStore uses.
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *MemStore) GetMonitorGroup(_ context.Context, id int64) (MonitorGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.monGroups[id]
	if !ok {
		return MonitorGroup{}, ErrNotFound
	}
	return g, nil
}

// parentCycleLocked walks the stored parent chain; the caller holds m.mu.
func (m *MemStore) parentCycleLocked(id, parentID int64) error {
	if parentID == id {
		return ErrParentCycle
	}
	seen := make(map[int64]bool)
	for cur := parentID; ; {
		if cur == id {
			return ErrParentCycle
		}
		if seen[cur] || len(seen) >= maxParentChain {
			return ErrParentCycle
		}
		seen[cur] = true
		mon, ok := m.monitors[cur]
		if !ok {
			return ErrNotFound
		}
		if mon.ParentID == nil {
			return nil
		}
		cur = *mon.ParentID
	}
}

func (m *MemStore) UpdateMonitorGroup(_ context.Context, id int64, name string, sortOrder int) (MonitorGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.monGroups[id]
	if !ok {
		return MonitorGroup{}, ErrNotFound
	}
	for otherID, other := range m.monGroups {
		if otherID != id && other.Name == name {
			return MonitorGroup{}, ErrConflict
		}
	}
	g.Name, g.SortOrder = name, sortOrder
	m.monGroups[id] = g
	return g, nil
}

// DeleteMonitorGroup drops the folder and unfiles its monitors, mirroring the
// schema's ON DELETE SET NULL. The monitors themselves MUST survive: a folder
// is a view onto them, and deleting one is not a way to delete monitors.
func (m *MemStore) DeleteMonitorGroup(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.monGroups[id]; !ok {
		return ErrNotFound
	}
	delete(m.monGroups, id)
	// Both maps, live and binned.
	//
	// The schema's ON DELETE SET NULL does not know this store's recycle bin
	// exists — it clears group_id on EVERY row referencing the folder, including
	// the soft-deleted ones. Clearing only the live map would leave a restored
	// monitor pointing at a folder that no longer exists, in MemStore and not in
	// PgStore: a divergence between the two implementations, which is precisely
	// what this store must not have.
	clearGroup := func(into map[int64]domain.Monitor) {
		for monID, mon := range into {
			if mon.GroupID != nil && *mon.GroupID == id {
				mon.GroupID = nil
				into[monID] = mon
			}
		}
	}
	clearGroup(m.monitors)
	clearGroup(m.binned)
	return nil
}

// --- tags ---

func (m *MemStore) CreateTag(_ context.Context, name, color string) (Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tags {
		if t.Name == name {
			return Tag{}, ErrConflict
		}
	}
	id := m.nextID()
	t := Tag{ID: id, Name: name, Color: color, CreatedAt: m.now()}
	m.tags[id] = t
	return t, nil
}

func (m *MemStore) ListTags(context.Context) ([]Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Tag, 0, len(m.tags))
	for _, t := range m.tags {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetTag(_ context.Context, id int64) (Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tags[id]
	if !ok {
		return Tag{}, ErrNotFound
	}
	return t, nil
}

func (m *MemStore) UpdateTag(_ context.Context, id int64, name, color string) (Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tags[id]
	if !ok {
		return Tag{}, ErrNotFound
	}
	t.Name, t.Color = name, color
	m.tags[id] = t
	return t, nil
}

func (m *MemStore) DeleteTag(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tags[id]; !ok {
		return ErrNotFound
	}
	delete(m.tags, id)
	return nil
}

// --- notifications ---

func (m *MemStore) CreateNotification(_ context.Context, n Notification) (Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID()
	now := m.now()
	n.ID, n.CreatedAt, n.UpdatedAt = id, now, now
	m.notifs[id] = n
	return n, nil
}

func (m *MemStore) ListNotifications(context.Context) ([]Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Notification, 0, len(m.notifs))
	for _, n := range m.notifs {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetNotification(_ context.Context, id int64) (Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.notifs[id]
	if !ok {
		return Notification{}, ErrNotFound
	}
	return n, nil
}

func (m *MemStore) UpdateNotification(_ context.Context, id int64, n Notification) (Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.notifs[id]
	if !ok {
		return Notification{}, ErrNotFound
	}
	n.ID, n.CreatedAt = id, existing.CreatedAt
	n.UpdatedAt = m.now()
	m.notifs[id] = n
	return n, nil
}

func (m *MemStore) DeleteNotification(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.notifs[id]; !ok {
		return ErrNotFound
	}
	delete(m.notifs, id)
	return nil
}

// --- maintenance ---

func (m *MemStore) CreateMaintenance(_ context.Context, w MaintenanceWindow) (MaintenanceWindow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID()
	now := m.now()
	w.ID, w.CreatedAt, w.UpdatedAt = id, now, now
	w.MonitorIDs = idList(w.MonitorIDs)
	m.maint[id] = w
	return hydrateMaintenance(w), nil
}

// hydrateMaintenance returns the window as a caller must see it, mirroring
// hydrateIncident: the monitor scope is empty rather than nil, and it is a copy
// so a caller cannot mutate what the store holds. See idList for why both
// matter — a window with no scope is the fleet-wide case, not a rare one.
func hydrateMaintenance(w MaintenanceWindow) MaintenanceWindow {
	w.MonitorIDs = idList(w.MonitorIDs)
	return w
}

func (m *MemStore) ListMaintenance(context.Context) ([]MaintenanceWindow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MaintenanceWindow, 0, len(m.maint))
	for _, w := range m.maint {
		out = append(out, hydrateMaintenance(w))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetMaintenance(_ context.Context, id int64) (MaintenanceWindow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.maint[id]
	if !ok {
		return MaintenanceWindow{}, ErrNotFound
	}
	return hydrateMaintenance(w), nil
}

func (m *MemStore) UpdateMaintenance(_ context.Context, id int64, w MaintenanceWindow) (MaintenanceWindow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.maint[id]
	if !ok {
		return MaintenanceWindow{}, ErrNotFound
	}
	w.ID, w.CreatedAt = id, existing.CreatedAt
	w.UpdatedAt = m.now()
	w.MonitorIDs = idList(w.MonitorIDs)
	m.maint[id] = w
	return hydrateMaintenance(w), nil
}

func (m *MemStore) DeleteMaintenance(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.maint[id]; !ok {
		return ErrNotFound
	}
	delete(m.maint, id)
	return nil
}

// --- status pages ---

// applyLayout rebuilds the stored groups + monitor entries for a page from the
// incoming StatusPage, matching/creating groups by name. It also refreshes the
// flat MonitorIDs projection in display order.
func (m *MemStore) applyLayout(pageID int64, p *StatusPage) {
	// Build groups (assign IDs), keyed by name.
	byName := map[string]int64{}
	var groups []StatusPageGroup
	for _, g := range p.Groups {
		gid := m.nextID()
		byName[g.Name] = gid
		groups = append(groups, StatusPageGroup{ID: gid, StatusPageID: pageID, Name: g.Name, SortOrder: g.SortOrder})
	}

	// Entries: prefer the rich Monitors list, fall back to flat MonitorIDs.
	var entries []StatusPageMonitorEntry
	if len(p.Monitors) > 0 {
		for i, e := range p.Monitors {
			ent := StatusPageMonitorEntry{MonitorID: e.MonitorID, SortOrder: e.SortOrder, ShowUptime: e.ShowUptime}
			if ent.SortOrder == 0 {
				ent.SortOrder = i
			}
			if e.GroupName != "" {
				if gid, ok := byName[e.GroupName]; ok {
					g := gid
					ent.GroupID = &g
					ent.GroupName = e.GroupName
				}
			} else if e.GroupID != nil {
				ent.GroupID = e.GroupID
			}
			entries = append(entries, ent)
		}
	} else {
		for i, mid := range p.MonitorIDs {
			entries = append(entries, StatusPageMonitorEntry{MonitorID: mid, SortOrder: i, ShowUptime: true})
		}
	}
	m.pageGroups[pageID] = groups
	m.pageMonitors[pageID] = entries

	// Refresh derived projections on the stored page.
	p.Groups = groups
	p.Monitors = entries
	p.MonitorIDs = entryMonitorIDs(entries)
}

// idList returns ids as an empty-but-not-nil slice, copied. It sits next to
// entryMonitorIDs, which PgStore also calls, because both stores need it.
//
// Two jobs, both of which the maintenance path needed:
//
// Empty, never nil, because a nil slice marshals to JSON null while
// openapi.json declares monitor_ids as a non-nullable array — and
// emptySliceAsArray cannot fix it, since that only rewrites a slice which IS
// the whole response body, not one nested inside a record. The empty case is
// the DEFAULT one here rather than an edge: a maintenance window that names no
// monitor applies to every monitor (internal/maintenance.Window), which is how
// fleet-wide downtime is scheduled.
//
// Copied, because MemStore keeps what it is handed. Storing the caller's slice
// verbatim lets a later append by that caller reach inside the store — the same
// reason the incident path already copies.
func idList(ids []int64) []int64 {
	return append([]int64{}, ids...)
}

func entryMonitorIDs(entries []StatusPageMonitorEntry) []int64 {
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.MonitorID)
	}
	return ids
}

func (m *MemStore) hydratePage(p StatusPage) StatusPage {
	p.Groups = append([]StatusPageGroup(nil), m.pageGroups[p.ID]...)
	p.Monitors = append([]StatusPageMonitorEntry(nil), m.pageMonitors[p.ID]...)
	p.MonitorIDs = entryMonitorIDs(p.Monitors)
	p.HasPassword = p.PasswordHash != ""
	// Mirrors scanStatusPage: the stored value stays as written, but nothing
	// leaves the store with a logo path this server did not issue.
	p.LogoPath = sanitizeLogoPath(p.LogoPath)
	return p
}

func (m *MemStore) CreateStatusPage(_ context.Context, p StatusPage) (StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.pages {
		if existing.Slug == p.Slug {
			return StatusPage{}, ErrConflict
		}
		if p.CustomDomain != "" && strings.EqualFold(existing.CustomDomain, p.CustomDomain) {
			return StatusPage{}, ErrConflict
		}
	}
	if p.PasswordHash == passwordClearSentinel {
		p.PasswordHash = ""
	}
	id := m.nextID()
	now := m.now()
	p.ID, p.CreatedAt, p.UpdatedAt = id, now, now
	m.applyLayout(id, &p)
	m.pages[id] = p
	return m.hydratePage(p), nil
}

func (m *MemStore) ListStatusPages(context.Context) ([]StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StatusPage, 0, len(m.pages))
	for _, p := range m.pages {
		out = append(out, m.hydratePage(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetStatusPage(_ context.Context, id int64) (StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[id]
	if !ok {
		return StatusPage{}, ErrNotFound
	}
	return m.hydratePage(p), nil
}

func (m *MemStore) GetStatusPageBySlug(_ context.Context, slug string) (StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pages {
		if p.Slug == slug {
			return m.hydratePage(p), nil
		}
	}
	return StatusPage{}, ErrNotFound
}

func (m *MemStore) GetStatusPageByDomain(_ context.Context, host string) (StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	host = strings.ToLower(strings.TrimSpace(host))
	for _, p := range m.pages {
		if p.CustomDomain != "" && strings.EqualFold(p.CustomDomain, host) {
			return m.hydratePage(p), nil
		}
	}
	return StatusPage{}, ErrNotFound
}

func (m *MemStore) UpdateStatusPage(_ context.Context, id int64, p StatusPage) (StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.pages[id]
	if !ok {
		return StatusPage{}, ErrNotFound
	}
	for oid, other := range m.pages {
		if oid == id {
			continue
		}
		if other.Slug == p.Slug {
			return StatusPage{}, ErrConflict
		}
		if p.CustomDomain != "" && strings.EqualFold(other.CustomDomain, p.CustomDomain) {
			return StatusPage{}, ErrConflict
		}
	}
	p.ID, p.CreatedAt = id, existing.CreatedAt
	p.UpdatedAt = m.now()
	p.ViewCount = existing.ViewCount
	// logo_path is server-owned (written only by SetStatusPageLogo), mirroring
	// the PgStore UPDATE which no longer touches the column.
	p.LogoPath = existing.LogoPath
	// Preserve the existing password unless the caller set a new one or asked
	// to clear it (sentinel).
	switch p.PasswordHash {
	case "":
		p.PasswordHash = existing.PasswordHash
	case passwordClearSentinel:
		p.PasswordHash = ""
	}
	m.applyLayout(id, &p)
	m.pages[id] = p
	return m.hydratePage(p), nil
}

// SetStatusPageLogo updates only the logo path, leaving the rest of the page
// untouched, and reports the path it replaced. It mirrors
// PgStore.SetStatusPageLogo and is the only writer of LogoPath after creation.
func (m *MemStore) SetStatusPageLogo(_ context.Context, id int64, logoPath string) (StatusPage, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[id]
	if !ok {
		return StatusPage{}, "", ErrNotFound
	}
	// Read and write under the same lock, as PgStore does under the row lock,
	// so overlapping uploads each learn the path they actually displaced. The
	// raw stored value is returned: it names a file on disk.
	replaced := p.LogoPath
	p.LogoPath = logoPath
	p.UpdatedAt = m.now()
	m.pages[id] = p
	return m.hydratePage(p), replaced, nil
}

// StatusPageSlugsForMonitor implements Store, mirroring PgStore: published
// pages only, sorted by slug. The page↔monitor link is unique per page, so one
// match per page is all there is to find.
func (m *MemStore) StatusPageSlugsForMonitor(_ context.Context, monitorID int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for id, p := range m.pages {
		if !p.Published {
			continue
		}
		for _, e := range m.pageMonitors[id] {
			if e.MonitorID == monitorID {
				out = append(out, p.Slug)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemStore) DeleteStatusPage(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pages[id]; !ok {
		return ErrNotFound
	}
	delete(m.pages, id)
	delete(m.pageGroups, id)
	delete(m.pageMonitors, id)
	for iid, inc := range m.incidents {
		if inc.StatusPageID == id {
			delete(m.incidents, iid)
			delete(m.incUpdates, iid)
		}
	}
	for sid, sub := range m.subscribers {
		if sub.StatusPageID == id {
			delete(m.subscribers, sid)
		}
	}
	return nil
}

// ReorderStatusPageLayout mirrors PgStore.ReorderStatusPageLayout: it rewrites
// group and monitor positions from a complete ordering, rejecting anything that
// is not exactly the page's current id set. Unlike Postgres there is no ORDER BY
// to re-derive display order on read — hydratePage copies the stored slices
// verbatim — so the slices are sorted here with the same tiebreakers.
func (m *MemStore) ReorderStatusPageLayout(_ context.Context, pageID int64, order StatusPageLayoutOrder) (StatusPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[pageID]
	if !ok {
		return StatusPage{}, ErrNotFound
	}
	groups := append([]StatusPageGroup(nil), m.pageGroups[pageID]...)
	if len(groups) != len(order.Groups) {
		return StatusPage{}, ErrConflict
	}
	idxByGroup := map[int64]int{}
	for i := range groups {
		idxByGroup[groups[i].ID] = i
	}
	seen := map[int64]bool{}
	for i, g := range order.Groups {
		j, ok := idxByGroup[g.GroupID]
		if !ok || seen[g.GroupID] {
			return StatusPage{}, ErrConflict
		}
		seen[g.GroupID] = true
		groups[j].SortOrder = i
	}
	entries := append([]StatusPageMonitorEntry(nil), m.pageMonitors[pageID]...)
	idxByMonitor := map[int64]int{}
	for i := range entries {
		idxByMonitor[entries[i].MonitorID] = i
	}
	pos := 0
	placed := map[int64]bool{}
	place := func(gid *int64, name string, ids []int64) error {
		for _, mid := range ids {
			j, ok := idxByMonitor[mid]
			if !ok || placed[mid] {
				return ErrConflict
			}
			placed[mid] = true
			entries[j].GroupID = gid
			entries[j].GroupName = name
			entries[j].SortOrder = pos
			pos++
		}
		return nil
	}
	if err := place(nil, "", order.Ungrouped); err != nil {
		return StatusPage{}, err
	}
	for _, g := range order.Groups {
		gid := g.GroupID
		if err := place(&gid, groups[idxByGroup[gid]].Name, g.MonitorIDs); err != nil {
			return StatusPage{}, err
		}
	}
	if pos != len(entries) {
		return StatusPage{}, ErrConflict
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].SortOrder != groups[j].SortOrder {
			return groups[i].SortOrder < groups[j].SortOrder
		}
		return groups[i].ID < groups[j].ID // mirrors pgstore's `ORDER BY sort_order, id`
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].SortOrder != entries[j].SortOrder {
			return entries[i].SortOrder < entries[j].SortOrder
		}
		return entries[i].MonitorID < entries[j].MonitorID // mirrors `ORDER BY sort_order, monitor_id`
	})
	m.pageGroups[pageID] = groups
	m.pageMonitors[pageID] = entries
	p.UpdatedAt = m.now()
	m.pages[pageID] = p
	return m.hydratePage(p), nil
}

func (m *MemStore) IncrementStatusPageViews(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[id]
	if !ok {
		return ErrNotFound
	}
	p.ViewCount++
	m.pages[id] = p
	return nil
}

// --- incidents ---

func (m *MemStore) CreateIncident(_ context.Context, in Incident) (Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pages[in.StatusPageID]; !ok {
		return Incident{}, ErrNotFound
	}
	id := m.nextID()
	now := m.now()
	in.ID, in.CreatedAt, in.UpdatedAt = id, now, now
	if in.StartedAt.IsZero() {
		in.StartedAt = now
	}
	in.MonitorIDs = append([]int64(nil), in.MonitorIDs...)
	m.incidents[id] = in
	// Seed the timeline with the opening update.
	m.incUpdates[id] = []IncidentUpdate{{
		ID: m.nextID(), IncidentID: id, Status: in.Status, Body: in.Body, CreatedBy: in.CreatedBy, CreatedAt: now,
	}}
	return m.hydrateIncident(in), nil
}

func (m *MemStore) hydrateIncident(in Incident) Incident {
	// Empty, never nil, matching PgStore: the public payload embeds this and a
	// nil slice renders as null.
	if in.MonitorIDs == nil {
		in.MonitorIDs = []int64{}
	}
	in.Updates = append([]IncidentUpdate{}, m.incUpdates[in.ID]...)
	sort.Slice(in.Updates, func(i, j int) bool { return in.Updates[i].CreatedAt.Before(in.Updates[j].CreatedAt) })
	return in
}

func (m *MemStore) ListIncidents(_ context.Context, pageID int64) ([]Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Incident{}
	for _, in := range m.incidents {
		if in.StatusPageID == pageID {
			out = append(out, m.hydrateIncident(in))
		}
	}
	// pinned first, then newest — the same ORDER BY PgStore uses.
	//
	// The pinned term was missing, and it stopped being cosmetic once the public
	// page began keeping only the first recentIncidentCount resolved incidents
	// from this list: in production ten pinned old incidents pushed every recent
	// one off the page, and in DEV_MODE and every Go test they did not.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

func (m *MemStore) GetIncident(_ context.Context, id int64) (Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.incidents[id]
	if !ok {
		return Incident{}, ErrNotFound
	}
	return m.hydrateIncident(in), nil
}

func (m *MemStore) UpdateIncident(_ context.Context, id int64, in Incident) (Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.incidents[id]
	if !ok {
		return Incident{}, ErrNotFound
	}
	existing.Title = in.Title
	existing.Body = in.Body
	existing.Severity = in.Severity
	existing.Status = in.Status
	existing.Pinned = in.Pinned
	existing.ResolvedAt = in.ResolvedAt
	existing.MonitorIDs = append([]int64(nil), in.MonitorIDs...)
	existing.UpdatedAt = m.now()
	// Postmortem and PostmortemPublishedAt are deliberately NOT copied from in:
	// they survive on `existing`, matching PgStore, whose UPDATE does not name
	// those columns. See the Store interface.
	m.incidents[id] = existing
	return m.hydrateIncident(existing), nil
}

func (m *MemStore) SetIncidentPostmortem(_ context.Context, id int64, body string, publishedAt *time.Time) (Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.incidents[id]
	if !ok {
		return Incident{}, ErrNotFound
	}
	existing.Postmortem = body
	existing.PostmortemPublishedAt = publishedAt
	existing.UpdatedAt = m.now()
	m.incidents[id] = existing
	return m.hydrateIncident(existing), nil
}

func (m *MemStore) DeleteIncident(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.incidents[id]; !ok {
		return ErrNotFound
	}
	delete(m.incidents, id)
	delete(m.incUpdates, id)
	return nil
}

func (m *MemStore) AddIncidentUpdate(_ context.Context, u IncidentUpdate) (IncidentUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.incidents[u.IncidentID]
	if !ok {
		return IncidentUpdate{}, ErrNotFound
	}
	now := m.now()
	u.ID = m.nextID()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	m.incUpdates[u.IncidentID] = append(m.incUpdates[u.IncidentID], u)
	// Advance the incident's own status; stamp the resolution time, and clear it
	// again if the incident leaves the resolved state. See PgStore's identical
	// clause for why clearing matters: without it a reopened incident carries a
	// resolution time into active_incidents, and the post-mortem template prints
	// a Duration for an outage that is still running.
	in.Status = u.Status
	in.UpdatedAt = now
	switch {
	case u.Status == IncidentResolved && in.ResolvedAt == nil:
		t := now
		in.ResolvedAt = &t
	case u.Status != IncidentResolved:
		in.ResolvedAt = nil
	}
	m.incidents[u.IncidentID] = in
	return u, nil
}

// --- subscribers ---

func (m *MemStore) CreateSubscriber(_ context.Context, sub Subscriber) (Subscriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pages[sub.StatusPageID]; !ok {
		return Subscriber{}, ErrNotFound
	}
	for _, existing := range m.subscribers {
		if existing.StatusPageID == sub.StatusPageID && strings.EqualFold(existing.Email, sub.Email) {
			return Subscriber{}, ErrConflict
		}
	}
	id := m.nextID()
	sub.ID = id
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = m.now()
	}
	m.subscribers[id] = sub
	return sub, nil
}

func (m *MemStore) ListSubscribers(_ context.Context, pageID int64, verifiedOnly bool) ([]Subscriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Subscriber{}
	for _, s := range m.subscribers {
		if s.StatusPageID != pageID {
			continue
		}
		if verifiedOnly && !s.Verified {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetSubscriberByVerifyToken(_ context.Context, token string) (Subscriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subscribers {
		if s.VerifyToken != "" && s.VerifyToken == token {
			return s, nil
		}
	}
	return Subscriber{}, ErrNotFound
}

func (m *MemStore) GetSubscriberByUnsubscribeToken(_ context.Context, token string) (Subscriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subscribers {
		if s.UnsubscribeToken == token {
			return s, nil
		}
	}
	return Subscriber{}, ErrNotFound
}

func (m *MemStore) VerifySubscriber(_ context.Context, id int64) (Subscriber, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subscribers[id]
	if !ok {
		return Subscriber{}, ErrNotFound
	}
	now := m.now()
	s.Verified = true
	s.VerifyToken = ""
	s.VerifiedAt = &now
	m.subscribers[id] = s
	return s, nil
}

func (m *MemStore) DeleteSubscriber(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subscribers[id]; !ok {
		return ErrNotFound
	}
	delete(m.subscribers, id)
	return nil
}

// --- settings ---

func (m *MemStore) GetSetting(_ context.Context, key string) (Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.settings[key]
	if !ok {
		return Setting{}, ErrNotFound
	}
	return s, nil
}

func (m *MemStore) ListSettings(context.Context) ([]Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Setting, 0, len(m.settings))
	for _, s := range m.settings {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *MemStore) SetSetting(_ context.Context, key string, value []byte) (Setting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Setting{Key: key, Value: append(json.RawMessage(nil), value...), UpdatedAt: m.now()}
	m.settings[key] = s
	return s, nil
}

// --- audit ---

func (m *MemStore) InsertAudit(_ context.Context, e AuditEntry) (AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = m.nextID()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = m.now()
	}
	m.audit = append(m.audit, e)
	return e, nil
}

func (m *MemStore) ListAudit(_ context.Context, p Page) ([]AuditEntry, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// newest first
	all := make([]AuditEntry, len(m.audit))
	for i, e := range m.audit {
		all[len(m.audit)-1-i] = e
	}
	total := len(all)
	return paginate(all, p), total, nil
}

func (m *MemStore) ExportAudit(_ context.Context, from, to time.Time, limit int) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []AuditEntry{}
	for _, e := range m.audit {
		// Inclusive on both ends, like the SQL. A zero `from` admits everything,
		// which is what time.Time.Before already gives: no entry predates the
		// zero time.
		if e.CreatedAt.Before(from) || e.CreatedAt.After(to) {
			continue
		}
		out = append(out, e)
	}
	// Sorted, not merely reversed.
	//
	// The slice is in INSERTION order, which is not timestamp order: an entry can
	// be written with an explicit CreatedAt (the retention pruner backdates, and
	// tests seed history). Walking it backwards would put such a row at the top
	// of the export while PgStore's ORDER BY created_at DESC, id DESC puts it
	// where its timestamp says. The whole point of this store is to be a faithful
	// double, and the difference here is the ORDER OF A FILE someone files as
	// evidence — so it sorts, with the id as the same tie-break the SQL uses.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	// The cap is applied AFTER sorting, so a truncated export keeps the newest
	// rows — the half an operator asking "what just happened" wants — rather than
	// whichever ones happened to be inserted first.
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- heartbeats ---

// AddHeartbeat records a heartbeat (used by tests and the DEV_MODE engine store
// bridge).
func (m *MemStore) AddHeartbeat(hb HeartbeatView) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beats[hb.MonitorID] = append(m.beats[hb.MonitorID], hb)
}

func (m *MemStore) ListHeartbeats(_ context.Context, monitorID int64, limit int, importantOnly bool) ([]HeartbeatView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	beats := m.beats[monitorID]
	// newest first
	out := make([]HeartbeatView, 0, len(beats))
	for i := len(beats) - 1; i >= 0; i-- {
		if importantOnly && !beats[i].Important {
			continue
		}
		out = append(out, beats[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListImportantHeartbeats implements Store, mirroring PgStore: important beats
// of the monitors in the live map only — a binned monitor has been MOVED out of
// it, so its beats drop out here exactly as deleted_at IS NULL drops them there.
func (m *MemStore) ListImportantHeartbeats(_ context.Context, p Page) ([]HeartbeatView, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := []HeartbeatView{}
	for id, beats := range m.beats {
		if _, live := m.monitors[id]; !live {
			continue
		}
		for _, hb := range beats {
			if hb.Important {
				all = append(all, hb)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CheckedAt.Equal(all[j].CheckedAt) {
			return all[i].CheckedAt.After(all[j].CheckedAt)
		}
		return all[i].MonitorID < all[j].MonitorID
	})
	return paginate(all, p), len(all), nil
}

// ListHeartbeatWindow implements Store, mirroring PgStore: at most limit beats
// from [from, to], newest first, spread evenly across the window rather than
// taken from its newest end.
func (m *MemStore) ListHeartbeatWindow(_ context.Context, monitorID int64, from, to time.Time, limit int, importantOnly bool) ([]HeartbeatView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = defaultHeartbeatLimit
	}
	beats := m.beats[monitorID]
	inWindow := make([]HeartbeatView, 0, len(beats))
	for i := len(beats) - 1; i >= 0; i-- { // newest first
		hb := beats[i]
		if importantOnly && !hb.Important {
			continue
		}
		if hb.CheckedAt.Before(from) || hb.CheckedAt.After(to) {
			continue
		}
		inWindow = append(inWindow, hb)
	}
	if len(inWindow) <= limit {
		return inWindow, nil
	}
	if limit == 1 {
		return inWindow[:1], nil
	}
	// Same stride the SQL computes: ceil((total-1)/(limit-1)), sized over the
	// gaps BETWEEN the endpoints so the oldest row has a slot and the count
	// still never exceeds limit.
	step := (len(inWindow) - 1 + limit - 2) / (limit - 1)
	if step < 1 {
		step = 1
	}
	out := make([]HeartbeatView, 0, limit)
	last := len(inWindow) - 1
	for i := 0; i < len(inWindow); i += step {
		out = append(out, inWindow[i])
	}
	if last%step != 0 {
		out = append(out, inWindow[last])
	}
	return out, nil
}

func (m *MemStore) LatestHeartbeat(_ context.Context, monitorID int64) (HeartbeatView, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	beats := m.beats[monitorID]
	if len(beats) == 0 {
		return HeartbeatView{}, false, nil
	}
	return beats[len(beats)-1], true, nil
}

// --- helpers ---

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func paginate[T any](all []T, p Page) []T {
	if p.Offset >= len(all) {
		return []T{}
	}
	end := len(all)
	if p.Limit > 0 && p.Offset+p.Limit < end {
		end = p.Offset + p.Limit
	}
	return all[p.Offset:end]
}
