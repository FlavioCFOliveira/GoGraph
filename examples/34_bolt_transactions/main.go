// Example 34_bolt_transactions — the Bolt v5 write and transaction surface,
// driven end to end with the official neo4j-go-driver against GoGraph's
// embedded Bolt server.
//
// Where example 23 measures read throughput over many sessions, this example
// exercises the parts of the protocol that carry the ACID and security
// guarantees over the wire:
//
//   - Authentication: the server runs a BasicAuthHandler with a
//     constant-time credential check; a client presenting the wrong password
//     is rejected, the right one is accepted.
//   - Write transactions: a managed ExecuteWrite (BEGIN/RUN/COMMIT) creates a
//     node and the commit is observed by a follow-up read.
//   - Rollback: an explicit transaction that creates a node and then Rolls
//     back leaves the graph unchanged — atomicity on the abort path, over Bolt.
//   - Failure classification and RESET recovery: an invalid statement returns
//     a Bolt FAILURE the driver surfaces as an error, after which the same
//     session recovers (the driver's RESET) and serves the next query.
//   - TLS: the whole flow is repeated once over an encrypted connection
//     (bolt+ssc) against a server started from server.DefaultTLSConfig with a
//     self-signed certificate, proving the transport-security path works.
//
// The server is secure-by-default (it refuses to start with a nil Auth
// handler), so a real credential handler is wired here rather than NoAuth.
//
// # Scale
//
// The default seeds a small directory and runs each scenario once, in
// milliseconds. The graph size is a flag so the write/read latencies become
// observable at scale:
//
//	go run ./examples/34_bolt_transactions -persons 100000
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

const (
	labelPerson = "Person"
	principal   = "neo4j"
)

// config captures the scale and shape knobs.
type config struct {
	password string // the Bolt credential the server requires
	persons  int    // number of :Person nodes seeded
	seed     int64  // reserved for uniformity; the seed graph is deterministic
}

func defaultConfig() config {
	return config{persons: 200, password: "correct-horse-battery-staple", seed: 1}
}

func (c config) validate() error {
	switch {
	case c.persons < 1:
		return fmt.Errorf("persons must be >= 1, got %d", c.persons)
	case c.password == "":
		return fmt.Errorf("password must not be empty")
	}
	return nil
}

func main() {
	cfg := defaultConfig()
	flag.IntVar(&cfg.persons, "persons", cfg.persons, "number of :Person nodes to seed")
	flag.StringVar(&cfg.password, "password", cfg.password, "Bolt credential the server requires")
	flag.Int64Var(&cfg.seed, "seed", cfg.seed, "RNG seed (reserved)")
	flag.Parse()

	if err := run(context.Background(), os.Stdout, cfg); err != nil {
		log.Fatal(err)
	}
}

// run seeds the directory, starts a Bolt server with credential auth, drives
// the write/transaction/failure scenarios over the wire, then repeats a query
// over TLS. Bare lines carry deterministic facts; "# " lines carry telemetry.
func run(ctx context.Context, w io.Writer, cfg config) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	fmt.Fprintf(w, "config.persons=%d\n", cfg.persons)

	eng := newEngine(cfg)

	auth := server.BasicAuthHandler{Validate: server.ConstantTimeValidate(principal, cfg.password)}
	addr, stop, err := startBolt(ctx, eng, auth, nil)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	defer func() { _ = stop() }()

	start := time.Now()
	if err := driveScenarios(ctx, w, "bolt://"+addr, cfg); err != nil {
		return err
	}
	fmt.Fprintf(w, "# scenarios.elapsed=%s\n", time.Since(start).Round(time.Microsecond))

	// Repeat one authenticated read over TLS (bolt+ssc trusts the self-signed
	// certificate) to certify the encrypted transport path.
	tlsOK, err := driveTLS(ctx, cfg)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	fmt.Fprintf(w, "tls.query_succeeded=%t\n", tlsOK)
	return nil
}

// driveScenarios runs the five wire-level scenarios in sequence and prints one
// fact per scenario. Sequencing keeps the node-count assertions deterministic.
func driveScenarios(ctx context.Context, w io.Writer, uri string, cfg config) error {
	// 1. Bad credentials are rejected at connect time.
	badRejected, err := authRejected(ctx, uri, "wrong-password")
	if err != nil {
		return fmt.Errorf("auth-reject probe: %w", err)
	}
	fmt.Fprintf(w, "auth.bad_rejected=%t\n", badRejected)

	// Everything else uses the correct credential.
	driver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(principal, cfg.password, ""))
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}
	defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown

	// 2. Correct credentials are accepted.
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return fmt.Errorf("verify connectivity with valid creds: %w", err)
	}
	fmt.Fprintf(w, "auth.good_accepted=%t\n", true)

	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx) //nolint:errcheck // best-effort close on teardown

	before, err := count(ctx, sess)
	if err != nil {
		return err
	}

	// 3. A committed write transaction is observed by a follow-up read.
	if _, err := sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, rerr := tx.Run(ctx, "CREATE (:Person {name:$n})", map[string]any{"n": "committed-write"})
		return nil, rerr
	}); err != nil {
		return fmt.Errorf("execute write: %w", err)
	}
	afterCommit, err := count(ctx, sess)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "tx.write_committed=%t\n", afterCommit == before+1)

	// 4. An explicit transaction that rolls back leaves the graph unchanged.
	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.Run(ctx, "CREATE (:Person {name:$n})", map[string]any{"n": "rolled-back"}); err != nil {
		return fmt.Errorf("tx run: %w", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		return fmt.Errorf("tx rollback: %w", err)
	}
	afterRollback, err := count(ctx, sess)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "tx.rollback_discarded=%t\n", afterRollback == afterCommit)

	// 5. An invalid statement returns a FAILURE; the session then recovers
	//    (the driver issues RESET) and serves the next query.
	_, failErr := sess.Run(ctx, "THIS IS NOT CYPHER", nil)
	recovered := false
	if failErr != nil {
		if _, err := count(ctx, sess); err == nil {
			recovered = true
		}
	}
	fmt.Fprintf(w, "error.failure_then_recovered=%t\n", failErr != nil && recovered)
	return nil
}

// authRejected connects with the given (wrong) password and reports whether the
// server refused the connection. The driver is always closed.
func authRejected(ctx context.Context, uri, wrongPassword string) (bool, error) {
	driver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(principal, wrongPassword, ""))
	if err != nil {
		return false, err
	}
	defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown
	// A wrong credential must surface as a connectivity/auth error.
	return driver.VerifyConnectivity(ctx) != nil, nil
}

// driveTLS starts a second server over TLS with a self-signed certificate and
// runs one authenticated read over an encrypted bolt+ssc connection.
func driveTLS(ctx context.Context, cfg config) (bool, error) {
	tlsCfg, err := selfSignedTLS()
	if err != nil {
		return false, fmt.Errorf("self-signed cert: %w", err)
	}
	auth := server.BasicAuthHandler{Validate: server.ConstantTimeValidate(principal, cfg.password)}
	addr, stop, err := startBolt(ctx, newEngine(cfg), auth, tlsCfg)
	if err != nil {
		return false, err
	}
	defer func() { _ = stop() }()

	// bolt+ssc trusts a self-signed server certificate.
	driver, err := neo4j.NewDriverWithContext("bolt+ssc://"+addr, neo4j.BasicAuth(principal, cfg.password, ""))
	if err != nil {
		return false, err
	}
	defer driver.Close(ctx) //nolint:errcheck // best-effort close on teardown
	sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx) //nolint:errcheck // best-effort close on teardown
	n, err := count(ctx, sess)
	return err == nil && n == int64(cfg.persons), err
}

// count runs the deterministic :Person count query and returns the scalar.
func count(ctx context.Context, sess neo4j.SessionWithContext) (int64, error) {
	res, err := sess.Run(ctx, "MATCH (p:Person) RETURN count(p) AS c", nil)
	if err != nil {
		return 0, fmt.Errorf("run count: %w", err)
	}
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, fmt.Errorf("count single: %w", err)
	}
	v, ok := rec.Get("c")
	if !ok {
		return 0, fmt.Errorf("count column 'c' missing")
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("count 'c': expected int64, got %T", v)
	}
	return n, nil
}

// newEngine builds an in-memory multigraph engine seeded with cfg.persons
// deterministically-named :Person nodes.
func newEngine(cfg config) *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < cfg.persons; i++ {
		id := fmt.Sprintf("p%06d", i)
		_ = g.AddNode(id)
		_ = g.SetNodeLabel(id, labelPerson)
		_ = g.SetNodeProperty(id, "name", lpg.StringValue(fmt.Sprintf("person-%06d", i)))
	}
	return cypher.NewEngine(g)
}

// startBolt starts a Bolt server (optionally over tlsCfg) on a kernel-assigned
// port and serves it in the background. It returns the address and a stop
// function that shuts the server down and drains the serve goroutine (so the
// example is goroutine-leak-clean).
func startBolt(ctx context.Context, eng *cypher.Engine, auth server.AuthHandler, tlsCfg *tls.Config) (string, func() error, error) {
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: 16,
		ConnTimeout:    30 * time.Second,
		Auth:           auth,
		TLSConfig:      tlsCfg,
	})
	if err != nil {
		return "", nil, fmt.Errorf("new server: %w", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()
	serveCtx, serveCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	stop := func() error {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sErr := srv.Shutdown(shutCtx)
		shutCancel()
		serveCancel()
		<-serveErr
		return sErr
	}
	return addr, stop, nil
}

// selfSignedTLS returns a hardened server tls.Config (DefaultTLSConfig) carrying
// a freshly generated self-signed ECDSA certificate for 127.0.0.1.
func selfSignedTLS() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(0, 0).AddDate(100, 0, 0),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cfg := server.DefaultTLSConfig()
	cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
	return cfg, nil
}
