package main

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// This file builds the deterministic fixture that gives the example a
// realistic shape: a small software-house ("webshop" e-commerce backend)
// with a Code layer (modules and components plus their dependency graph),
// a People layer (developers and teams) and a Work layer (tasks, sprints
// and workflow states), stitched together by the inter-layer spine
// Developer-[:ASSIGNED_TO]->Task-[:TOUCHES]->Component.
//
// The data is sized so every maintenance query in SPEC.md §9 returns a
// meaningful answer: there is a dependency cycle (orders<->payments), a
// transitive blocking chain (WS-14 -> WS-10 -> WS-12), several
// bus-factor-1 components, and developers holding both completed (past)
// and planned work. All timestamps are fixed ISO-8601 UTC strings so the
// node/edge counts and byte output are reproducible.

// prop is a single typed property assignment, used to keep the fixture
// declarations compact.
type prop struct {
	k string
	v lpg.PropertyValue
}

func ps(k, v string) prop       { return prop{k, lpg.StringValue(v)} }
func pi(k string, v int64) prop { return prop{k, lpg.Int64Value(v)} }
func pb(k string, v bool) prop  { return prop{k, lpg.BoolValue(v)} }

// seeder issues AddNode/SetNodeLabel/SetNodeProperty and
// AddEdge/SetEdgeLabel/SetEdgeProperty calls against a transaction,
// short-circuiting on the first error so the fixture body stays linear.
type seeder struct {
	tx  *txn.Tx[string, float64]
	err error
}

// node adds a node with its layer label, its type label and the given
// properties.
func (s *seeder) node(key, layer, typ string, props ...prop) {
	if s.err != nil {
		return
	}
	if s.err = s.tx.AddNode(key); s.err != nil {
		return
	}
	if s.err = s.tx.SetNodeLabel(key, layer); s.err != nil {
		return
	}
	if s.err = s.tx.SetNodeLabel(key, typ); s.err != nil {
		return
	}
	// Store the natural key as a queryable `key` property. The
	// AddNode identifier is the node's internal identity, but Cypher
	// patterns such as {key:$k} match a property, so every node carries
	// its key explicitly (mirrors the convention in example #24).
	if s.err = s.tx.SetNodeProperty(key, "key", lpg.StringValue(key)); s.err != nil {
		return
	}
	for _, p := range props {
		if s.err = s.tx.SetNodeProperty(key, p.k, p.v); s.err != nil {
			return
		}
	}
}

// edge adds a directed edge of the given type (src->dst) with a unit
// weight and the given properties.
func (s *seeder) edge(src, dst, typ string, props ...prop) {
	if s.err != nil {
		return
	}
	if s.err = s.tx.AddEdge(src, dst, 1.0); s.err != nil {
		return
	}
	if s.err = s.tx.SetEdgeLabel(src, dst, typ); s.err != nil {
		return
	}
	for _, p := range props {
		if s.err = s.tx.SetEdgeProperty(src, dst, p.k, p.v); s.err != nil {
			return
		}
	}
}

// seedKey is a node whose presence marks the graph as already seeded.
const seedKey = "repo:webshop"

// hasSeed reports whether the fixture is already loaded.
func hasSeed(g *lpg.Graph[string, float64]) bool {
	return g.HasNodeLabel(seedKey, typeRepository)
}

// seedFixture loads the fixture in a single atomic transaction. It is
// idempotent: if the graph already contains the seed it returns
// (false, nil) without writing. On a fresh graph it commits the whole
// fixture and returns (true, nil).
func seedFixture(store *txn.Store[string, float64]) (bool, error) {
	if hasSeed(store.Graph()) {
		return false, nil
	}
	tx := store.Begin()
	if err := applyFixture(tx); err != nil {
		_ = tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("seed commit: %w", err)
	}
	return true, nil
}

// applyFixture issues every node and edge of the fixture against tx. All
// nodes are added before any edge so endpoint nodes always exist.
func applyFixture(tx *txn.Tx[string, float64]) error {
	s := &seeder{tx: tx}

	// ---------------------------------------------------------------
	// People layer — developers and teams.
	// ---------------------------------------------------------------
	s.node("team:platform", layerPeople, typeTeam, ps("name", "Platform"))
	s.node("team:product", layerPeople, typeTeam, ps("name", "Product"))

	s.node("dev:alice", layerPeople, typeDeveloper,
		ps("name", "Alice Pereira"), ps("handle", "alice"), ps("email", "alice@webshop.dev"),
		ps("seniority", "senior"), pb("active", true))
	s.node("dev:bob", layerPeople, typeDeveloper,
		ps("name", "Bob Martins"), ps("handle", "bob"), ps("email", "bob@webshop.dev"),
		ps("seniority", "mid"), pb("active", true))
	s.node("dev:carol", layerPeople, typeDeveloper,
		ps("name", "Carol Nunes"), ps("handle", "carol"), ps("email", "carol@webshop.dev"),
		ps("seniority", "senior"), pb("active", true))
	s.node("dev:dave", layerPeople, typeDeveloper,
		ps("name", "Dave Sousa"), ps("handle", "dave"), ps("email", "dave@webshop.dev"),
		ps("seniority", "junior"), pb("active", true))
	s.node("dev:erin", layerPeople, typeDeveloper,
		ps("name", "Erin Lopes"), ps("handle", "erin"), ps("email", "erin@webshop.dev"),
		ps("seniority", "staff"), pb("active", true))
	s.node("dev:frank", layerPeople, typeDeveloper,
		ps("name", "Frank Dias"), ps("handle", "frank"), ps("email", "frank@webshop.dev"),
		ps("seniority", "mid"), pb("active", false))

	// ---------------------------------------------------------------
	// Code layer — repository, modules, components.
	// ---------------------------------------------------------------
	s.node("repo:webshop", layerCode, typeRepository,
		ps("name", "webshop"), ps("url", "git@webshop.dev:webshop.git"),
		ps("created_at", "2025-11-03T09:00:00Z"))

	mods := []struct{ key, name, path string }{
		{"mod:api", "api", "internal/api"},
		{"mod:catalog", "catalog", "internal/catalog"},
		{"mod:orders", "orders", "internal/orders"},
		{"mod:payments", "payments", "internal/payments"},
		{"mod:platform", "platform", "internal/platform"},
	}
	for _, m := range mods {
		s.node(m.key, layerCode, typeModule, ps("name", m.name), ps("path", m.path))
	}

	comps := []struct {
		key, name, path, kind string
		loc                   int64
	}{
		{"comp:api/router.go", "router.go", "internal/api/router.go", "file", 210},
		{"comp:api/handlers.go", "handlers.go", "internal/api/handlers.go", "file", 540},
		{"comp:catalog/service.go", "service.go", "internal/catalog/service.go", "file", 480},
		{"comp:catalog/repository.go", "repository.go", "internal/catalog/repository.go", "file", 360},
		{"comp:orders/service.go", "service.go", "internal/orders/service.go", "file", 520},
		{"comp:orders/repository.go", "repository.go", "internal/orders/repository.go", "file", 300},
		{"comp:payments/gateway.go", "gateway.go", "internal/payments/gateway.go", "file", 280},
		{"comp:payments/service.go", "service.go", "internal/payments/service.go", "file", 410},
		{"comp:platform/db.go", "db.go", "internal/platform/db.go", "file", 230},
		{"comp:platform/logging.go", "logging.go", "internal/platform/logging.go", "file", 140},
		{"comp:platform/auth.go", "auth.go", "internal/platform/auth.go", "file", 260},
		{"comp:platform/config.go", "config.go", "internal/platform/config.go", "file", 120},
	}
	for _, c := range comps {
		s.node(c.key, layerCode, typeComponent,
			ps("name", c.name), ps("path", c.path), ps("kind", c.kind),
			ps("language", "go"), pi("loc", c.loc))
	}

	// ---------------------------------------------------------------
	// Work layer — workflow states, sprints, tasks.
	// ---------------------------------------------------------------
	states := []struct {
		key, name string
		order     int64
		terminal  bool
	}{
		{"state:todo", "todo", 1, false},
		{"state:in_progress", "in_progress", 2, false},
		{"state:in_review", "in_review", 3, false},
		{"state:done", "done", 4, true},
	}
	for _, st := range states {
		s.node(st.key, layerWork, typeWorkflowState,
			ps("name", st.name), pi("order", st.order), pb("is_terminal", st.terminal))
	}

	s.node("sprint:2026-S1", layerWork, typeSprint,
		ps("name", "2026 Sprint 1"), ps("starts_at", "2026-01-06T00:00:00Z"), ps("ends_at", "2026-03-27T00:00:00Z"))
	s.node("sprint:2026-S2", layerWork, typeSprint,
		ps("name", "2026 Sprint 2"), ps("starts_at", "2026-03-30T00:00:00Z"), ps("ends_at", "2026-06-19T00:00:00Z"))

	tasks := []struct {
		key, title, status, kind, created string
		priority                          int64
	}{
		// Completed (past) work — sprint 1.
		{"task:WS-1", "Bootstrap platform config loader", "done", "chore", "2026-01-07T10:00:00Z", 5},
		{"task:WS-2", "Implement database access layer", "done", "feature", "2026-01-12T10:00:00Z", 7},
		{"task:WS-3", "Add structured logging", "done", "feature", "2026-01-19T10:00:00Z", 4},
		{"task:WS-4", "Build catalog service and repository", "done", "feature", "2026-01-28T10:00:00Z", 7},
		{"task:WS-5", "Add authentication middleware", "done", "feature", "2026-02-06T10:00:00Z", 6},
		{"task:WS-6", "Build orders service and repository", "done", "feature", "2026-02-18T10:00:00Z", 7},
		{"task:WS-7", "Integrate payments gateway", "done", "feature", "2026-03-04T10:00:00Z", 8},
		{"task:WS-8", "Wire API router and handlers", "done", "feature", "2026-03-18T10:00:00Z", 6},
		// Pending / planned work — sprint 2.
		{"task:WS-9", "Add product search to catalog", "in_progress", "feature", "2026-04-01T10:00:00Z", 7},
		{"task:WS-10", "Refactor orders/payments dependency cycle", "todo", "refactor", "2026-04-02T10:00:00Z", 8},
		{"task:WS-11", "Rate-limit authentication tokens", "todo", "feature", "2026-04-03T10:00:00Z", 5},
		{"task:WS-12", "Implement payments refund flow", "in_review", "feature", "2026-04-04T10:00:00Z", 8},
		{"task:WS-13", "Add catalog caching layer", "todo", "feature", "2026-04-05T10:00:00Z", 6},
		{"task:WS-14", "Add database connection pooling", "todo", "chore", "2026-04-06T10:00:00Z", 6},
	}
	for _, t := range tasks {
		s.node(t.key, layerWork, typeTask,
			ps("title", t.title), ps("status", t.status), ps("kind", t.kind),
			pi("priority", t.priority), ps("created_at", t.created))
	}

	// ---------------------------------------------------------------
	// Code-layer intra edges — containment and dependencies.
	// ---------------------------------------------------------------
	for _, m := range mods {
		s.edge("repo:webshop", m.key, relContains)
	}
	contains := map[string][]string{
		"mod:api":      {"comp:api/router.go", "comp:api/handlers.go"},
		"mod:catalog":  {"comp:catalog/service.go", "comp:catalog/repository.go"},
		"mod:orders":   {"comp:orders/service.go", "comp:orders/repository.go"},
		"mod:payments": {"comp:payments/gateway.go", "comp:payments/service.go"},
		"mod:platform": {"comp:platform/db.go", "comp:platform/logging.go", "comp:platform/auth.go", "comp:platform/config.go"},
	}
	for _, m := range mods { // deterministic order
		for _, c := range contains[m.key] {
			s.edge(m.key, c, relContains)
		}
	}

	// DEPENDS_ON: dependent -> dependency. The last edge closes a cycle
	// orders/service <-> payments/service (a deliberate architectural
	// smell surfaced by query Q7).
	deps := []struct {
		src, dst, kind string
		strength       int64
	}{
		{"comp:api/router.go", "comp:api/handlers.go", "import", 1},
		{"comp:api/handlers.go", "comp:catalog/service.go", "call", 6},
		{"comp:api/handlers.go", "comp:orders/service.go", "call", 5},
		{"comp:api/handlers.go", "comp:platform/auth.go", "call", 3},
		{"comp:catalog/service.go", "comp:catalog/repository.go", "call", 8},
		{"comp:catalog/service.go", "comp:platform/logging.go", "call", 2},
		{"comp:catalog/repository.go", "comp:platform/db.go", "call", 7},
		{"comp:orders/service.go", "comp:orders/repository.go", "call", 9},
		{"comp:orders/service.go", "comp:payments/service.go", "call", 4},
		{"comp:orders/service.go", "comp:catalog/service.go", "call", 3},
		{"comp:orders/repository.go", "comp:platform/db.go", "call", 6},
		{"comp:payments/service.go", "comp:payments/gateway.go", "call", 5},
		{"comp:payments/gateway.go", "comp:platform/logging.go", "call", 2},
		{"comp:platform/auth.go", "comp:platform/config.go", "call", 2},
		{"comp:platform/db.go", "comp:platform/config.go", "call", 2},
		{"comp:platform/logging.go", "comp:platform/config.go", "call", 1},
		{"comp:payments/service.go", "comp:orders/service.go", "call", 2}, // closes the cycle
	}
	for _, d := range deps {
		s.edge(d.src, d.dst, relDependsOn, ps("kind", d.kind), pi("strength", d.strength))
	}

	// ---------------------------------------------------------------
	// People-layer intra edges — team membership.
	// ---------------------------------------------------------------
	members := []struct{ dev, team string }{
		{"dev:alice", "team:product"},
		{"dev:bob", "team:product"},
		{"dev:carol", "team:platform"},
		{"dev:dave", "team:product"},
		{"dev:erin", "team:platform"},
		{"dev:frank", "team:platform"},
	}
	for _, m := range members {
		s.edge(m.dev, m.team, relMemberOf, ps("since", "2025-11-03T00:00:00Z"))
	}

	// ---------------------------------------------------------------
	// Work-layer intra edges — workflow structure.
	// ---------------------------------------------------------------
	taskState := map[string]string{
		"task:WS-1": "state:done", "task:WS-2": "state:done", "task:WS-3": "state:done",
		"task:WS-4": "state:done", "task:WS-5": "state:done", "task:WS-6": "state:done",
		"task:WS-7": "state:done", "task:WS-8": "state:done",
		"task:WS-9": "state:in_progress", "task:WS-10": "state:todo", "task:WS-11": "state:todo",
		"task:WS-12": "state:in_review", "task:WS-13": "state:todo", "task:WS-14": "state:todo",
	}
	for _, t := range tasks { // deterministic order
		s.edge(t.key, taskState[t.key], relHasState)
	}
	taskSprint := map[string]string{
		"task:WS-1": "sprint:2026-S1", "task:WS-2": "sprint:2026-S1", "task:WS-3": "sprint:2026-S1",
		"task:WS-4": "sprint:2026-S1", "task:WS-5": "sprint:2026-S1", "task:WS-6": "sprint:2026-S1",
		"task:WS-7": "sprint:2026-S1", "task:WS-8": "sprint:2026-S1",
		"task:WS-9": "sprint:2026-S2", "task:WS-10": "sprint:2026-S2", "task:WS-11": "sprint:2026-S2",
		"task:WS-12": "sprint:2026-S2", "task:WS-13": "sprint:2026-S2", "task:WS-14": "sprint:2026-S2",
	}
	for _, t := range tasks {
		s.edge(t.key, taskSprint[t.key], relInSprint)
	}

	// NEXT: build-order sequencing of the foundational work.
	for _, n := range [][2]string{
		{"task:WS-1", "task:WS-2"}, {"task:WS-2", "task:WS-4"},
		{"task:WS-4", "task:WS-6"}, {"task:WS-6", "task:WS-8"},
	} {
		s.edge(n[0], n[1], relNext)
	}

	// SUBTASK_OF: caching and rate-limiting are decomposed from search.
	s.edge("task:WS-13", "task:WS-9", relSubtaskOf)
	s.edge("task:WS-11", "task:WS-9", relSubtaskOf)

	// BLOCKS: blocker -> blocked. A transitive chain reaches WS-12:
	// WS-14 -> WS-10 -> WS-12 (and WS-10 also blocks WS-13).
	s.edge("task:WS-14", "task:WS-10", relBlocks, ps("reason", "pooling must land before the cycle refactor"))
	s.edge("task:WS-10", "task:WS-12", relBlocks, ps("reason", "refund flow needs orders/payments decoupled"))
	s.edge("task:WS-10", "task:WS-13", relBlocks, ps("reason", "caching waits on the service refactor"))

	// ---------------------------------------------------------------
	// Inter-layer coupling — ASSIGNED_TO (People->Work) and TOUCHES
	// (Work->Code). Completed tasks carry TOUCHES (realised work);
	// planned tasks carry only ASSIGNED_TO {state:'planned'}.
	// ---------------------------------------------------------------
	type assign struct {
		dev, task, role, state string
	}
	assigns := []assign{
		{"dev:erin", "task:WS-1", "author", "done"},
		{"dev:carol", "task:WS-2", "author", "done"},
		{"dev:carol", "task:WS-3", "author", "done"},
		{"dev:alice", "task:WS-4", "author", "done"},
		{"dev:bob", "task:WS-4", "reviewer", "done"},
		{"dev:dave", "task:WS-5", "author", "done"},
		{"dev:bob", "task:WS-6", "author", "done"},
		{"dev:frank", "task:WS-7", "author", "done"},
		{"dev:alice", "task:WS-8", "author", "done"},
		{"dev:alice", "task:WS-9", "author", "active"},
		{"dev:bob", "task:WS-10", "author", "planned"},
		{"dev:dave", "task:WS-11", "author", "planned"},
		{"dev:frank", "task:WS-12", "author", "active"},
		{"dev:carol", "task:WS-12", "reviewer", "active"},
		{"dev:alice", "task:WS-13", "author", "planned"},
		{"dev:carol", "task:WS-14", "author", "planned"},
	}
	for _, a := range assigns {
		s.edge(a.dev, a.task, relAssignedTo, ps("role", a.role), ps("state", a.state))
	}

	// TOUCHES: which completed task changed which component, with churn
	// and the change timestamp (drives ownership, bus-factor, history).
	type touch struct {
		task, comp, change string
		churn              int64
		at                 string
	}
	touches := []touch{
		{"task:WS-1", "comp:platform/config.go", "add", 120, "2026-01-08T16:00:00Z"},
		{"task:WS-2", "comp:platform/db.go", "add", 340, "2026-01-15T16:00:00Z"},
		{"task:WS-2", "comp:platform/config.go", "modify", 25, "2026-01-15T16:30:00Z"},
		{"task:WS-3", "comp:platform/logging.go", "add", 180, "2026-01-20T16:00:00Z"},
		{"task:WS-4", "comp:catalog/service.go", "add", 420, "2026-02-03T16:00:00Z"},
		{"task:WS-4", "comp:catalog/repository.go", "add", 260, "2026-02-03T16:30:00Z"},
		{"task:WS-5", "comp:platform/auth.go", "add", 210, "2026-02-10T16:00:00Z"},
		{"task:WS-6", "comp:orders/service.go", "add", 380, "2026-02-24T16:00:00Z"},
		{"task:WS-6", "comp:orders/repository.go", "add", 240, "2026-02-24T16:30:00Z"},
		{"task:WS-7", "comp:payments/gateway.go", "add", 300, "2026-03-09T16:00:00Z"},
		{"task:WS-7", "comp:payments/service.go", "add", 190, "2026-03-09T16:30:00Z"},
		{"task:WS-8", "comp:api/router.go", "add", 150, "2026-03-23T16:00:00Z"},
		{"task:WS-8", "comp:api/handlers.go", "add", 330, "2026-03-23T16:30:00Z"},
	}
	for _, t := range touches {
		s.edge(t.task, t.comp, relTouches, ps("change_type", t.change), pi("churn", t.churn), ps("at", t.at))
	}

	return s.err
}
