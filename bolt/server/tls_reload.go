package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// ErrCertOutsideValidity is the sentinel wrapped by every [CertReloader.Reload]
// refusal caused by the validity window of the leaf on disk: it has expired, or
// it is not valid yet. Callers that distinguish a doomed renewal from unreadable
// or mismatched material — an alerting hook, say — match it with [errors.Is].
//
// It is NOT returned for a pair that fails to read, parse or pair; those keep
// their own wrapped errors.
var ErrCertOutsideValidity = errors.New("bolt/server: certificate outside its validity window")

// CertReloader watches a (certificate, key) PEM file pair on disk
// and serves the most recent successfully loaded pair via the
// [CertReloader.GetCertificate] hook installable on
// [tls.Config.GetCertificate].
//
// The intent is operational: rotate the server's TLS material
// (e.g. cert-manager / Let's Encrypt) without restarting the Bolt
// server. The previous certificate stays in service until the new
// pair is fully validated and only then is the swap performed
// atomically via [sync/atomic.Pointer]. "Fully validated" means the
// pair reads, parses, pairs, AND its leaf is valid at the current
// instant: a reload that fails any of those leaves the live
// certificate untouched and surfaces the error via the provided
// OnError callback (or via stderr when nil). See
// [CertReloader.Reload] for the exact rule and [ErrCertOutsideValidity].
//
// CertReloader is safe for concurrent use; the hot path is a
// single atomic.Pointer.Load.
type CertReloader struct {
	// loadedCertSum / loadedKeySum are SHA-256 digests of the file bytes the
	// certificate currently in service was built from. They are what makes the
	// reload skip SOUND — see [CertReloader.Reload].
	loadedCertSum     [sha256.Size]byte
	loadedKeySum      [sha256.Size]byte
	current           atomic.Pointer[tls.Certificate]
	onError           func(error)
	clk               clock.Clock // validity-window clock; nil means [clock.Real]
	certPath, keyPath string
	mu                sync.Mutex // serialises reload work; does NOT block readers
}

// NewCertReloader loads the certificate + key from disk and returns
// a CertReloader holding the result. The initial load is mandatory:
// if the files cannot be read or parsed, NewCertReloader returns the
// error and the caller MUST fail fast (do not start the server with
// a broken TLS config).
//
// onError is invoked when a later reload (triggered by Reload or by
// the optional Watch goroutine) is REFUSED — whether because the new
// pair cannot be read, parsed or paired, or because its leaf is
// outside its validity window (see [ErrCertOutsideValidity]). A nil
// onError defaults to printing to stderr via fmt.Fprintln.
//
// The initial load performed here is gated on read, parse and pair
// only; see [CertReloader.Reload] for why the validity check applies
// to the SWAP and not to construction.
func NewCertReloader(certPath, keyPath string, onError func(error)) (*CertReloader, error) {
	r := &CertReloader{
		certPath: certPath,
		keyPath:  keyPath,
		onError:  onError,
	}
	if r.onError == nil {
		r.onError = func(err error) { fmt.Fprintln(os.Stderr, "bolt/server: TLS reload:", err) }
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads the certificate + key from disk and atomically
// swaps the live certificate when the new pair is fully validated. A
// failure leaves the live certificate untouched and returns the
// error so the caller (or the OnError callback installed via
// NewCertReloader) can record the incident.
//
// Validation is two-part. The pair must read, parse and pair — that
// is [tls.X509KeyPair] — and, when a certificate is already in
// service, the new leaf must also be valid AT THE CURRENT INSTANT.
// A leaf that has expired, or whose NotBefore is still in the
// future, is refused with an error wrapping [ErrCertOutsideValidity]
// and the live certificate stays in service. Without that second
// part a routine renewal that produced expired material — or a clock
// skew on the renewing host — would replace a working certificate
// with one that fails every handshake, converting a recoverable
// condition into a total outage of the listener (rmp #2557).
//
// # Why the skip is keyed on content, not on mtime
//
// Reload reads both files on every call and compares a SHA-256
// digest of each against the digests the live certificate was built
// from; it re-parses only when they differ.
//
// The skip is kept because it is measurably cheaper, though by less
// than intuition suggests: 20.6 µs and 2.0 kB per call against
// 37.8 µs and 9.4 kB for the full load, best of 5 on darwin/arm64
// (BenchmarkCertReloader_ReloadUnchanged against
// BenchmarkCertReloader_ParseCostAvoidedBySkip). The two file reads
// dominate BOTH paths, so the skip is a 1.8x saving on a call that
// happens once per poll interval — it earns its place by not
// recomputing and re-publishing a certificate that has not changed,
// not by being fast. What matters far more is that it is PROVABLY a
// no-op.
//
// The original skip compared file mtimes, and mtime is not a content
// hash. Any rotation that does not advance it — a rename from another
// directory, cp -p, a restore from an archive, or two rotations
// inside one filesystem timestamp tick — was reported as a SUCCESS
// having loaded nothing, so a rotation performed to REVOKE a
// certificate could be silently ignored while the operator believed
// the material had been replaced, and, because the call succeeded,
// onError never fired to say otherwise (rmp #2558).
//
// The digests are recorded only on a load that succeeded, so a
// refused pair is always re-examined on the next call.
//
// Three properties of that refusal matter operationally:
//
//   - It does NOT record the refused pair's digests, so the very
//     next Reload re-parses the same files. A pair refused for being
//     not-yet-valid is therefore picked up by the Watch poller as
//     soon as its NotBefore passes, with no operator action.
//
//   - It checks the LEAF only, never the rest of the chain. An
//     expired issuer deliberately kept in a bundle is a real,
//     working deployment pattern — the Let's Encrypt DST Root CA X3
//     cross-sign after 2021-09-30 is the canonical case, and it was
//     their DEFAULT recommended chain — and refusing it would break
//     rotations that serve clients fine.
//
//     The residual is accepted deliberately, not overlooked: an
//     expired LOAD-BEARING intermediate still breaks every handshake
//     and this check will not catch it. Envoy Gateway is the one
//     surveyed implementation that does validate the whole serving
//     chain, and it documents two incidents its strictness caused
//     (envoyproxy/gateway#9225 and #9473 — a configuration stall, and
//     one broken Secret silently breaking healthy Gateways), because
//     dropping an expired chain member leaves the key matching a
//     certificate no longer served. Closing this residual by
//     whole-chain validation would trade a narrow gap for a wider
//     blast radius and for false refusals of chains that work.
//
//   - It gates the SWAP, not the initial load. The contract being
//     protected is that a WORKING certificate is never replaced by a
//     broken one; at construction there is nothing to protect, and
//     refusing there would make a host whose clock has not yet
//     synchronised unable to start at all, which is strictly worse
//     than serving material its clients may well accept. That is not
//     a hypothetical: refusing a certificate for its dates AT START
//     is a repeatedly reproduced outage (docker/for-win#2913,
//     kubernetes/minikube#13779, k3s-io/k3s#6152, and the 2012 Azure
//     leap-day disruption, where an agent that failed to create its
//     certificates terminated). RFC 5280 §6.1.1 and §4.1.2.5 put the
//     validity check on the RELYING PARTY and frame the window as a
//     CA warranty, and RFC 8446 §4.4.2.2 imposes no validity rule on
//     a server certificate at all — so a presenter refusing its own
//     leaf is a local availability policy, and it belongs only where
//     it protects something.
//
//     Nor is a clock-skew leeway constant needed here (Kubernetes
//     carries CertificateBackdate = 5m for exactly that). Backdating
//     exists where the refusal is terminal; this refusal costs at
//     most one poll interval, because the digests of a refused pair
//     are not recorded and Watch re-examines it.
func (r *CertReloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	//nolint:gosec // G304: the paths are the operator's own configuration, supplied to NewCertReloader; there is no request-derived input here.
	certPEM, err := os.ReadFile(r.certPath)
	if err != nil {
		return fmt.Errorf("read cert: %w", err)
	}
	//nolint:gosec // G304: as above — an operator-configured path, not attacker-controlled.
	keyPEM, err := os.ReadFile(r.keyPath)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	certSum, keySum := sha256.Sum256(certPEM), sha256.Sum256(keyPEM)
	// Skip the parse only when the bytes on disk are PROVABLY the ones already
	// in service. Reporting success then is honest: there is nothing to load.
	if r.current.Load() != nil &&
		bytes.Equal(certSum[:], r.loadedCertSum[:]) &&
		bytes.Equal(keySum[:], r.loadedKeySum[:]) {
		return nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load X509 key pair: %w", err)
	}
	// A pair that parses is not necessarily a pair that can serve. Only
	// refuse when there is a live certificate to protect: see the
	// "gates the SWAP" note above.
	if r.current.Load() != nil {
		if err := r.checkLeafValidity(&cert); err != nil {
			return err
		}
	}
	r.current.Store(&cert)
	r.loadedCertSum, r.loadedKeySum = certSum, keySum
	return nil
}

// checkLeafValidity reports whether cert's leaf is valid at the instant the
// reloader's clock reads, wrapping [ErrCertOutsideValidity] when it is not.
//
// The leaf is taken from [tls.Certificate.Leaf] when the standard library has
// already populated it and parsed from the first DER block otherwise, so the
// check costs nothing on the common path and stays correct if that changes.
func (r *CertReloader) checkLeafValidity(cert *tls.Certificate) error {
	leaf := cert.Leaf
	if leaf == nil {
		if len(cert.Certificate) == 0 {
			return fmt.Errorf("bolt/server: TLS reload: the pair carries no certificate")
		}
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return fmt.Errorf("parse leaf certificate: %w", err)
		}
		leaf = parsed
	}
	now := r.now()
	switch {
	case now.Before(leaf.NotBefore):
		return fmt.Errorf("%w: leaf %q is not valid until %s (%s from now); keeping the certificate in service",
			ErrCertOutsideValidity, leaf.Subject.CommonName,
			leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotBefore.Sub(now).Round(time.Second))
	case now.After(leaf.NotAfter):
		return fmt.Errorf("%w: leaf %q expired at %s (%s ago); keeping the certificate in service",
			ErrCertOutsideValidity, leaf.Subject.CommonName,
			leaf.NotAfter.UTC().Format(time.RFC3339), now.Sub(leaf.NotAfter).Round(time.Second))
	}
	return nil
}

// now reads the reloader's validity clock, defaulting to real wall time. A
// zero-value CertReloader has no clock, so the nil check is load-bearing rather
// than defensive.
func (r *CertReloader) now() time.Time {
	if r.clk == nil {
		return time.Now()
	}
	return r.clk.Now()
}

// SetClock overrides the clock the VALIDITY WINDOW is evaluated against, so a
// harness can express an expired or not-yet-valid certificate as a fixture with
// fixed dates instead of waiting for real time to pass. A nil clock is ignored.
// Call it before the first [CertReloader.Reload] that must observe it; the
// initial load performed by [NewCertReloader] never consults the clock, so
// setting it immediately after construction is in time for every swap.
//
// It governs the validity comparison and NOTHING else. In particular
// [CertReloader.Watch] keeps its own [time.Ticker] on real time deliberately: a
// poller driven by a frozen fake clock would never tick, and the harness arm
// that proves onError fires over a broken pair depends on real polling.
//
// # Why this is exported, and why it is still not a public API
//
// It is a MODULE-INTERNAL seam, exactly as [Server.SetClock] is. [clock.Clock]
// lives under internal/ and its method set returns internal types
// ([clock.Timer], [clock.Ticker]), so no package outside this module can name
// the parameter type or structurally implement it: the method is unreachable
// from outside GoGraph even though its name is capitalised. An export_test.go
// wrapper would not do — a _test.go file is compiled only into its own
// package's test binary, so internal/sim could never reach it.
func (r *CertReloader) SetClock(clk clock.Clock) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if clk != nil {
		r.clk = clk
	}
}

// GetCertificate is the hook to install on [tls.Config.GetCertificate].
// It returns the most recently loaded certificate. The signature
// matches the standard library's expectation so callers can do:
//
//	cfg := &tls.Config{GetCertificate: reloader.GetCertificate}
//
// The returned *tls.Certificate is shared across all concurrent
// handshakes; callers must NOT mutate the returned value.
func (r *CertReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := r.current.Load()
	if cert == nil {
		return nil, fmt.Errorf("bolt/server: TLS certificate not loaded")
	}
	return cert, nil
}

// Watch starts a background goroutine that calls Reload every
// interval; Reload itself decides whether the bytes on disk differ
// from those in service and skips the parse when they do not. The
// goroutine exits when stop is closed. Watch returns immediately;
// pair it with sync.WaitGroup if the caller wants to block on
// shutdown.
//
// Common usage:
//
//	stop := make(chan struct{})
//	go reloader.Watch(30*time.Second, stop)
//	defer close(stop)
//
// Errors from Reload are surfaced via the onError callback
// installed at construction time; Watch itself never returns an
// error.
func (r *CertReloader) Watch(interval time.Duration, stop <-chan struct{}) {
	pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("component", "tls-cert-reloader")))
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := r.Reload(); err != nil {
				r.onError(err)
			}
		}
	}
}
