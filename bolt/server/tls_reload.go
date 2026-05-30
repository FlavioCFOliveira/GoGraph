package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"time"
)

// CertReloader watches a (certificate, key) PEM file pair on disk
// and serves the most recent successfully loaded pair via the
// [CertReloader.GetCertificate] hook installable on
// [tls.Config.GetCertificate].
//
// The intent is operational: rotate the server's TLS material
// (e.g. cert-manager / Let's Encrypt) without restarting the Bolt
// server. The previous certificate stays in service until the new
// pair is fully validated and only then is the swap performed
// atomically via [sync/atomic.Pointer]. A reload that fails to
// parse leaves the live certificate untouched and surfaces the
// error via the provided OnError callback (or via stderr when nil).
//
// CertReloader is safe for concurrent use; the hot path is a
// single atomic.Pointer.Load.
type CertReloader struct {
	certPath, keyPath string
	current           atomic.Pointer[tls.Certificate]
	mu                sync.Mutex // serialises reload work; does NOT block readers
	lastCertModTime   time.Time
	lastKeyModTime    time.Time
	onError           func(error)
}

// NewCertReloader loads the certificate + key from disk and returns
// a CertReloader holding the result. The initial load is mandatory:
// if the files cannot be read or parsed, NewCertReloader returns the
// error and the caller MUST fail fast (do not start the server with
// a broken TLS config).
//
// onError is invoked when a later reload (triggered by Reload or by
// the optional Watch goroutine) fails to parse the new pair. A nil
// onError defaults to printing to stderr via fmt.Fprintln.
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
// swaps the live certificate when the parse succeeds. A parse
// failure leaves the live certificate untouched and returns the
// error so the caller (or the OnError callback installed via
// NewCertReloader) can record the incident.
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
	r.current.Store(&cert)
	r.lastCertModTime = certInfo.ModTime()
	r.lastKeyModTime = keyInfo.ModTime()
	return nil
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
