package server_test

import (
	"crypto/tls"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// aeadECDHESuites is the set of cipher-suite IDs DefaultTLSConfig is permitted
// to advertise: every entry is an AEAD construction over an ECDHE key
// exchange. The test asserts the returned list is non-empty and a subset of
// this set, so a future edit that introduces a CBC or RSA-key-exchange suite
// (or any non-AEAD/non-ECDHE suite) fails the build.
var aeadECDHESuites = map[uint16]struct{}{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256:       {},
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:         {},
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384:       {},
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:         {},
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256: {},
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256:   {},
}

// TestDefaultTLSConfig_Hardened asserts the baseline config enforces a TLS 1.2
// floor and advertises only modern AEAD/ECDHE cipher suites for the TLS 1.2
// handshake.
func TestDefaultTLSConfig_Hardened(t *testing.T) {
	cfg := server.DefaultTLSConfig()

	if cfg == nil {
		t.Fatal("DefaultTLSConfig returned nil")
	}

	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = 0x%04x, want >= TLS 1.2 (0x%04x)", cfg.MinVersion, tls.VersionTLS12)
	}

	// MaxVersion must be left unset (0) so TLS 1.3 is negotiated automatically.
	if cfg.MaxVersion != 0 {
		t.Errorf("MaxVersion = 0x%04x, want 0 (unset) so TLS 1.3 can be negotiated", cfg.MaxVersion)
	}

	if len(cfg.CipherSuites) == 0 {
		t.Fatal("CipherSuites is empty, want a non-empty modern cipher list for the TLS 1.2 floor")
	}

	for _, id := range cfg.CipherSuites {
		if _, ok := aeadECDHESuites[id]; !ok {
			t.Errorf("CipherSuites contains non-AEAD/non-ECDHE suite 0x%04x (%s)", id, tls.CipherSuiteName(id))
		}
	}
}

// TestDefaultTLSConfig_FreshPerCall asserts each call returns an independent
// config: mutating one must not affect another, so callers can safely add
// their own Certificates/GetCertificate without aliasing.
func TestDefaultTLSConfig_FreshPerCall(t *testing.T) {
	a := server.DefaultTLSConfig()
	b := server.DefaultTLSConfig()

	if a == b {
		t.Fatal("DefaultTLSConfig returned the same pointer twice; want a fresh config per call")
	}

	// Mutate a's scalar field and slice; b must be untouched.
	a.MinVersion = tls.VersionTLS10
	if b.MinVersion < tls.VersionTLS12 {
		t.Errorf("mutating one config's MinVersion affected another: b.MinVersion = 0x%04x", b.MinVersion)
	}

	if len(a.CipherSuites) == 0 || len(b.CipherSuites) == 0 {
		t.Fatal("CipherSuites unexpectedly empty")
	}
	if &a.CipherSuites[0] == &b.CipherSuites[0] {
		t.Fatal("CipherSuites slices share backing array across calls; want independent slices")
	}
	a.CipherSuites[0] = 0xdead
	if b.CipherSuites[0] == 0xdead {
		t.Error("mutating one config's CipherSuites affected another (shared backing array)")
	}
}
