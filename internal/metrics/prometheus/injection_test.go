package prometheus

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// validMetricName matches the Prometheus metric-name grammar.
var validMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// TestSanitize_AlwaysValid asserts sanitize maps any hostile input onto a
// grammatically valid Prometheus metric name (#1438).
func TestSanitize_AlwaysValid(t *testing.T) {
	t.Parallel()
	hostile := []string{
		"clean_name",
		"with.dots",
		"with-dash",
		"with/slash",
		"newline\ninjected 1\n# TYPE evil counter\nevil",
		"has space",
		`brace{le="x"}`,
		`quote"name`,
		"5leadingdigit",
		"",
		"日本語",
		"tab\tname",
		".-/",
		"\x00nul",
	}
	for _, h := range hostile {
		got := sanitize(h)
		if !validMetricName.MatchString(got) {
			t.Errorf("sanitize(%q) = %q, not a valid Prometheus metric name", h, got)
		}
	}
}

// seriesName extracts the metric name from a single exposition line,
// handling both "# TYPE <name> <type>" comments and "<name>[{labels}] v"
// samples.
func seriesName(line string) string {
	if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
		if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			return rest[:sp]
		}
		return rest
	}
	end := len(line)
	if b := strings.IndexAny(line, "{ "); b >= 0 {
		end = b
	}
	return line[:end]
}

// TestWriteText_NoInjectionViaName feeds hostile names through the public
// Registry methods and asserts the exposition contains only validly named
// series and no forged/injected lines (#1438).
func TestWriteText_NoInjectionViaName(t *testing.T) {
	t.Parallel()
	r := New()
	r.IncCounter("evil\n# TYPE forged counter\nforged 999", 1)
	r.IncCounter("ok_counter", 5)
	r.ObserveLatency(`lat{le="boom"}`, time.Millisecond)

	var sb strings.Builder
	if err := r.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := sb.String()

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		name := seriesName(line)
		if !validMetricName.MatchString(name) {
			t.Errorf("exposition line %q has invalid series name %q", line, name)
		}
	}
	// The newline in the hostile name must have been collapsed, so the
	// forged standalone sample line cannot appear.
	if strings.Contains(out, "\nforged 999") || strings.HasPrefix(out, "forged 999") {
		t.Errorf("forged sample line survived sanitisation:\n%s", out)
	}
}
