package sim

// notifications.go — DST adjudication of the engine's out-of-band query
// notifications (rmp #2462).
//
// cypher/notification.go analyses every planned query for a Cartesian product
// between disconnected pattern components and attaches an advisory to the
// result. The DST issued Cartesian shapes routinely — `MATCH (a:Person),
// (b:Person) CREATE (a)-[:FOLLOWS]->(b)` is one — but never once looked at a
// notification, so the whole advisory surface was unguarded: it could have
// stopped firing, or started firing on connected patterns, without a single
// scenario noticing.
//
// This checker pins both directions, which is what makes it more than a smoke
// test: a genuinely disconnected shape MUST carry the warning, and a connected
// one MUST NOT. An implementation that returned the advisory unconditionally,
// or never, fails one arm or the other.
//
// # Where notifications surface
//
// A notification reaches a caller through [cypher.Result.Notifications] (and,
// for a driver, the Bolt SUCCESS metadata "notifications" field, which
// bolt/server/session.go fills from the same accessor). The simulator reads it
// through the [notificationReporter] facet of its narrow [Result] view.
//
// Notifications are attached on the READ path: [cypher.Engine.Run] populates
// them from the plan-cache entry, while the RunInTx write path leaves them
// empty. Every probe below is therefore a read, which is also what a real
// caller issues for a Cartesian MATCH.

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// notificationReporter is the optional facet a [Result] implements when it can
// surface the engine's plan-time advisories. It mirrors [counterReporter]: the
// checker asserts on it explicitly, so an adapter that stopped implementing it
// makes the probe fail loudly instead of silently checking nothing.
type notificationReporter interface {
	// Notifications returns the out-of-band advisories attached to the result,
	// nil when the query produced none.
	Notifications() []cypher.Notification
}

// cartesianProductNotificationCode is the notification code the engine emits
// for a query building a cross product between disconnected patterns. It
// mirrors Neo4j's code so a Bolt driver receives a familiar advisory; pinning
// the exact string is deliberate, because the code is the part a downstream
// consumer matches on.
const cartesianProductNotificationCode = "Neo.ClientNotification.Statement.CartesianProductWarning"

// notificationProbe is one query paired with whether the Cartesian-product
// advisory must be attached to its result.
type notificationProbe struct {
	label      string
	query      string
	wantWarned bool
}

// cartesianNotificationProbes are the four shapes the checker adjudicates. Two
// must warn and two must not, so neither an always-on nor an always-off
// implementation can pass:
//
//   - comma-separated disconnected paths — the canonical Cartesian product;
//   - two sequential MATCH clauses over disconnected patterns — the same cross
//     product written differently, which the analyser must also catch;
//   - a connected pattern — one component, so no cross product exists;
//   - disconnected paths joined by a WHERE predicate that references both — the
//     predicate connects the components, and the advisory must be suppressed.
//
// Every probe returns count(*), so each one drains exactly one row regardless
// of population size and the checker stays O(1) in rows.
var cartesianNotificationProbes = [...]notificationProbe{
	{
		label:      "cartesian (comma-separated paths)",
		query:      "MATCH (a:Person),(b:Person) RETURN count(*)",
		wantWarned: true,
	},
	{
		label:      "cartesian (sequential MATCH clauses)",
		query:      "MATCH (a:Person) MATCH (b:Person) RETURN count(*)",
		wantWarned: true,
	},
	{
		label:      "connected (single component)",
		query:      "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN count(*)",
		wantWarned: false,
	},
	{
		label:      "disconnected paths joined by WHERE",
		query:      "MATCH (a:Person),(b:Person) WHERE a.age = b.age RETURN count(*)",
		wantWarned: false,
	},
}

// CheckCartesianNotification asserts the engine's Cartesian-product advisory
// fires for exactly the queries that build a cross product between disconnected
// patterns, and stays silent for the ones that do not. See
// [cartesianNotificationProbes] for the four shapes and why both directions are
// required.
//
// The check is read-only and population-independent: it inspects the plan-time
// advisory, which is a function of the query text alone, so it is meaningful
// even on an empty graph and can never perturb a run. A clean pass returns nil.
func CheckCartesianNotification(tick int64, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation

	for _, p := range cartesianNotificationProbes {
		notes, err := queryNotifications(ctx, engine, p.query)
		if err != nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: "cartesian notification",
				Message: fmt.Sprintf("%s: %v\nquery: %s", p.label, err, p.query),
			})
			continue
		}

		warned := false
		for _, n := range notes {
			if n.Code == cartesianProductNotificationCode {
				warned = true
				break
			}
		}
		if warned == p.wantWarned {
			continue
		}
		if p.wantWarned {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: "cartesian notification",
				Message: fmt.Sprintf(
					"%s: expected the %s advisory, got %d notification(s) %s\nquery: %s",
					p.label, cartesianProductNotificationCode, len(notes), notificationCodes(notes), p.query),
			})
			continue
		}
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "cartesian notification",
			Message: fmt.Sprintf(
				"%s: the %s advisory was attached to a query that builds NO cross product between "+
					"disconnected patterns\nquery: %s",
				p.label, cartesianProductNotificationCode, p.query),
		})
	}
	return vs
}

// queryNotifications runs query on the read path, drains it, and returns the
// advisories attached to the result. A result that does not implement
// [notificationReporter] is an error rather than an empty list, so the probe can
// never pass by silently checking nothing.
func queryNotifications(ctx context.Context, engine *EngineAdapter, query string) ([]cypher.Notification, error) {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return nil, fmt.Errorf("query error: %w", err)
	}
	for res.Next() { //nolint:revive // draining is the point
	}
	drainErr := res.Err()
	notes, facetErr := notificationsOf(res)
	_ = res.Close()
	if drainErr != nil {
		return nil, fmt.Errorf("drain error: %w", drainErr)
	}
	return notes, facetErr
}

// notificationsOf extracts the advisories from a drained [Result]. A result that
// does not implement [notificationReporter] is an ERROR rather than an empty
// list: silently treating a missing facet as "no notifications" would make every
// probe below pass while checking nothing, which is precisely the dead-arm
// failure mode this checker guards against.
func notificationsOf(res Result) ([]cypher.Notification, error) {
	nr, ok := res.(notificationReporter)
	if !ok {
		return nil, fmt.Errorf("result type %T does not expose Notifications; the advisory probe would "+
			"otherwise pass while checking nothing", res)
	}
	return nr.Notifications(), nil
}

// notificationCodes renders the codes of notes for a failure message.
func notificationCodes(notes []cypher.Notification) string {
	if len(notes) == 0 {
		return "[]"
	}
	codes := make([]string, 0, len(notes))
	for _, n := range notes {
		codes = append(codes, n.Code)
	}
	return fmt.Sprintf("%v", codes)
}
