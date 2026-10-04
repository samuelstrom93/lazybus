package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPercentilesAndAggregation(t *testing.T) {
	cases := []struct {
		samples                  []float64
		median, p75, p95, lo, hi float64
	}{
		{[]float64{7}, 7, 7, 7, 7, 7},
		{[]float64{4, 1, 3, 2}, 2, 3, 4, 1, 4},
		{[]float64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, 5, 8, 10, 1, 10},
		{[]float64{5, 1, 4, 2, 3}, 3, 4, 5, 1, 5},
	}
	for _, c := range cases {
		in := append([]float64(nil), c.samples...)
		r := newResult("X/ns_per_op", unitNs, in)
		if r.Median != c.median || r.P75 != c.p75 || r.P95 != c.p95 || r.Min != c.lo || r.Max != c.hi {
			t.Errorf("%v: median %v p75 %v p95 %v min %v max %v", c.samples, r.Median, r.P75, r.P95, r.Min, r.Max)
		}
		if !reflect.DeepEqual(r.Samples, c.samples) {
			t.Errorf("samples reordered: %v, want %v", r.Samples, c.samples)
		}
	}
}

func TestParseBench(t *testing.T) {
	out := `goos: linux
goarch: amd64
pkg: github.com/samuelstrom93/lazybus/internal/ui
cpu: AMD RYZEN AI MAX+ 395 w/ Radeon 8060S
BenchmarkViewLargeList-32        	     462	   2588571 ns/op	  483656 B/op	   11255 allocs/op
BenchmarkLoadLargeList-32        	      27	  39497156 ns/op	   3869800 heap-bytes	401448091 B/op	   11340 allocs/op
BenchmarkViewLargeList-32        	     465	   2599022.5 ns/op	  483657 B/op	   11255 allocs/op
BenchmarkNoSuffix 	 10	 5 ns/op
--- BENCH: BenchmarkSomething-32
PASS
ok  	github.com/samuelstrom93/lazybus/internal/ui	20.1s
`
	got, err := parseBench(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if got.cpu != "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S" {
		t.Errorf("cpu %q", got.cpu)
	}
	want := []metric{
		{"ViewLargeList/ns_per_op", unitNs, []float64{2588571, 2599022.5}},
		{"ViewLargeList/bytes_per_op", unitBytes, []float64{483656, 483657}},
		{"ViewLargeList/allocs_per_op", unitCount, []float64{11255, 11255}},
		{"LoadLargeList/ns_per_op", unitNs, []float64{39497156}},
		{"LoadLargeList/heap_bytes", unitBytes, []float64{3869800}},
		{"LoadLargeList/bytes_per_op", unitBytes, []float64{401448091}},
		{"LoadLargeList/allocs_per_op", unitCount, []float64{11340}},
		{"NoSuffix/ns_per_op", unitNs, []float64{5}},
	}
	if !reflect.DeepEqual(got.metrics, want) {
		t.Errorf("metrics:\n%+v\nwant\n%+v", got.metrics, want)
	}

	if _, err := parseBench(strings.NewReader("PASS\n")); err == nil {
		t.Error("output without results parsed")
	}
}

func TestSelectBase(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		head  string
		want  string
	}{
		{"latest stable below", []string{"v0.1.0", "v0.2.0", "v0.10.0", "v0.3.0"}, "v0.11.0", "v0.10.0"},
		{"strictly lower", []string{"v0.1.0", "v0.2.0"}, "v0.2.0", "v0.1.0"},
		{"prereleases skipped", []string{"v0.1.0", "v0.2.0-rc.1"}, "v0.2.0", "v0.1.0"},
		{"prerelease head is below its stable", []string{"v0.1.0", "v0.2.0"}, "v0.2.0-rc.2", "v0.1.0"},
		{"none", []string{"v0.2.0-rc.1", "v0.3.0"}, "v0.2.0", ""},
		{"empty dir", nil, "v0.1.0", ""},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for _, f := range c.files {
			if err := os.WriteFile(filepath.Join(dir, "bench-"+f+".json"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		os.WriteFile(filepath.Join(dir, "notes.json"), nil, 0o644)
		head, _ := parseVersion(c.head)
		got, err := selectBase(dir, head)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if c.want != "" {
			want = filepath.Join(dir, "bench-"+c.want+".json")
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", c.name, got, want)
		}
	}
}

func TestVersionLess(t *testing.T) {
	order := []string{"v0.9.9", "v0.10.0", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.1.0", "v2.0.0"}
	for i := 1; i < len(order); i++ {
		a, _ := parseVersion(order[i-1])
		b, _ := parseVersion(order[i])
		if !a.less(b) || b.less(a) {
			t.Errorf("want %s < %s", order[i-1], order[i])
		}
	}
	for _, bad := range []string{"1.0.0", "v1.0", "v1.0.0+build", ""} {
		if _, ok := parseVersion(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func res(name string, median float64, comparable bool, th *Threshold) Result {
	return Result{Name: name, Unit: unitNs, Samples: []float64{median}, Median: median, Comparable: comparable, Threshold: th}
}

func TestCompareStatuses(t *testing.T) {
	th := &Threshold{MaxRegressionRatio: 1.15, MinAbsoluteRegression: 10}
	base := Snapshot{Version: "v0.1.0", Results: []Result{
		res("Faster", 100, true, th),
		res("Ratio", 100, true, th),         // 100 → 116: both
		res("RatioOnly", 20, true, th),      // 20 → 25: +25% but +5 < 10
		res("AbsoluteOnly", 1000, true, th), // 1000 → 1100: +100 but +10%
		res("ExactRatio", 100, true, th),    // 100 → 115: exactly +15%
		res("ExactFloor", 20, true, th),     // 20 → 30: +50% but exactly +10
		res("Accepted", 100, true, th),
		res("Gone", 100, true, th),
		res("GoneInfo", 100, false, nil),
		res("NotComparable", 100, false, th),
		res("NoThreshold", 100, true, nil),
		res("BaseThreshold", 100, true, th),
		res("Zero", 0, true, th),
		res("ZeroSame", 0, true, th),
	}}
	head := Snapshot{Version: "v0.2.0", Results: []Result{
		res("Faster", 50, true, th),
		res("Ratio", 116, true, th),
		res("RatioOnly", 25, true, th),
		res("AbsoluteOnly", 1100, true, th),
		res("ExactRatio", 115, true, th),
		res("ExactFloor", 30, true, th),
		res("Accepted", 200, true, th),
		res("NotComparable", 500, false, th),
		res("NoThreshold", 500, true, nil),
		res("BaseThreshold", 500, true, nil),
		res("Zero", 20, true, th),
		res("ZeroSame", 0, true, th),
		res("Added", 1, true, th),
	}, AcceptedRegressions: []Accepted{{Name: "Accepted", Reason: "new column renderer"}}}

	rep := compare(head, &base)
	got := map[string]string{}
	for _, r := range rep.rows {
		got[r.name] = r.status
	}
	want := map[string]string{
		"Faster": statusOK, "Ratio": statusRegression, "RatioOnly": statusOK, "AbsoluteOnly": statusOK,
		"ExactRatio": statusOK, "ExactFloor": statusOK,
		"Accepted": statusAccepted, "Gone": statusMissing, "GoneInfo": statusInfo,
		"NotComparable": statusInfo, "NoThreshold": statusInfo, "BaseThreshold": statusRegression,
		"Zero": statusRegression, "ZeroSame": statusOK, "Added": statusNew,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses:\n%v\nwant\n%v", got, want)
	}
	if rep.failures() != 4 {
		t.Errorf("failures = %d, want 4", rep.failures())
	}
	md := rep.markdown()
	for _, s := range []string{"❌ 4 material regressions", "| regression | Ratio | 100 ns | 116 ns | +16.0% | +15% and +10 ns |", "- Accepted: new column renderer"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown lacks %q:\n%s", s, md)
		}
	}
}

func TestHuman(t *testing.T) {
	cases := []struct {
		v    float64
		unit string
		want string
	}{
		{512, unitNs, "512 ns"},
		{50e3, unitNs, "50.0 µs"},
		{2588571, unitNs, "2.59 ms"},
		{512, unitBytes, "512 B"},
		{64 * kib, unitBytes, "64.0 KiB"},
		{401448091, unitBytes, "382.9 MiB"},
		{11255, unitCount, "11255"},
	}
	for _, c := range cases {
		if got := human(c.v, c.unit); got != c.want {
			t.Errorf("human(%v, %s) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
}

// TestCompareExitStatus runs the compare subcommand end to end on snapshot
// files: no base passes, a material regression exits 1, a missing head is
// an error.
func TestCompareExitStatus(t *testing.T) {
	th := &Threshold{MaxRegressionRatio: 1.15, MinAbsoluteRegression: 10}
	write := func(dir, v string, median float64) {
		t.Helper()
		s := Snapshot{SchemaVersion: schemaVersion, Version: v, Results: []Result{res("View/ns_per_op", median, true, th)}}
		if err := writeSnapshot(filepath.Join(dir, "bench-"+v+".json"), s); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	summary := filepath.Join(t.TempDir(), "summary.md")
	cmp := func(v string) (int, string) {
		var out strings.Builder
		code := cli([]string{"compare", "-version", v, "-dir", dir, "-summary", summary}, &out, io.Discard)
		return code, out.String()
	}

	write(dir, "v0.1.0", 100)
	if code, out := cmp("v0.1.0"); code != 0 || !strings.Contains(out, "First baseline") {
		t.Errorf("first baseline: exit %d\n%s", code, out)
	}
	write(dir, "v0.2.0", 105)
	if code, out := cmp("v0.2.0"); code != 0 || !strings.Contains(out, "✅ No material regressions") {
		t.Errorf("within threshold: exit %d\n%s", code, out)
	}
	write(dir, "v0.3.0", 200)
	if code, out := cmp("v0.3.0"); code != 1 || !strings.Contains(out, "Base: v0.2.0") {
		t.Errorf("regression: exit %d\n%s", code, out)
	}
	if code, _ := cmp("v0.4.0"); code != 1 {
		t.Errorf("missing head: exit %d", code)
	}
	data, _ := os.ReadFile(summary)
	if n := strings.Count(string(data), "## Release benchmark gate"); n != 3 {
		t.Errorf("summary has %d reports, want 3 (appended)", n)
	}
}
