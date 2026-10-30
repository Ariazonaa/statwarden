package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// adminGuardStore adds the last-active-admin invariant to user mutations: the
// check and the write happen atomically in the store, so concurrent demotions,
// deactivations or deletes cannot each observe the other's admin and both
// succeed. Both MemStore and PgStore implement it.
type adminGuardStore interface {
	UpdateUserGuarded(ctx context.Context, id int64, email, username, displayName, role string, isActive bool) (User, error)
	DeleteUserGuarded(ctx context.Context, id int64) error
}

type createUserRequest struct {
	Email       string `json:"email"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type updateUserRequest struct {
	Email       string `json:"email"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	IsActive    *bool  `json:"is_active"`
}

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if storeError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateCredentials(req.Email, req.Password); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !auth.Role(req.Role).Valid() {
		writeProblemDetail(w, http.StatusBadRequest, "role must be admin, user or viewer")
		return
	}
	// An explicit username is format-checked (400 on a bad format, 409 later on a
	// duplicate); an omitted one is derived from the e-mail local part.
	username := strings.TrimSpace(req.Username)
	if username == "" {
		username = deriveUsername(strings.TrimSpace(req.Email))
	} else if err := validateUsername(username); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "hash password")
		return
	}
	u, err := s.store.CreateUser(r.Context(), strings.TrimSpace(req.Email), username, hash, req.DisplayName, req.Role)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "user.create", "user", &u.ID, clientIP(r), map[string]any{"email": u.Email, "role": u.Role})
	writeJSON(w, http.StatusCreated, u)
}

func (s *server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateEmail(req.Email); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	if !auth.Role(req.Role).Valid() {
		writeProblemDetail(w, http.StatusBadRequest, "role must be admin, user or viewer")
		return
	}
	// A provided username is format-checked; an omitted one leaves the existing
	// login name untouched (this is a full-replace of the other editable fields).
	username := strings.TrimSpace(req.Username)
	if username != "" {
		if err := validateUsername(username); err != nil {
			writeProblemDetail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	existing, err := s.store.GetUserByID(r.Context(), id)
	if storeError(w, err) {
		return
	}
	if username == "" {
		username = existing.Username
	}
	isActive := existing.IsActive
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	guard, ok := s.store.(adminGuardStore)
	if !ok {
		writeProblemDetail(w, http.StatusInternalServerError, "store does not support admin guards")
		return
	}
	u, err := guard.UpdateUserGuarded(r.Context(), id, strings.TrimSpace(req.Email), username, req.DisplayName, req.Role, isActive)
	if errors.Is(err, ErrLastAdmin) {
		writeProblemDetail(w, http.StatusConflict, "at least one active administrator must remain")
		return
	}
	if storeError(w, err) {
		return
	}
	// A deactivated user's sessions are revoked immediately.
	if !isActive {
		_ = s.store.DeleteSessionsByUser(r.Context(), id, "")
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "user.update", "user", &id, clientIP(r), nil)
	writeJSON(w, http.StatusOK, u)
}

func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	p := principalFrom(r)
	if id == p.userID {
		writeProblemDetail(w, http.StatusConflict, "you cannot delete your own account")
		return
	}
	guard, ok := s.store.(adminGuardStore)
	if !ok {
		writeProblemDetail(w, http.StatusInternalServerError, "store does not support admin guards")
		return
	}
	if err := guard.DeleteUserGuarded(r.Context(), id); errors.Is(err, ErrLastAdmin) {
		writeProblemDetail(w, http.StatusConflict, "at least one active administrator must remain")
		return
	} else if storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "user.delete", "user", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	p := parsePage(r)
	entries, total, err := s.store.ListAudit(r.Context(), p)
	if storeError(w, err) {
		return
	}
	writeList(w, entries, total, p)
}
