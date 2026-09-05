package server

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"
	"time"
)

// BenchmarkCertReloader_ReloadUnchanged measures the path [CertReloader.Watch]
// takes on every tick when nothing on disk has changed: read both files, digest
// them, compare, return. It is the cost of KEEPING a skip.
//
// Its counterpart is BenchmarkCertReloader_ParseCostAvoidedBySkip, which measures
// what the skip buys. rmp #2558 replaced an unsound mtime heuristic with a
// content digest, and the mandate is to measure rather than assume the cheap path
// is worth keeping.
func BenchmarkCertReloader_ReloadUnchanged(b *testing.B) {
	dir := b.TempDir()
	certPath, keyPath := writeBenchPair(b, dir)
	r, err := NewCertReloader(certPath, keyPath, func(error) {})
	if err != nil {
		b.Fatalf("NewCertReloader: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := r.Reload(); err != nil {
			b.Fatalf("Reload: %v", err)
		}
	}
}

// BenchmarkCertReloader_ParseCostAvoidedBySkip measures the work a skip avoids:
// the PEM/DER decode, the key-pair agreement check, and the leaf parse the
// validity check needs. It reads the same two files first, so the two benchmarks
// differ by exactly the parse.
func BenchmarkCertReloader_ParseCostAvoidedBySkip(b *testing.B) {
	dir := b.TempDir()
	certPath, keyPath := writeBenchPair(b, dir)
	b.ReportAllocs()
	for b.Loop() {
		certPEM, err := os.ReadFile(certPath) //nolint:gosec // G304: certPath was written by this benchmark under its own temp dir.
		if err != nil {
			b.Fatalf("read cert: %v", err)
		}
		keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // G304: keyPath was written by this benchmark under its own temp dir.
		if err != nil {
			b.Fatalf("read key: %v", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			b.Fatalf("X509KeyPair: %v", err)
		}
		if cert.Leaf == nil {
			if _, err := x509.ParseCertificate(cert.Certificate[0]); err != nil {
				b.Fatalf("ParseCertificate: %v", err)
			}
		}
	}
}

// writeBenchPair writes one valid (cert, key) pair for the benchmarks above.
func writeBenchPair(b *testing.B, dir string) (string, string) {
	b.Helper()
	now := time.Now()
	return writeValidityPair(b, dir, "bench", now.Add(-time.Hour), now.Add(time.Hour))
}
