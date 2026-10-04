package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type measureConfig struct {
	version   string
	count     int
	benchtime string
	cpu       int // go test -cpu; 0: not passed
	pkg       string
	log       io.Writer // progress: go test output, warnings
	now       func() time.Time
}

// measure runs the benchmarks and builds the binary, and returns the
// snapshot (without acceptedRegressions).
func measure(c measureConfig) (Snapshot, error) {
	sha, err := gitSHA(c.log)
	if err != nil {
		return Snapshot{}, err
	}
	rt, err := goRuntime()
	if err != nil {
		return Snapshot{}, err
	}

	// One sample per go test run, repeated count times, rather than
	// -count N: each benchmark's samples then spread over the whole run, so
	// a burst of other load on the machine skews one sample of each
	// benchmark instead of every sample of one.
	args := []string{"test", "-run", "^$", "-bench", ".", "-benchmem",
		"-count", "1", "-benchtime", c.benchtime}
	if c.cpu > 0 {
		args = append(args, "-cpu", strconv.Itoa(c.cpu))
	}
	args = append(args, c.pkg)
	var out bytes.Buffer
	for i := range c.count {
		fmt.Fprintf(c.log, "benchreport: run %d of %d\n", i+1, c.count)
		cmd := exec.Command("go", args...)
		cmd.Stdout = io.MultiWriter(&out, c.log)
		cmd.Stderr = c.log
		if err := cmd.Run(); err != nil {
			return Snapshot{}, fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
		}
	}
	parsed, err := parseBench(&out)
	if err != nil {
		return Snapshot{}, err
	}
	rt.CPU = parsed.cpu

	size, err := binarySize()
	if err != nil {
		return Snapshot{}, err
	}

	var results []Result
	for _, m := range parsed.metrics {
		results = append(results, newResult(m.name, m.unit, m.samples))
	}
	// One sample: the build is deterministic, so repeating it adds nothing.
	results = append(results, newResult(binaryMetric, unitBytes, []float64{float64(size)}))
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, r := range results {
		if r.Comparable && r.Threshold == nil {
			fmt.Fprintf(c.log, "benchreport: warning: no threshold for %s, it is reported but never gated (tools/benchreport/thresholds.go)\n", r.Name)
		}
	}

	return Snapshot{
		SchemaVersion:       schemaVersion,
		Version:             c.version,
		GitSHA:              sha,
		GeneratedAt:         c.now().UTC().Format(time.RFC3339),
		Runtime:             rt,
		BenchCommand:        shellJoin(append([]string{"go"}, args...)),
		SamplesPerBenchmark: c.count,
		Results:             results,
		AcceptedRegressions: []Accepted{},
	}, nil
}

type metric struct {
	name    string
	unit    string
	samples []float64
}

type benchOutput struct {
	cpu     string
	metrics []metric // in order of first appearance
}

// procSuffix is the -GOMAXPROCS suffix go test appends to benchmark names.
var procSuffix = regexp.MustCompile(`-\d+$`)

// parseBench reads `go test -bench` output. A result line is
//
//	BenchmarkName-32  	  462	  2588571 ns/op	 3869800 heap-bytes	  483656 B/op	  11255 allocs/op
//
// and becomes metrics Name/ns_per_op, Name/heap_bytes, Name/bytes_per_op,
// Name/allocs_per_op, one sample per line.
func parseBench(r io.Reader) (benchOutput, error) {
	var out benchOutput
	index := map[string]int{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if cpu, ok := strings.CutPrefix(line, "cpu:"); ok {
			out.cpu = strings.TrimSpace(cpu)
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || len(f)%2 != 0 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		if _, err := strconv.Atoi(f[1]); err != nil {
			continue
		}
		bench := procSuffix.ReplaceAllString(strings.TrimPrefix(f[0], "Benchmark"), "")
		for i := 2; i < len(f); i += 2 {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return out, fmt.Errorf("bad value %q in %q", f[i], line)
			}
			name, unit := metricName(bench, f[i+1])
			j, ok := index[name]
			if !ok {
				j = len(out.metrics)
				index[name] = j
				out.metrics = append(out.metrics, metric{name: name, unit: unit})
			}
			out.metrics[j].samples = append(out.metrics[j].samples, v)
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	if len(out.metrics) == 0 {
		return out, fmt.Errorf("no benchmark results in the go test output")
	}
	return out, nil
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// metricName maps a go test unit to the metric name and its unit. An
// unknown custom unit keeps its own name and is not comparable.
func metricName(bench, goUnit string) (name, unit string) {
	switch goUnit {
	case "ns/op":
		return bench + "/ns_per_op", unitNs
	case "B/op":
		return bench + "/bytes_per_op", unitBytes
	case "allocs/op":
		return bench + "/allocs_per_op", unitCount
	}
	suffix := strings.Trim(nonWord.ReplaceAllString(strings.ToLower(goUnit), "_"), "_")
	if strings.HasSuffix(goUnit, "bytes") {
		return bench + "/" + suffix, unitBytes
	}
	return bench + "/" + suffix, goUnit
}

// binarySize builds the release-shaped binary and returns its size.
func binarySize() (int64, error) {
	dir, err := os.MkdirTemp("", "benchreport-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "lazybus")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", bin, "./cmd/lazybus")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("go build ./cmd/lazybus: %w\n%s", err, out)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// gitSHA is HEAD, with -dirty appended when the tree has changes.
func gitSHA(log io.Writer) (string, error) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	if len(bytes.TrimSpace(status)) > 0 {
		fmt.Fprintln(log, "benchreport: warning: the working tree has uncommitted changes; gitSha is marked -dirty")
		sha += "-dirty"
	}
	return sha, nil
}

func goRuntime() (Runtime, error) {
	out, err := exec.Command("go", "env", "GOVERSION", "GOOS", "GOARCH").Output()
	if err != nil {
		return Runtime{}, fmt.Errorf("go env: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 3 {
		return Runtime{}, fmt.Errorf("go env: unexpected output %q", out)
	}
	return Runtime{GoVersion: f[0], GOOS: f[1], GOARCH: f[2]}, nil
}

// shellJoin quotes args that a POSIX shell would otherwise expand.
func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t$^*?'\"\\|&;<>()[]{}~#!") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}
