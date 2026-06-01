package server

import "crypto/tls"

// DefaultTLSConfig returns a hardened baseline [tls.Config] that operators
// should use as the STARTING POINT for the server's transport security.
//
// The configuration sets a TLS 1.2 floor and a modern, AEAD-only cipher
// list for the TLS 1.2 handshake; TLS 1.3 is negotiated automatically when
// both peers support it (TLS 1.3 cipher suites are fixed by the Go runtime
// and are always safe, so they are not — and cannot be — listed here). No
// MaxVersion is set, so a 1.3-capable client always upgrades to 1.3.
//
// The returned config is INCOMPLETE on its own: it carries no certificate.
// Callers MUST populate it with their own server identity before use, by
// setting one of:
//
//   - [tls.Config.Certificates], or
//   - [tls.Config.GetCertificate] (for example by wiring the on-disk hot
//     reloader: cfg.GetCertificate = reloader.GetCertificate; see
//     [CertReloader]).
//
// Then pass the result in [Options.TLSConfig].
//
// The server does NOT impose this baseline automatically. It wraps whatever
// [Options.TLSConfig] the operator supplies verbatim: passing a nil
// TLSConfig keeps the existing behaviour of running PLAINTEXT TCP (no TLS at
// all). DefaultTLSConfig only provides and documents a safe default; it never
// overrides an operator-supplied config, so embedders are never surprised.
//
// A fresh, independent config is returned on every call (no shared mutable
// global), so callers may freely mutate the result — adding Certificates,
// GetCertificate, client-auth policy, etc. — without aliasing another
// caller's configuration.
func DefaultTLSConfig() *tls.Config {
	return &tls.Config{
		// TLS 1.2 floor. TLS 1.3 is negotiated automatically when the peer
		// supports it; MaxVersion is intentionally left unset so 1.3 is used.
		MinVersion: tls.VersionTLS12,
		// Modern AEAD + ECDHE suites for the TLS 1.2 handshake only. The TLS
		// 1.3 suites are not configurable in Go and are always safe, so they
		// are deliberately absent. Server-side suite ordering is no longer
		// honoured by the Go runtime (PreferServerCipherSuites is deprecated
		// and ignored), so this is a permitted-set, not a preference order;
		// every entry below is an AEAD construction over an ECDHE key
		// exchange.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}
}
