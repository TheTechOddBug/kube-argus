package auth

import (
	"context"
	"net/http/httptest"
	"testing"
)

// IsAdmin must respect DefaultRole when auth is disabled, not unconditionally
// return true. This was the bug behind v1.2.7's apiConfigData secret-leak fix.
func TestIsAdmin_NoAuth_DefaultViewer(t *testing.T) {
	savedAuth, savedRole := Enabled, DefaultRole
	defer func() { Enabled, DefaultRole = savedAuth, savedRole }()

	Enabled = false
	DefaultRole = "viewer"

	r := httptest.NewRequest("GET", "/", nil)
	if IsAdmin(r) {
		t.Fatal("IsAdmin should be false when auth disabled and DEFAULT_ROLE=viewer")
	}
}

func TestIsAdmin_NoAuth_DefaultAdmin(t *testing.T) {
	savedAuth, savedRole := Enabled, DefaultRole
	defer func() { Enabled, DefaultRole = savedAuth, savedRole }()

	Enabled = false
	DefaultRole = "admin"

	r := httptest.NewRequest("GET", "/", nil)
	if !IsAdmin(r) {
		t.Fatal("IsAdmin should be true when auth disabled and DEFAULT_ROLE=admin")
	}
}

func TestIsAdmin_AuthEnabled_NoSession(t *testing.T) {
	savedAuth, savedRole := Enabled, DefaultRole
	defer func() { Enabled, DefaultRole = savedAuth, savedRole }()

	Enabled = true
	DefaultRole = "admin" // shouldn't matter when auth is on

	r := httptest.NewRequest("GET", "/", nil)
	if IsAdmin(r) {
		t.Fatal("IsAdmin should be false when auth enabled and no session")
	}
}

func TestIsAdmin_AuthEnabled_AdminSession(t *testing.T) {
	savedAuth := Enabled
	defer func() { Enabled = savedAuth }()

	Enabled = true

	r := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(r.Context(), UserCtxKey, &SessionData{
		Email: "alice@example.com", Role: "admin",
	})
	r = r.WithContext(ctx)

	if !IsAdmin(r) {
		t.Fatal("IsAdmin should be true for admin session")
	}
}

func TestIsAdmin_AuthEnabled_ViewerSession(t *testing.T) {
	savedAuth := Enabled
	defer func() { Enabled = savedAuth }()

	Enabled = true

	r := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(r.Context(), UserCtxKey, &SessionData{
		Email: "alice@example.com", Role: "viewer",
	})
	r = r.WithContext(ctx)

	if IsAdmin(r) {
		t.Fatal("IsAdmin should be false for viewer session")
	}
}
