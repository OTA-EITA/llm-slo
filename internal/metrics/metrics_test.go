package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	var r Registry
	c := r.Counter("x_total", "Things.", "app")
	c.Add(2, "a")
	c.Inc(`we"ird\`)
	h := r.Histogram("d_seconds", "Durations.", []float64{1, 0.5, 1}, "app")
	h.Observe(0.2, "a")
	h.Observe(0.5, "a") // le is inclusive
	h.Observe(3, "a")
	r.Info("b_info", "Build.", []string{"version"}, []string{"1.0"})

	var out bytes.Buffer
	r.WriteText(&out)
	want := `# HELP x_total Things.
# TYPE x_total counter
x_total{app="a"} 2
x_total{app="we\"ird\\"} 1
# HELP d_seconds Durations.
# TYPE d_seconds histogram
d_seconds_bucket{app="a",le="0.5"} 2
d_seconds_bucket{app="a",le="1"} 2
d_seconds_bucket{app="a",le="+Inf"} 3
d_seconds_sum{app="a"} 3.7
d_seconds_count{app="a"} 3
# HELP b_info Build.
# TYPE b_info gauge
b_info{version="1.0"} 1
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestWrongLabelCountIsIgnored(t *testing.T) {
	var r Registry
	c := r.Counter("x_total", "x", "a", "b")
	c.Inc("only-one")
	c.Add(-1, "a", "b")
	var out bytes.Buffer
	r.WriteText(&out)
	if strings.Contains(out.String(), "x_total{") {
		t.Fatalf("bad samples written:\n%s", out.String())
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(2, nil)
	if l.Value("a") != "a" || l.Value("b") != "b" || l.Value("c") != Other || l.Value("a") != "a" {
		t.Fatal("cap not applied")
	}
	al := NewLimiter(100, []string{"bot"})
	if al.Value("bot") != "bot" || al.Value("x") != Other {
		t.Fatal("allow list not applied")
	}
}
