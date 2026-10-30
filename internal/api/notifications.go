package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Ariazonaa/statwarden/internal/notify"
)

type notificationRequest struct {
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Config    json.RawMessage `json:"config"`
	IsDefault bool            `json:"is_default"`
	Active    *bool           `json:"active"`
}

func (s *server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	notifs, err := s.store.ListNotifications(r.Context())
	if storeError(w, err) {
		return
	}
	for i := range notifs {
		notifs[i].Config = redactConfig(notifs[i].Config)
	}
	writeJSON(w, http.StatusOK, notifs)
}

func (s *server) handleGetNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	n, err := s.store.GetNotification(r.Context(), id)
	if storeError(w, err) {
		return
	}
	n.Config = redactConfig(n.Config)
	writeJSON(w, http.StatusOK, n)
}

func (s *server) handleCreateNotification(w http.ResponseWriter, r *http.Request) {
	var req notificationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.validateNotification(req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.notificationFromRequest(req)
	if err != nil {
		s.logger.Error("could not seal a notification config", "err", err)
		writeProblemDetail(w, http.StatusInternalServerError,
			"the channel configuration could not be encrypted and was not saved")
		return
	}
	created, err := s.store.CreateNotification(r.Context(), n)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "notification.create", "notification", &created.ID, clientIP(r), map[string]any{"name": created.Name, "type": created.Type})
	created.Config = redactConfig(created.Config)
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) handleUpdateNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req notificationRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// An ABSENT config means "leave the configuration alone".
	//
	// Read responses hide a channel's config wholesale — unlike a monitor's,
	// which is masked field by field and can be handed back — so an editor
	// opened on an existing channel starts with every field blank. Saving after
	// changing only the name then submitted that blank form as the new
	// configuration: for most providers a required field is missing and the
	// write is rejected, so a rename is impossible without re-typing the SMTP
	// password; for a provider whose fields are all optional it went through
	// and the credentials were gone, and the channel stopped delivering.
	//
	// Absent is distinguishable from empty here: decodeJSON leaves the field nil
	// when the key is missing, and `{}` is a real, empty object.
	if req.Config == nil {
		existing, gerr := s.store.GetNotification(r.Context(), id)
		if storeError(w, gerr) {
			return
		}
		// Opened before it is reused: the stored value is an encryption
		// envelope, and both the type's validator and the re-seal below expect
		// the real configuration. Handing the envelope straight back would fail
		// validation on a config that was valid when it was written.
		req.Config = s.openNotificationConfig(existing.Config)
	}

	if err := s.validateNotification(req); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := s.notificationFromRequest(req)
	if err != nil {
		s.logger.Error("could not seal a notification config", "err", err)
		writeProblemDetail(w, http.StatusInternalServerError,
			"the channel configuration could not be encrypted and was not saved")
		return
	}
	updated, err := s.store.UpdateNotification(r.Context(), id, n)
	if storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "notification.update", "notification", &id, clientIP(r), nil)
	updated.Config = redactConfig(updated.Config)
	writeJSON(w, http.StatusOK, updated)
}

func (s *server) handleDeleteNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.store.DeleteNotification(r.Context(), id); storeError(w, err) {
		return
	}
	p := principalFrom(r)
	s.audit(r.Context(), &p.userID, "notification.delete", "notification", &id, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestNotification sends a test message through the dispatcher using the
// stored (decrypted) channel config.
func (s *server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	n, err := s.store.GetNotification(r.Context(), id)
	if storeError(w, err) {
		return
	}
	if s.dispatcher == nil {
		writeProblemDetail(w, http.StatusServiceUnavailable, "notification dispatcher not available")
		return
	}
	cfg := n.Config
	if s.box != nil {
		if plain, derr := s.box.DecryptConfig(cfg); derr == nil {
			cfg = plain
		}
	}
	err = s.dispatcher.TestSend(r.Context(), notify.Notification{
		ID:     n.ID,
		Name:   n.Name,
		Type:   n.Type,
		Config: cfg,
		Active: n.Active,
	})
	if err != nil {
		writeProblemDetail(w, http.StatusBadGateway, "test send failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// validateNotification checks the provider type is registered and its config
// validates.
func (s *server) validateNotification(req notificationRequest) error {
	if req.Name == "" {
		return validationError("name is required")
	}
	notifier, ok := s.notifyReg.Get(req.Type)
	if !ok {
		return validationError("unknown notification type " + req.Type)
	}
	cfg := req.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	// The read marker is not a configuration.
	//
	// Reads answer {"$encrypted":true} for a sealed channel. Submitting that
	// back stores it AS the configuration, and the next read masks it to the
	// same marker — so a channel with no usable settings is indistinguishable
	// from a working one, and it simply never delivers. Refused with the way
	// out, which is to leave the field off entirely.
	if isRedactionMarker(cfg) {
		return validationError(
			"that is the placeholder shown in place of a hidden configuration, not a " +
				"configuration; omit the field to keep the stored one, or send the real values")
	}
	if err := notifier.Validate(cfg); err != nil {
		return validationError("invalid config: " + err.Error())
	}
	return nil
}

func (s *server) notificationFromRequest(req notificationRequest) (Notification, error) {
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	cfg := req.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	// Seal secrets at rest when a box is configured; the pgstore expects
	// already-encrypted configs mirroring the notify package's boundary.
	//
	// A failure here used to fall through and store the PLAINTEXT config —
	// SMTP passwords, webhook tokens — while the API answered 201 as if
	// everything had worked. EncryptConfig fails when the system random source
	// does, which is rare and exactly the moment silence is worst: the
	// at-rest guarantee is quietly gone and nothing says so. Refusing the write
	// is the only honest answer.
	if s.box != nil {
		sealed, err := s.box.EncryptConfig(cfg)
		if err != nil {
			return Notification{}, fmt.Errorf("seal notification config: %w", err)
		}
		cfg = sealed
	}
	return Notification{
		Name:      req.Name,
		Type:      req.Type,
		Config:    cfg,
		IsDefault: req.IsDefault,
		Active:    active,
	}, nil
}

// redactConfig hides an encrypted config envelope from API responses; a
// plaintext (DEV_MODE) config is returned as-is.
func redactConfig(cfg json.RawMessage) json.RawMessage {
	var env struct {
		Encrypted string `json:"$encrypted"`
	}
	if err := json.Unmarshal(cfg, &env); err == nil && env.Encrypted != "" {
		return json.RawMessage(`{"$encrypted":true}`)
	}
	return cfg
}

// openNotificationConfig unseals a stored channel config. With no box
// (DEV_MODE) or a plaintext value it returns the input unchanged, mirroring the
// monitor read path.
//
// A config that cannot be opened is returned as-is rather than replaced: the
// caller then fails validation and reports it, which is the honest outcome for
// a value this process has no key for.
func (s *server) openNotificationConfig(cfg json.RawMessage) json.RawMessage {
	if s.box == nil || len(cfg) == 0 {
		return cfg
	}
	plain, err := s.box.DecryptConfig(cfg)
	if err != nil {
		s.logger.Error("could not open a stored notification config", "err", err)
		return cfg
	}
	return plain
}

// isRedactionMarker reports whether cfg is the {"$encrypted":true} stand-in
// that read responses show in place of a sealed channel config. A real
// envelope carries a base64 STRING there, which is why the type is what
// separates them.
func isRedactionMarker(cfg json.RawMessage) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &probe); err != nil {
		return false
	}
	raw, ok := probe["$encrypted"]
	if !ok {
		return false
	}
	var asBool bool
	return json.Unmarshal(raw, &asBool) == nil
}
