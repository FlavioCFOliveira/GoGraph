package server

import "gograph/bolt/packstream"

// RoutingTable returns the single-host routing table for the server at addr.
// The TTL is hardcoded to 300 seconds. All three roles (WRITE, READ, ROUTE)
// point to the same single-host address.
//
// The returned map matches the Bolt v5 routing table format expected inside a
// SUCCESS metadata "rt" key.
func RoutingTable(addr string) map[string]packstream.Value {
	servers := []packstream.Value{
		map[string]packstream.Value{
			"addresses": []packstream.Value{addr},
			"role":      "WRITE",
		},
		map[string]packstream.Value{
			"addresses": []packstream.Value{addr},
			"role":      "READ",
		},
		map[string]packstream.Value{
			"addresses": []packstream.Value{addr},
			"role":      "ROUTE",
		},
	}

	return map[string]packstream.Value{
		"rt": map[string]packstream.Value{
			"ttl":     int64(300),
			"db":      "",
			"servers": servers,
		},
	}
}
