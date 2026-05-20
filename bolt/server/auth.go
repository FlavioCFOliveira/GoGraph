package server

import "errors"

// Common auth errors.
var (
	// ErrAuthFailed is returned when credentials are invalid.
	ErrAuthFailed = errors.New("bolt: authentication failed")

	// ErrSchemeUnknown is returned when the auth scheme is not supported.
	ErrSchemeUnknown = errors.New("bolt: unknown auth scheme")
)

// Identity carries the authenticated principal's metadata after a successful
// authentication exchange.
type Identity struct {
	// Principal is the authenticated username or identifier.
	Principal string
}

// AuthHandler is the pluggable authentication interface. Implementations must
// be safe for concurrent use.
type AuthHandler interface {
	// Authenticate validates the auth scheme, principal, and credentials.
	// On success it returns an Identity; on failure it returns a non-nil error.
	// Returning ErrAuthFailed causes the server to send a Failure with code
	// "Neo.ClientError.Security.Unauthorized". Returning ErrSchemeUnknown
	// causes a Failure with code "Neo.ClientError.Security.AuthProviderFailed".
	Authenticate(scheme, principal, credentials string) (Identity, error)
}

// NoAuthHandler accepts any credentials without validation. Suitable for
// development and testing only.
//
// NoAuthHandler is safe for concurrent use.
type NoAuthHandler struct{}

// Authenticate implements [AuthHandler]. It always returns an Identity with
// the given principal and a nil error.
func (NoAuthHandler) Authenticate(_, principal, _ string) (Identity, error) {
	return Identity{Principal: principal}, nil
}

// BasicAuthHandler validates credentials by delegating to a caller-supplied
// Validate function. The Validate function must return nil on success and a
// non-nil error (typically ErrAuthFailed) on failure.
//
// BasicAuthHandler is safe for concurrent use as long as Validate is.
type BasicAuthHandler struct {
	// Validate is called with the principal and credentials from the client.
	// It must return nil on success and a non-nil error on failure.
	Validate func(principal, credentials string) error
}

// Authenticate implements [AuthHandler]. It accepts only the "basic" scheme;
// any other scheme returns [ErrSchemeUnknown]. It calls h.Validate with the
// principal and credentials; if Validate returns a non-nil error, Authenticate
// returns [ErrAuthFailed].
func (h BasicAuthHandler) Authenticate(scheme, principal, credentials string) (Identity, error) {
	if scheme != "basic" && scheme != "" {
		return Identity{}, ErrSchemeUnknown
	}
	if err := h.Validate(principal, credentials); err != nil {
		return Identity{}, ErrAuthFailed
	}
	return Identity{Principal: principal}, nil
}
