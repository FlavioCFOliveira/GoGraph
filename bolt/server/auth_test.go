package server

import (
	"errors"
	"testing"
)

func TestNoAuthHandler(t *testing.T) {
	t.Parallel()

	h := NoAuthHandler{}

	t.Run("accepts empty credentials", func(t *testing.T) {
		t.Parallel()
		id, err := h.Authenticate("none", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Principal != "" {
			t.Fatalf("principal: got %q, want %q", id.Principal, "")
		}
	})

	t.Run("accepts any credentials", func(t *testing.T) {
		t.Parallel()
		id, err := h.Authenticate("basic", "alice", "secret123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Principal != "alice" {
			t.Fatalf("principal: got %q, want %q", id.Principal, "alice")
		}
	})

	t.Run("preserves principal", func(t *testing.T) {
		t.Parallel()
		id, err := h.Authenticate("token", "bob", "tok_xyz")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Principal != "bob" {
			t.Fatalf("principal: got %q, want %q", id.Principal, "bob")
		}
	})
}

func TestBasicAuthHandler(t *testing.T) {
	t.Parallel()

	t.Run("valid credentials", func(t *testing.T) {
		t.Parallel()
		h := BasicAuthHandler{
			Validate: func(principal, credentials string) error {
				if principal == "admin" && credentials == "password" {
					return nil
				}
				return ErrAuthFailed
			},
		}
		id, err := h.Authenticate("basic", "admin", "password")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Principal != "admin" {
			t.Fatalf("principal: got %q, want %q", id.Principal, "admin")
		}
	})

	t.Run("wrong credentials return ErrAuthFailed", func(t *testing.T) {
		t.Parallel()
		h := BasicAuthHandler{
			Validate: func(_, _ string) error { return errors.New("bad") },
		}
		_, err := h.Authenticate("basic", "user", "wrong")
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("got %v, want ErrAuthFailed", err)
		}
	})

	t.Run("unknown scheme returns ErrSchemeUnknown", func(t *testing.T) {
		t.Parallel()
		h := BasicAuthHandler{
			Validate: func(_, _ string) error { return nil },
		}
		_, err := h.Authenticate("token", "user", "tok")
		if !errors.Is(err, ErrSchemeUnknown) {
			t.Fatalf("got %v, want ErrSchemeUnknown", err)
		}
	})

	t.Run("empty scheme accepted as basic", func(t *testing.T) {
		t.Parallel()
		called := false
		h := BasicAuthHandler{
			Validate: func(p, c string) error {
				called = true
				if p == "u" && c == "p" {
					return nil
				}
				return ErrAuthFailed
			},
		}
		id, err := h.Authenticate("", "u", "p")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Fatal("Validate was not called")
		}
		if id.Principal != "u" {
			t.Fatalf("principal: got %q, want %q", id.Principal, "u")
		}
	})

	t.Run("validator error wraps to ErrAuthFailed", func(t *testing.T) {
		t.Parallel()
		customErr := errors.New("custom internal error")
		h := BasicAuthHandler{
			Validate: func(_, _ string) error { return customErr },
		}
		_, err := h.Authenticate("basic", "x", "y")
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("got %v, want ErrAuthFailed", err)
		}
	})
}
