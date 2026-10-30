package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// pubReq issues an unauthenticated request to a public status-page endpoint,
// optionally carrying a page-password header and/or an unlock cookie.
func (h *harness) pubReq(method, target, password string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader *bytes.Reader
	req := httptest.NewRequest(method, target, nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
		req = httptest.NewRequest(method, target, reader)
		req.Header.Set("Content-Type", "application/json")
	}
	if password != "" {
		req.Header.Set("X-Page-Password", password)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// unlockCookie pulls the page-unlock cookie out of a response, failing the test
// if it is absent.
func unlockCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == pageUnlockCookie {
			return c
		}
	}
	t.Fatalf("response set no %s cookie; headers: %v", pageUnlockCookie, rec.Header())
	return nil
}

// TestProtectedPageAllChannelsRequirePassword pins the P0: a password-protected
// page must gate EVERY public output channel, not just the JSON view. Before the
// fix the SVG badge answered without any password, leaking the very incident
// history and existence the password is meant to hide. Each assertion here fails
// (200 instead of 401) if the corresponding checkPagePassword call is removed.
func TestProtectedPageAllChannelsRequirePassword(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "secret", Title: "Secret", Published: &pub, Password: "hunter2",
	})

	base := "/api/v1/public/status/secret"
	channels := []struct {
		name, method, target string
		body                 any
		wantOpen             int // status when access is granted
	}{
		{"json", http.MethodGet, base, nil, http.StatusOK},
		{"badge", http.MethodGet, base + "/badge.svg", nil, http.StatusOK},
	}

	for _, ch := range channels {
		t.Run(ch.name+"/no-password", func(t *testing.T) {
			rec := h.pubReq(ch.method, ch.target, "", nil, ch.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s without password = %d, want 401", ch.name, rec.Code)
			}
			if rec.Header().Get("X-Password-Required") != "true" {
				t.Fatalf("%s 401 missing X-Password-Required marker", ch.name)
			}
		})
		t.Run(ch.name+"/with-password", func(t *testing.T) {
			rec := h.pubReq(ch.method, ch.target, "hunter2", nil, ch.body)
			if rec.Code != ch.wantOpen {
				t.Fatalf("%s with password = %d, want %d, body %s", ch.name, rec.Code, ch.wantOpen, rec.Body.String())
			}
		})
	}
}

// TestPageUnlockCookieUnlocksEveryChannel pins the single-unlock flow: submitting
// the password once (here on the JSON view) mints a scoped cookie that then opens
// the badge endpoint WITHOUT re-sending the password, and a forged or wrong-page
// cookie does not.
func TestPageUnlockCookieUnlocksEveryChannel(t *testing.T) {
	h := newStatusHarness(t)
	admin := h.setupAdmin()
	pub := true
	h.createPage(admin, statusPageRequest{
		Slug: "vault", Title: "Vault", Published: &pub, Password: "hunter2",
	})
	// A second protected page: its cookie must NOT unlock the first.
	h.createPage(admin, statusPageRequest{
		Slug: "other", Title: "Other", Published: &pub, Password: "different",
	})

	base := "/api/v1/public/status/vault"

	// Submit the password on the JSON view; capture the unlock cookie.
	rec := h.pubReq(http.MethodGet, base, "hunter2", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock GET = %d, body %s", rec.Code, rec.Body.String())
	}
	cookie := unlockCookie(t, rec)
	if !cookie.HttpOnly {
		t.Fatalf("unlock cookie is not HttpOnly")
	}

	// The cookie alone (no password) now opens every channel.
	for _, ch := range []struct {
		name, method, target string
		body                 any
		want                 int
	}{
		{"json", http.MethodGet, base, nil, http.StatusOK},
		{"badge", http.MethodGet, base + "/badge.svg", nil, http.StatusOK},
	} {
		if rec := h.pubReq(ch.method, ch.target, "", cookie, ch.body); rec.Code != ch.want {
			t.Fatalf("%s with unlock cookie = %d, want %d, body %s", ch.name, rec.Code, ch.want, rec.Body.String())
		}
	}

	// A cookie minted for "other" must not unlock "vault".
	orec := h.pubReq(http.MethodGet, "/api/v1/public/status/other", "different", nil, nil)
	otherCookie := unlockCookie(t, orec)
	if rec := h.pubReq(http.MethodGet, base, "", otherCookie, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-page cookie GET = %d, want 401", rec.Code)
	}

	// A tampered signature is rejected.
	forged := &http.Cookie{Name: pageUnlockCookie, Value: cookie.Value + "x"}
	if rec := h.pubReq(http.MethodGet, base, "", forged, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie GET = %d, want 401", rec.Code)
	}
}
