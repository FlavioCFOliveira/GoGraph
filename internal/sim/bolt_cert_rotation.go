package sim

// bolt_cert_rotation.go — TLS certificate rotation under fault (rmp #2481).
//
// # What this covers, and why it is here
//
// [github.com/FlavioCFOliveira/GoGraph/bolt/server.CertReloader] is the operator
// seam for rotating the Bolt server's TLS material without a restart. Its whole
// promise is a negative one: a rotation that goes wrong must leave the LIVE
// certificate untouched, so the server keeps serving handshakes on the last pair
// that fully validated. A promise of that shape is only tested by breaking the
// rotation, and until now no DST scenario drove it at all — the only SimServer in
// the harness runs plaintext.
//
// The failure modes here are the ones a real rotator produces, not invented ones:
//
//   - a TORN key: the writer was interrupted, so only a PREFIX of the PEM landed;
//   - a GARBLED key: the bytes landed but a sector under them went bad;
//   - an ABSENT key: the rotator unlinked the old file and died before writing;
//   - a MISMATCHED pair: the cert landed and the key did not, so the two no
//     longer belong together — the most dangerous case, because both files parse.
//
// # The oracle is a real TLS handshake, not our own accessor
//
// Every step's verdict has two independent halves, and it matters which one
// answers which question.
//
// WHICH pair is in service is settled by the Common Name of the served leaf,
// compared against the pair the step was expected to leave live. THAT the served
// pair is usable at all is settled by completing a genuine TLS 1.3 handshake
// through crypto/tls over a net.Pipe against whatever GetCertificate currently
// returns, with the client trusting exactly that certificate and verifying the
// name. crypto/tls is the independent reference for the second half: it proves the
// certificate parses, the name matches, and — decisively — that the private key
// really corresponds to the certificate's public key, which is the property a
// mismatched rotation destroys and a byte comparison cannot see.
//
// The handshake deliberately does NOT trust a pre-agreed root, so it cannot by
// itself detect "the wrong pair went live"; that is the CN half's job. Stating it
// the other way round would credit the handshake with a check it does not make.
//
// # Why the certificate files are REAL files
//
// CertReloader reads through os.Stat and tls.LoadX509KeyPair; it exposes no
// filesystem seam, and growing one purely for this scenario would change a
// production API rather than test it. [SimDisk] is therefore the IMAGE AUTHORITY —
// every version of every file, and every fault applied to it, is produced through
// SimDisk and its injectors, so the whole sequence is seeded and reproducible —
// and each image is then projected onto a real file in a temporary directory for
// CertReloader to read. The projection is the only step outside the simulated
// disk, and it copies bytes without deciding any of them.
//
// What justifies that split is that SimDisk cannot be substituted for os here at
// all: the reader's dependency is on the standard library, not on an interface,
// and growing CertReloader a filesystem seam purely for this scenario would
// change a production API rather than test it. The precedent for leaving the
// simulated disk where the seam does not exist is established in
// wal_writer_surface.go.
//
// The torn key is produced by an ACTUAL SimDisk host crash, not by writing a
// short file: the prefix is written and Sync'd (so it enters the durable image),
// the remainder is written and left un-synced, and [SimDisk.CrashHost] then
// reverts the file to its durable image. Since rmp #2535 that image advances only
// on a Sync that returned nil, so the crash discards exactly the un-synced
// remainder — which is what a power failure mid-rotation leaves on a real
// filesystem. (Before #2535 un-synced bytes survived a crash intact, so this arm
// would have had to fake the truncation; the fix is what makes the crash
// load-bearing here.)

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// certRotationHost is the SAN every generated certificate carries, and the name
// the verifying client asks for. A modern TLS client ignores the Common Name for
// name verification, so the SAN is what makes the handshake oracle meaningful.
const certRotationHost = "sim.local"

// certRotationNotBefore / certRotationNotAfter are FIXED validity bounds. Using
// time.Now() would make the DER bytes differ run to run, and a scenario that
// claims bit-reproducibility cannot afford a timestamp in its fixtures.
//
// certRotationVerifyAt is the instant the verifying handshake evaluates validity
// at, injected into tls.Config.Time on BOTH sides. Without it the fixed bounds
// above would be a TIME BOMB: crypto/tls would compare them against the real
// clock, so the whole scenario would begin failing on 2036-01-01 for a reason
// having nothing to do with the code under test. Pinning the evaluation instant
// keeps the verdict a function of the seed alone — and it is what will later allow
// an EXPIRED-certificate arm to be expressed as a fixture rather than as a wait.
var (
	certRotationNotBefore = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	certRotationNotAfter  = time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	certRotationVerifyAt  = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

// certRotationWatchInterval / certRotationWatchBudget bound the Watch arm: a
// short REAL poll period (Watch owns its own time.Ticker and exposes no clock
// seam) and a generous ceiling, so the arm fails loudly rather than hanging if
// onError never fires.
const (
	certRotationWatchInterval = 5 * time.Millisecond
	certRotationWatchBudget   = 5 * time.Second
)

// certRotationSeedMix and certRotationDiskSeedMix decorrelate this scenario's two
// seeded streams — the key material and the simulated disk — from the scenario
// seed AND from each other. Two mixes rather than one because a single derived
// value would make the two streams identical, which is not what "decorrelated"
// means even where it happens to be harmless.
const (
	certRotationSeedMix     = 0xC0F1_9E5A
	certRotationDiskSeedMix = 0x5E4C_70B3
)

// simCertPair is one generated (certificate, key) pair plus the identity the
// handshake oracle recognises it by.
type simCertPair struct {
	// CN is the Common Name, used purely as a label in evidence: it says WHICH
	// pair is in service without needing to compare bytes.
	CN string
	// CertPEM / KeyPEM are the encoded files.
	CertPEM, KeyPEM []byte
}

// deterministicReader is an io.Reader over a seeded stream. It exists so
// [ed25519.GenerateKey] produces the SAME key for the same seed: a fixture that
// is regenerated on every run cannot be replayed, and a replayable rotation
// scenario is the point.
type deterministicReader struct{ seed *Seed }

// Read fills p from the seeded stream. It never fails and never returns short.
func (r deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r.seed.IntN(256))
	}
	return len(p), nil
}

// newSimCertPair generates a self-signed Ed25519 leaf for [certRotationHost],
// deterministically from seed.
func newSimCertPair(seed *Seed, cn string) (simCertPair, error) {
	return newSimCertPairForHost(seed, cn, certRotationHost)
}

// newSimCertPairForHost generates a self-signed Ed25519 leaf for an arbitrary SAN,
// deterministically from seed. Ed25519 is chosen because both key generation and
// signing are deterministic given their inputs, so the PEM bytes are a pure
// function of the seed — which an ECDSA pair, whose signature draws randomness,
// would not be.
//
// The host is a parameter so a NEGATIVE control can be issued: a certificate for
// the wrong name is what proves the handshake oracle in [certRotationVerify] is
// able to fail at all. The scenario itself always passes [certRotationHost].
func newSimCertPairForHost(seed *Seed, cn, host string) (simCertPair, error) {
	rd := deterministicReader{seed: seed}
	pub, priv, err := ed25519.GenerateKey(rd)
	if err != nil {
		return simCertPair{}, fmt.Errorf("sim: cert rotation generate key: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(int64(seed.Uint64N(1 << 40))),
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              []string{host},
		NotBefore:             certRotationNotBefore,
		NotAfter:              certRotationNotAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rd, &tmpl, &tmpl, pub, priv)
	if err != nil {
		return simCertPair{}, fmt.Errorf("sim: cert rotation create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return simCertPair{}, fmt.Errorf("sim: cert rotation marshal key: %w", err)
	}
	return simCertPair{
		CN:      cn,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// CertRotationStep records one rotation attempt and what the reloader served
// afterwards.
type CertRotationStep struct {
	// Name identifies the step in a violation message.
	Name string
	// WantCN is the Common Name that MUST be in service after this step: the newly
	// installed pair for a step expected to succeed, the previous one for a step
	// expected to fail.
	WantCN string
	// ServedCN is the Common Name actually served by GetCertificate afterwards.
	ServedCN string
	// ReloadErr is the error Reload returned, rendered ("" when it succeeded).
	ReloadErr string
	// HandshakeErr is the error the verifying TLS handshake returned, rendered
	// ("" when the handshake completed).
	HandshakeErr string
	// KeyBytes is the size of the key file this step left on disk, and KeyWantBytes
	// the size of the key the step was WRITING. They exist so the fault a step
	// claims to inject is proven distinct from its neighbour's: a torn key is
	// SHORTER than the key it truncates, a garbled one is exactly as long. Without
	// them two steps could quietly inject the same fault — they already produce the
	// same parse error — and the roster would overstate what was covered.
	KeyBytes, KeyWantBytes int
	// WantReloadOK records the step's INTENT: true when the pair on disk is
	// complete and valid, false when the step deliberately broke it.
	WantReloadOK bool
	// ReloadOK records what actually happened.
	ReloadOK bool
	// Handshook records whether a real TLS handshake completed against the served
	// certificate. It must be true after EVERY step, successful or not: a broken
	// rotation may not change the certificate, and may not break the one in
	// service either.
	Handshook bool
}

// BoltCertRotationEvidence is everything one [RunBoltCertRotation] observed.
type BoltCertRotationEvidence struct {
	// Steps are the rotation attempts in the order they ran.
	Steps []CertRotationStep
	// InitialLoadTornErr is the error [server.NewCertReloader] returned when
	// constructed over a TORN key, rendered. The initial load is documented as
	// mandatory, so this must be non-empty: a reloader that starts on unparseable
	// material would put a server into service with no certificate at all.
	InitialLoadTornErr string
	// WatchErrors are the errors the reloader's onError callback delivered while a
	// [server.Watch] goroutine polled a BROKEN pair.
	//
	// onError is the only operator-visible signal that a BACKGROUND rotation
	// failed, and before this scenario nothing in the module asserted it: deleting
	// the callback from Watch would have broken no test. It is the same defect
	// class as the Options.Logger bypass this sprint fixed — a security-relevant
	// event with no reachable destination — so the signal is now evidence.
	WatchErrors []string
	// UnloadedGetErr is the error a zero-value CertReloader's GetCertificate
	// returned, rendered. It must be non-empty rather than a nil certificate,
	// which the TLS stack would dereference.
	UnloadedGetErr string
	// Seed is the seed the run was built from.
	Seed uint64
}

// RunBoltCertRotation drives the certificate-rotation surface once and returns
// the evidence. It is bit-reproducible from seed: the key material, the serials
// and the torn-prefix length are all drawn from it in a fixed order.
//
// The returned error is reserved for harness failures; a refused reload is
// EVIDENCE, not an error.
func RunBoltCertRotation(ctx context.Context, seed uint64) (BoltCertRotationEvidence, error) {
	s := NewSeed(seed ^ certRotationSeedMix)
	disk := NewSimDisk(NewSeed(seed^certRotationDiskSeedMix), 0)

	pairA, err := newSimCertPair(s, "rotation-A")
	if err != nil {
		return BoltCertRotationEvidence{}, err
	}
	pairB, err := newSimCertPair(s, "rotation-B")
	if err != nil {
		return BoltCertRotationEvidence{}, err
	}
	pairC, err := newSimCertPair(s, "rotation-C")
	if err != nil {
		return BoltCertRotationEvidence{}, err
	}

	dir, err := os.MkdirTemp("", "sim-cert-rotation-*")
	if err != nil {
		return BoltCertRotationEvidence{}, fmt.Errorf("sim: cert rotation temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	r := &certRotationRunner{
		ctx:      ctx,
		disk:     disk,
		seed:     s,
		dir:      dir,
		certPath: filepath.Join(dir, "server.crt"),
		keyPath:  filepath.Join(dir, "server.key"),
		ev:       &BoltCertRotationEvidence{Seed: seed},
		// Seeded well inside the certificates' own validity window, and advanced by
		// a whole second per projection: the mtime a projection stamps must be
		// strictly greater than every earlier one AND a plausible timestamp, since
		// os.Chtimes near the zero time is not portable.
		projectedMTime: certRotationNotBefore,
	}
	if err := r.drive(pairA, pairB, pairC); err != nil {
		return BoltCertRotationEvidence{}, err
	}
	return *r.ev, nil
}

// certRotationRunner owns the simulated images, their real-file projection, and
// the reloader under test.
type certRotationRunner struct {
	// ctx bounds the run. Seven file rotations, seven os.Chtimes calls and seven
	// real TLS handshakes carry no engine call that would propagate cancellation
	// implicitly, so the run checks it itself at every step boundary — otherwise a
	// swarm bounded by -duration could not interrupt this scenario.
	ctx  context.Context //nolint:containedctx // the runner IS the scoped run; every method is one step of it.
	disk *SimDisk
	// watchErrs collects onError deliveries. It is guarded because Watch calls the
	// callback from its own goroutine.
	watchMu           sync.Mutex
	watchErrs         []string
	seed              *Seed
	reloader          *server.CertReloader
	ev                *BoltCertRotationEvidence
	dir               string
	certPath, keyPath string
	live              simCertPair // the pair currently expected to be in service
	projectedMTime    time.Time
}

// simPath maps a real file path to its key in the simulated disk. The image
// authority is SimDisk; the real path is only where the projection lands.
func simPath(realPath string) string { return "cert/" + filepath.Base(realPath) }

// writeImage stores content as the whole content of the simulated file, then
// projects it onto the real path. It returns the simulated path so a fault
// injector can be aimed at it.
func (r *certRotationRunner) writeImage(realPath string, content []byte) error {
	sp := simPath(realPath)
	if err := r.disk.RemoveAll(sp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sim: cert rotation reset %s: %w", sp, err)
	}
	h, err := r.disk.OpenFile(sp, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("sim: cert rotation open %s: %w", sp, err)
	}
	if _, err := h.Write(content); err != nil {
		_ = h.Close()
		return fmt.Errorf("sim: cert rotation write %s: %w", sp, err)
	}
	if err := h.Close(); err != nil {
		return fmt.Errorf("sim: cert rotation close %s: %w", sp, err)
	}
	return r.project(realPath)
}

// project copies the simulated image onto the real path, advancing its mtime past
// every previous projection.
//
// The mtime is set EXPLICITLY rather than left to the filesystem because
// CertReloader short-circuits a reload when neither file's mtime has advanced
// past the last successful load. On a filesystem whose timestamp granularity is
// coarser than the time two consecutive projections take, an honest rotation
// would be skipped and the scenario would be measuring the clock instead of the
// reloader.
func (r *certRotationRunner) project(realPath string) error {
	img, err := r.disk.ReadFile(simPath(realPath))
	if err != nil {
		return fmt.Errorf("sim: cert rotation read image for %s: %w", realPath, err)
	}
	//nolint:gosec // G306: certificate files are world-readable by design; the key is written 0600 below.
	perm := os.FileMode(0o644)
	if filepath.Ext(realPath) == ".key" {
		perm = 0o600
	}
	if err := os.WriteFile(realPath, img, perm); err != nil {
		return fmt.Errorf("sim: cert rotation project %s: %w", realPath, err)
	}
	r.projectedMTime = r.projectedMTime.Add(time.Second)
	if err := os.Chtimes(realPath, r.projectedMTime, r.projectedMTime); err != nil {
		return fmt.Errorf("sim: cert rotation chtimes %s: %w", realPath, err)
	}
	return nil
}

// removeReal deletes the real file (its simulated counterpart too), modelling a
// rotator that unlinked the old material and died before writing the new.
func (r *certRotationRunner) removeReal(realPath string) error {
	if err := r.disk.RemoveAll(simPath(realPath)); err != nil {
		return fmt.Errorf("sim: cert rotation remove image %s: %w", realPath, err)
	}
	if err := os.Remove(realPath); err != nil {
		return fmt.Errorf("sim: cert rotation remove %s: %w", realPath, err)
	}
	return nil
}

// installPair writes both halves of a pair (cert first, then key), the order a
// rotator uses.
func (r *certRotationRunner) installPair(p simCertPair) error {
	if err := r.writeImage(r.certPath, p.CertPEM); err != nil {
		return err
	}
	return r.writeImage(r.keyPath, p.KeyPEM)
}

// tornPrefixLen returns the seed-chosen length a crash will leave durable: at
// least a quarter of the file and at most three quarters, so the survivor is
// neither empty (indistinguishable from a truncate) nor complete.
func (r *certRotationRunner) tornPrefixLen(content []byte) int {
	quarter := len(content) / 4
	if quarter == 0 {
		quarter = 1
	}
	span := len(content)/2 + 1
	return quarter + r.seed.IntN(span)
}

// writeTornImage writes content to the simulated file in two parts and then
// CRASHES the host, so what survives is decided by SimDisk's durability model
// rather than by the harness: the first `keep` bytes are Sync'd (entering the
// durable image), the remainder is written and left un-synced, and
// [SimDisk.CrashHost] reverts the file to its durable image. It returns the
// number of bytes that actually survived, read back from the disk, so the caller
// asserts against what the crash left rather than what it intended.
//
// The parent directory is synced first: CrashHost drops any file whose dirent
// never became durable, which would model an un-created file rather than a torn
// one.
func (r *certRotationRunner) writeTornImage(realPath string, content []byte, keep int) (int, error) {
	sp := simPath(realPath)
	if err := r.disk.RemoveAll(sp); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("sim: cert rotation reset %s: %w", sp, err)
	}
	h, err := r.disk.OpenFile(sp, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return 0, fmt.Errorf("sim: cert rotation open %s: %w", sp, err)
	}
	if _, err := h.Write(content[:keep]); err != nil {
		_ = h.Close()
		return 0, fmt.Errorf("sim: cert rotation write prefix %s: %w", sp, err)
	}
	if err := h.Sync(); err != nil {
		_ = h.Close()
		return 0, fmt.Errorf("sim: cert rotation sync prefix %s: %w", sp, err)
	}
	if err := r.disk.ParentDirSync(sp); err != nil {
		_ = h.Close()
		return 0, fmt.Errorf("sim: cert rotation dirsync %s: %w", sp, err)
	}
	if _, err := h.Write(content[keep:]); err != nil {
		_ = h.Close()
		return 0, fmt.Errorf("sim: cert rotation write suffix %s: %w", sp, err)
	}
	if err := h.Close(); err != nil {
		return 0, fmt.Errorf("sim: cert rotation close %s: %w", sp, err)
	}
	// The power fails here: everything past the last successful Sync is lost.
	r.disk.CrashHost()
	img, err := r.disk.ReadFile(sp)
	if err != nil {
		return 0, fmt.Errorf("sim: cert rotation read torn image %s: %w", sp, err)
	}
	if err := r.project(realPath); err != nil {
		return 0, err
	}
	return len(img), nil
}

// step runs one rotation attempt: apply the mutation, Reload, then observe what
// is served and whether it still completes a real handshake. keyWant is the size
// of the key the mutation was writing (0 when the step writes no key), against
// which the size actually on disk is recorded.
func (r *certRotationRunner) step(name, wantCN string, wantReloadOK bool, keyWant int, mutate func() error) error {
	if err := r.ctx.Err(); err != nil {
		return fmt.Errorf("sim: cert rotation step %q: %w", name, err)
	}
	if err := mutate(); err != nil {
		return err
	}
	st := CertRotationStep{Name: name, WantCN: wantCN, WantReloadOK: wantReloadOK, KeyWantBytes: keyWant}
	if fi, statErr := os.Stat(r.keyPath); statErr == nil {
		st.KeyBytes = int(fi.Size())
	}
	if err := r.reloader.Reload(); err != nil {
		st.ReloadErr = err.Error()
	} else {
		st.ReloadOK = true
	}
	cn, hsErr := certRotationVerify(r.reloader)
	st.ServedCN = cn
	if hsErr != nil {
		st.HandshakeErr = hsErr.Error()
	} else {
		st.Handshook = true
	}
	r.ev.Steps = append(r.ev.Steps, st)
	return nil
}

// recordWatchErr is the reloader's onError callback: it records the error rather
// than discarding it, so a failed BACKGROUND reload becomes evidence.
func (r *certRotationRunner) recordWatchErr(err error) {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	r.watchErrs = append(r.watchErrs, err.Error())
}

// driveWatch starts a real [server.CertReloader.Watch] goroutine over a BROKEN
// pair and waits for it to report. It is the only arm that exercises the
// background poller, and it is what makes onError a signal rather than a
// decoration: the callback is the operator's sole notification that an unattended
// rotation failed and a stale certificate is still being served.
//
// The wait is bounded and the goroutine always joined, so the arm can neither hang
// nor leak. The poll interval is small REAL time because Watch takes a
// time.Ticker of its own and exposes no clock seam; the assertion is on the
// callback firing, never on how many times.
func (r *certRotationRunner) driveWatch(broken simCertPair) error {
	// Break the key on disk, then let the poller find it.
	if err := r.writeImage(r.keyPath, broken.KeyPEM[:len(broken.KeyPEM)/2]); err != nil {
		return err
	}
	before := len(r.watchErrs)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.reloader.Watch(certRotationWatchInterval, stop)
	}()
	deadline := time.NewTimer(certRotationWatchBudget)
	defer deadline.Stop()
	for {
		r.watchMu.Lock()
		got := len(r.watchErrs)
		r.watchMu.Unlock()
		if got > before {
			break
		}
		select {
		case <-deadline.C:
			close(stop)
			<-done
			return fmt.Errorf("sim: cert rotation watch: onError never fired within %s over a broken pair", certRotationWatchBudget)
		case <-time.After(certRotationWatchInterval):
		}
	}
	close(stop)
	<-done
	r.watchMu.Lock()
	r.ev.WatchErrors = append([]string(nil), r.watchErrs...)
	r.watchMu.Unlock()
	return nil
}

// drive runs the whole rotation sequence.
func (r *certRotationRunner) drive(pairA, pairB, pairC simCertPair) error {
	// The unloaded contract first: a zero-value reloader must REFUSE to serve, not
	// hand the TLS stack a nil certificate.
	var unloaded server.CertReloader
	if _, err := unloaded.GetCertificate(nil); err != nil {
		r.ev.UnloadedGetErr = err.Error()
	}

	// The initial load is documented as mandatory. Prove it: construct over a TORN
	// key and require the constructor to fail.
	if err := r.writeImage(r.certPath, pairA.CertPEM); err != nil {
		return err
	}
	if _, err := r.writeTornImage(r.keyPath, pairA.KeyPEM, r.tornPrefixLen(pairA.KeyPEM)); err != nil {
		return err
	}
	if _, err := server.NewCertReloader(r.certPath, r.keyPath, r.recordWatchErr); err != nil {
		r.ev.InitialLoadTornErr = err.Error()
	}

	// Now install pair A whole and construct for real.
	if err := r.installPair(pairA); err != nil {
		return err
	}
	rl, err := server.NewCertReloader(r.certPath, r.keyPath, r.recordWatchErr)
	if err != nil {
		return fmt.Errorf("sim: cert rotation initial load of a VALID pair failed: %w", err)
	}
	r.reloader = rl
	r.live = pairA
	if err := r.step("initial-load", pairA.CN, true, len(pairA.KeyPEM), func() error { return nil }); err != nil {
		return err
	}

	// A clean rotation to B must take effect.
	if err := r.step("clean-rotation", pairB.CN, true, len(pairB.KeyPEM), func() error {
		r.live = pairB
		return r.installPair(pairB)
	}); err != nil {
		return err
	}

	// A TORN key: the cert landed, only a prefix of the key did. B must stay live.
	if err := r.step("torn-key", pairB.CN, false, len(pairC.KeyPEM), func() error {
		if err := r.writeImage(r.certPath, pairC.CertPEM); err != nil {
			return err
		}
		survived, err := r.writeTornImage(r.keyPath, pairC.KeyPEM, r.tornPrefixLen(pairC.KeyPEM))
		if err != nil {
			return err
		}
		if survived >= len(pairC.KeyPEM) {
			return fmt.Errorf("sim: cert rotation torn-key: the crash left %d of %d bytes — nothing was discarded, so the arm injected no fault", survived, len(pairC.KeyPEM))
		}
		return nil
	}); err != nil {
		return err
	}

	// A GARBLED key: the bytes all landed, then a sector under them went bad. The
	// corruption is injected through SimDisk and re-projected, so the fault comes
	// from the simulator's own injector.
	if err := r.step("garbled-key", pairB.CN, false, len(pairC.KeyPEM), func() error {
		if err := r.writeImage(r.keyPath, pairC.KeyPEM); err != nil {
			return err
		}
		mid := int64(len(pairC.KeyPEM) / 2)
		if err := r.disk.CorruptRange(simPath(r.keyPath), mid, 16); err != nil {
			return fmt.Errorf("sim: cert rotation corrupt key: %w", err)
		}
		return r.project(r.keyPath)
	}); err != nil {
		return err
	}

	// An ABSENT key: unlinked and never rewritten. B must stay live.
	if err := r.step("absent-key", pairB.CN, false, 0, func() error {
		return r.removeReal(r.keyPath)
	}); err != nil {
		return err
	}

	// A MISMATCHED pair: C's certificate with B's key. Both files parse perfectly;
	// only the pairing is wrong, which is why the handshake — not a parse — is the
	// oracle that can see it.
	if err := r.step("mismatched-pair", pairB.CN, false, len(pairB.KeyPEM), func() error {
		return r.writeImage(r.keyPath, pairB.KeyPEM)
	}); err != nil {
		return err
	}

	// The background poller must REPORT a failed rotation, not fail silently. It is
	// recorded as a step like any other, so the roster names what actually ran.
	if err := r.step("watch-reports-failure", pairB.CN, false, len(pairA.KeyPEM), func() error {
		return r.driveWatch(pairA)
	}); err != nil {
		return err
	}

	// Completing the rotation must recover: C's key finally lands beside C's cert.
	if err := r.step("rotation-completed", pairC.CN, true, len(pairC.KeyPEM), func() error {
		r.live = pairC
		return r.writeImage(r.keyPath, pairC.KeyPEM)
	}); err != nil {
		return err
	}
	return nil
}

// certRotationVerify completes a real TLS 1.3 handshake against the certificate
// the reloader currently serves and returns the served leaf's Common Name.
//
// The client trusts ONLY that certificate and verifies the [certRotationHost]
// name, so a successful handshake proves three things a byte comparison cannot:
// the certificate parses, the name matches, and the private key genuinely
// corresponds to the public key in the certificate.
func certRotationVerify(r *server.CertReloader) (cn string, err error) {
	cert, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: certRotationHost})
	if err != nil {
		return "", fmt.Errorf("GetCertificate: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return "", fmt.Errorf("served certificate carries no DER")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("parse served leaf: %w", err)
	}
	cn = leaf.Subject.CommonName

	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	srvErr := make(chan error, 1)
	go func() {
		tlsSrv := tls.Server(serverConn, &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: r.GetCertificate,
			Time:           func() time.Time { return certRotationVerifyAt },
		})
		defer func() { _ = tlsSrv.Close() }()
		if hsErr := tlsSrv.Handshake(); hsErr != nil {
			srvErr <- hsErr
			return
		}
		// Write one byte so the client's read cannot block on a server that
		// completed the handshake and then vanished.
		_, wErr := tlsSrv.Write([]byte{'k'})
		srvErr <- wErr
	}()

	tlsCli := tls.Client(clientConn, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: certRotationHost,
		RootCAs:    roots,
		Time:       func() time.Time { return certRotationVerifyAt },
	})
	if hsErr := tlsCli.Handshake(); hsErr != nil {
		<-srvErr
		return cn, fmt.Errorf("client handshake: %w", hsErr)
	}
	var b [1]byte
	if _, rErr := io.ReadFull(tlsCli, b[:]); rErr != nil {
		<-srvErr
		return cn, fmt.Errorf("read after handshake: %w", rErr)
	}
	if sErr := <-srvErr; sErr != nil {
		return cn, fmt.Errorf("server handshake: %w", sErr)
	}
	return cn, nil
}

// ── oracles ─────────────────────────────────────────────────────────────────

// certRotationExpectedSteps is the step roster the run must produce, in order. It
// is pinned so a dropped step is a violation rather than silently reduced
// coverage.
var certRotationExpectedSteps = []string{
	"initial-load",
	"clean-rotation",
	"torn-key",
	"garbled-key",
	"absent-key",
	"mismatched-pair",
	"watch-reports-failure",
	"rotation-completed",
}

// checkBoltCertRotation adjudicates the evidence against the reloader's contract.
func checkBoltCertRotation(e *BoltCertRotationEvidence) []Violation {
	var v []Violation
	for i := range e.Steps {
		v = append(v, checkCertRotationStep(&e.Steps[i])...)
	}
	if e.UnloadedGetErr == "" {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<cert-rotation-unloaded>",
			Message: "an unloaded CertReloader served a certificate instead of refusing: the TLS stack would be handed a nil pair",
		})
	}
	if e.InitialLoadTornErr == "" {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<cert-rotation-initial-load>",
			Message: "NewCertReloader ACCEPTED a torn key: the mandatory initial load did not fail closed, so a server could start with no usable certificate",
		})
	}
	if len(e.WatchErrors) == 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: "<cert-rotation-watch>",
			Message: "the Watch poller reported NO error over a broken pair: a background rotation failed with no operator-visible signal, leaving a stale certificate in service silently",
		})
	}
	return v
}

// checkCertRotationStep adjudicates one step: whether the reload went the way the
// material on disk dictates, which pair ended up in service, and — always —
// whether the served pair still completes a real handshake. It takes a pointer
// only because the value is 104 bytes; it mutates nothing.
func checkCertRotationStep(st *CertRotationStep) []Violation {
	var v []Violation
	op := "<cert-rotation:" + st.Name + ">"
	if st.WantReloadOK && !st.ReloadOK {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("step %q: Reload of a VALID pair failed: %s", st.Name, st.ReloadErr),
		})
	}
	if !st.WantReloadOK && st.ReloadOK {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("step %q: Reload ACCEPTED broken material and reported success", st.Name),
		})
	}
	if st.ServedCN != st.WantCN {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("step %q: certificate in service is %q, want %q", st.Name, st.ServedCN, st.WantCN),
		})
	}
	// The load-bearing invariant: a failed rotation may not degrade the live
	// certificate. Whatever the reload did, the served pair must still work.
	if !st.Handshook {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("step %q: the served certificate no longer completes a TLS handshake: %s", st.Name, st.HandshakeErr),
		})
	}
	return v
}

// checkBoltCertRotationNonVacuity proves the run exercised the surface: the whole
// roster ran, at least one reload succeeded AND at least one failed, and the
// certificate in service actually CHANGED at some point. Without the last check a
// reloader that ignored every rotation would satisfy every "the old pair is still
// live" assertion above.
func checkBoltCertRotationNonVacuity(e *BoltCertRotationEvidence) []Violation {
	var v []Violation
	seen := make(map[string]bool, len(e.Steps))
	for _, st := range e.Steps {
		seen[st.Name] = true
	}
	for _, want := range certRotationExpectedSteps {
		if !seen[want] {
			v = append(v, Violation{
				Kind: ViolationVacuousRun, Op: "<cert-rotation-nonvacuity>",
				Message: fmt.Sprintf("step %q did not run: the rotation surface was not fully driven", want),
			})
		}
	}
	var ok, failed int
	distinct := make(map[string]bool)
	for _, st := range e.Steps {
		if st.ReloadOK {
			ok++
		} else {
			failed++
		}
		if st.ServedCN != "" {
			distinct[st.ServedCN] = true
		}
	}
	if ok == 0 || failed == 0 {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<cert-rotation-nonvacuity>",
			Message: fmt.Sprintf("run observed %d successful and %d failed reload(s); both must be non-zero or the oracle cannot fail", ok, failed),
		})
	}
	if len(distinct) < 2 {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<cert-rotation-nonvacuity>",
			Message: fmt.Sprintf("only %d distinct certificate(s) were ever in service: a reloader that ignored every rotation would pass every retention check", len(distinct)),
		})
	}
	return append(v, checkCertRotationFaultsDistinct(e)...)
}

// checkCertRotationFaultsDistinct proves the fault arms injected DIFFERENT states,
// using the sizes recorded on disk. The torn and garbled arms produce the identical
// parse error, so without this the roster could name two faults where only one was
// ever applied.
func checkCertRotationFaultsDistinct(e *BoltCertRotationEvidence) []Violation {
	var v []Violation
	byName := make(map[string]CertRotationStep, len(e.Steps))
	for _, st := range e.Steps {
		byName[st.Name] = st
	}
	if torn, ok := byName["torn-key"]; ok && torn.KeyBytes >= torn.KeyWantBytes {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<cert-rotation-nonvacuity>",
			Message: fmt.Sprintf("torn-key left %d of %d key bytes on disk: nothing was truncated, so the TORN fault was never injected",
				torn.KeyBytes, torn.KeyWantBytes),
		})
	}
	if garbled, ok := byName["garbled-key"]; ok && garbled.KeyBytes != garbled.KeyWantBytes {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<cert-rotation-nonvacuity>",
			Message: fmt.Sprintf("garbled-key left %d of %d key bytes on disk: a garbled key is FULL length, so this arm duplicated the torn one instead of corrupting in place",
				garbled.KeyBytes, garbled.KeyWantBytes),
		})
	}
	if absent, ok := byName["absent-key"]; ok && absent.KeyBytes != 0 {
		v = append(v, Violation{
			Kind: ViolationVacuousRun, Op: "<cert-rotation-nonvacuity>",
			Message: fmt.Sprintf("absent-key left %d key bytes on disk: the file was not removed", absent.KeyBytes),
		})
	}
	return v
}

// String renders the evidence for a report: one line per step.
func (e BoltCertRotationEvidence) String() string {
	out := fmt.Sprintf("bolt-cert-rotation evidence (seed %d, %d steps):", e.Seed, len(e.Steps))
	for _, st := range e.Steps {
		verdict := "reload-FAILED"
		if st.ReloadOK {
			verdict = "reload-ok"
		}
		hs := "handshake-FAILED"
		if st.Handshook {
			hs = "handshake-ok"
		}
		out += fmt.Sprintf("\n  %-20s %-14s serving=%-12s %s key=%d/%d", st.Name, verdict, st.ServedCN, hs, st.KeyBytes, st.KeyWantBytes)
		if st.ReloadErr != "" {
			out += " err=" + st.ReloadErr
		}
	}
	out += fmt.Sprintf("\n  unloaded GetCertificate: %q\n  torn initial load: %q\n  watch onError deliveries: %d %v",
		e.UnloadedGetErr, e.InitialLoadTornErr, len(e.WatchErrors), e.WatchErrors)
	return out
}

// ── scenario ────────────────────────────────────────────────────────────────

// certRotationDefaultSeed is the catalogue default for [ScenarioBoltCertRotation].
const certRotationDefaultSeed = 0xC0F1_9E5

// boltCertRotationScenario drives TLS certificate rotation under fault: a torn
// key, a garbled key, an absent key and a mismatched pair must each leave the
// live certificate in service and handshaking, and completing the rotation must
// take effect.
func boltCertRotationScenario() Scenario {
	return Scenario{
		Name:        ScenarioBoltCertRotation,
		Description: "TLS cert rotation under fault: torn / garbled / absent / mismatched key never degrades the live pair (verified by a real TLS handshake)",
		Mode:        ModeDeterministic,
		DefaultSeed: certRotationDefaultSeed,
		run:         runBoltCertRotationScenario,
	}
}

// runBoltCertRotationScenario is the scenario entry point.
func runBoltCertRotationScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltCertRotation(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := append(checkBoltCertRotation(&ev), checkBoltCertRotationNonVacuity(&ev)...)
	if len(v) == 0 {
		return nil, nil
	}
	return &SimReport{
		Scenario:   ScenarioBoltCertRotation,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt cert rotation>"},
		Violations: v,
	}, nil
}
