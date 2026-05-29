package main

// maintenanceCatalogue is the curated set of "typical software-maintenance
// questions" the example answers (SPEC.md §9). Each entry is a complete
// Cypher statement meant to be sent to POST /query, with example
// parameters pointing at the seeded data so it runs as-is. The catalogue
// is the didactic heart of the example: it shows how one multi-layer
// graph answers questions that span code, work and people.

type maintenanceQuery struct {
	Name     string         // short id, e.g. "Q1"
	Question string         // the human question it answers
	Cypher   string         // the Cypher statement (uses $params)
	Example  map[string]any // sample parameters for /query (nil if none)
}

var maintenanceCatalogue = []maintenanceQuery{
	{
		Name:     "Q1",
		Question: "Change-impact: if a component changes, which components are affected and which tasks/developers must be told?",
		Example:  map[string]any{"k": "comp:platform/config.go"},
		Cypher: `MATCH (x:Component {key:$k})
MATCH (affected:Component)-[:DEPENDS_ON*0..]->(x)
OPTIONAL MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[:TOUCHES]->(affected)
RETURN affected.key AS component,
       collect(DISTINCT t.key)   AS impactedTasks,
       collect(DISTINCT dev.key) AS impactedDevelopers
ORDER BY component`,
	},
	{
		Name:     "Q2",
		Question: "Code ownership: who has worked on a component, and how much?",
		Example:  map[string]any{"k": "comp:platform/config.go"},
		Cypher: `MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[r:TOUCHES]->(c:Component {key:$k})
RETURN dev.key AS developer, count(r) AS touches, sum(r.churn) AS totalChurn
ORDER BY totalChurn DESC`,
	},
	{
		Name:     "Q3",
		Question: "Bus-factor: which components have been touched by exactly one developer?",
		Cypher: `MATCH (dev:Developer)-[:ASSIGNED_TO]->(:Task)-[:TOUCHES]->(c:Component)
WITH c, count(DISTINCT dev) AS busFactor, collect(DISTINCT dev.key) AS owners
WHERE busFactor = 1
RETURN c.key AS component, busFactor, owners
ORDER BY component`,
	},
	{
		Name:     "Q4",
		Question: "Workload: what is assigned to a developer and not yet done?",
		Example:  map[string]any{"dev": "dev:alice"},
		Cypher: `MATCH (d:Developer {key:$dev})-[a:ASSIGNED_TO]->(t:Task)
WHERE t.status <> 'done' AND a.state <> 'done'
RETURN t.key AS task, t.title AS title, t.status AS status, t.priority AS priority, a.role AS role
ORDER BY priority DESC`,
	},
	{
		Name:     "Q5",
		Question: "Blocked work: what chain of open blockers reaches a task?",
		Example:  map[string]any{"task": "task:WS-12"},
		Cypher: `MATCH path = (root:Task)-[:BLOCKS*1..]->(t:Task {key:$task})
WHERE root.status <> 'done'
RETURN [n IN nodes(path) | n.key] AS chain
ORDER BY length(path) DESC`,
	},
	{
		Name:     "Q6",
		Question: "Most-depended-upon: which components have the highest dependency in-degree?",
		Cypher: `MATCH (c:Component)<-[:DEPENDS_ON]-(dependent:Component)
RETURN c.key AS component, count(dependent) AS inDegree
ORDER BY inDegree DESC
LIMIT 10`,
	},
	{
		Name:     "Q7",
		Question: "Dependency cycles: which components lie on a dependency cycle?",
		Cypher: `MATCH (c:Component)-[:DEPENDS_ON*1..]->(c)
RETURN DISTINCT c.key AS componentOnCycle
ORDER BY componentOnCycle`,
	},
	{
		Name:     "Q8",
		Question: "History: who touched a component, and when?",
		Example:  map[string]any{"k": "comp:platform/config.go"},
		Cypher: `MATCH (dev:Developer)-[:ASSIGNED_TO]->(t:Task)-[r:TOUCHES]->(c:Component {key:$k})
RETURN dev.key AS developer, t.key AS task,
       r.change_type AS change, r.churn AS churn, r.at AS at
ORDER BY at DESC`,
	},
}
