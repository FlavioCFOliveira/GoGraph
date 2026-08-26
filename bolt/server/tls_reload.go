package server

import (
	"context"
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
	lastCertModTime   time.Time
	lastKeyModTime    time.Time
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
// is [tls.LoadX509KeyPair] — and, when a certificate is already in
// service, the new leaf must also be valid AT THE CURRENT INSTANT.
// A leaf that has expired, or whose NotBefore is still in the
// future, is refused with an error wrapping [ErrCertOutsideValidity]
// and the live certificate stays in service. Without that second
// part a routine renewal that produced expired material — or a clock
// skew on the renewing host — would replace a working certificate
// with one that fails every handshake, converting a recoverable
// condition into a total outage of the listener (rmp #2557).
//
// Three properties of that refusal matter operationally:
//
//   - It does NOT stamp the mtime bookkeeping, so the very next
//     Reload re-examines the same files. A pair refused for being
//     not-yet-valid is therefore picked up by the Watch poller as
//     soon as its NotBefore passes, with no operator action.
//   - It checks the LEAF only, never the rest of the chain. An
//     expired issuer deliberately kept in a bundle is a real,
//     working deployment pattern — the Let's Encrypt DST Root CA X3
//     cross-sign after 2021-09-30 is the canonical case — and
//     refusing it would break rotations that serve clients fine.
//   - It gates the SWAP, not the initial load. The contract being
//     protected is that a WORKING certificate is never replaced by a
//     broken one; at construction there is nothing to protect, and
//     refusing there would make a host whose clock has not yet
//     synchronised unable to start at all, which is strictly worse
//     than serving material its clients may well accept.
func (r *CertReloader) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	certInfo, err := os.Stat(r.certPath)
	if err != nil {
		return fmt.Errorf("stat cert: %w", err)
	}
	keyInfo, err := os.Stat(r.keyPath)
	if err != nil {
		return fmt.Errorf("stat key: %w", err)
	}
	// Skip the parse when nothing has changed since the last
	// successful load — the cheap mtime check avoids re-parsing the
	// PEM payload on every Watch tick.
	if !certInfo.ModTime().After(r.lastCertModTime) &&
		!keyInfo.ModTime().After(r.lastKeyModTime) &&
		r.current.Load() != nil {
		return nil
	}

	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
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
	r.lastCertModTime = certInfo.ModTime()
	r.lastKeyModTime = keyInfo.ModTime()
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

// Watch starts a background goroutine that polls the certificate
// and key files every interval and calls Reload when either has a
// fresh mtime. The goroutine exits when stop is closed. Watch
// returns immediately; pair it with sync.WaitGroup if the caller
// wants to block on shutdown.
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
