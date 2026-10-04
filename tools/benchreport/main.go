// Command benchreport measures the release benchmarks into a snapshot and
// compares a release's snapshot with the previous stable one. See
// docs/releasing.md.
//
//	go run ./tools/benchreport run -version v0.2.0      # writes benchmarks/release/bench-v0.2.0.json
//	go run ./tools/benchreport compare -version v0.2.0  # gate: exit 1 on a material regression
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Defaults for `run`, tuned on framen so run-to-run medians stay well
// inside the thresholds (docs/releasing.md). GOMAXPROCS is pinned (-cpu)
// so the GC-heavy benchmarks don't depend on how many cores happen to be
// idle.
const (
	defaultCount     = 15
	defaultBenchtime = "1s"
	defaultCPU       = 4
	defaultPkg       = "./internal/ui"
	defaultDir       = "benchmarks/release"
)

func main() {
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  benchreport run -version vX.Y.Z [-out path] [-count N] [-benchtime D] [-cpu N] [-pkg ./internal/ui]
  benchreport compare -version vX.Y.Z [-dir benchmarks/release] [-head path] [-base path] [-summary path] [-out path]
`

// cli runs one subcommand and returns the exit status: 0 ok, 1 failed gate
// or error, 2 usage.
func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = runCmd(args[1:], stderr)
	case "compare":
		var failed bool
		failed, err = compareCmd(args[1:], stdout, stderr)
		if err == nil && failed {
			return 1
		}
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "benchreport:", err)
		return 1
	}
	return 0
}

func runCmd(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "release version, e.g. v0.2.0 (required)")
	out := fs.String("out", "", "snapshot path (default benchmarks/release/bench-<version>.json)")
	count := fs.Int("count", defaultCount, "samples per benchmark: go test runs, one sample each")
	benchtime := fs.String("benchtime", defaultBenchtime, "go test -benchtime")
	cpu := fs.Int("cpu", defaultCPU, "GOMAXPROCS for the benchmarks (go test -cpu); 0 leaves it to go test")
	pkg := fs.String("pkg", defaultPkg, "package with the benchmarks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, ok := parseVersion(*version); !ok {
		return fmt.Errorf("-version must look like v1.2.3 (optionally v1.2.3-rc.1), got %q", *version)
	}
	if *count < 1 {
		return fmt.Errorf("-count must be at least 1")
	}
	if *out == "" {
		*out = filepath.Join(defaultDir, "bench-"+*version+".json")
	}
	snap, err := measure(measureConfig{
		version: *version, count: *count, benchtime: *benchtime, cpu: *cpu, pkg: *pkg, log: stderr, now: time.Now,
	})
	if err != nil {
		return err
	}
	// A snapshot rewritten for a retry keeps the regressions accepted in it.
	if prev, err := readSnapshot(*out); err == nil {
		snap.AcceptedRegressions = prev.AcceptedRegressions
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("existing %s: %w", *out, err)
	}
	if err := writeSnapshot(*out, snap); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "benchreport: wrote %s (%d metrics)\n", *out, len(snap.Results))
	return nil
}

func compareCmd(args []string, stdout, stderr io.Writer) (failed bool, err error) {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "release version; picks <dir>/bench-<version>.json as head (required unless -head)")
	dir := fs.String("dir", defaultDir, "directory of release snapshots")
	headPath := fs.String("head", "", "head snapshot (default <dir>/bench-<version>.json)")
	basePath := fs.String("base", "", "base snapshot (default: latest stable snapshot below head in <dir>)")
	summary := fs.String("summary", "", "append the Markdown report to this file (e.g. $GITHUB_STEP_SUMMARY)")
	out := fs.String("out", "", "write the Markdown report to this file")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if *headPath == "" {
		if _, ok := parseVersion(*version); !ok {
			return false, fmt.Errorf("-version must look like v1.2.3 (optionally v1.2.3-rc.1), got %q", *version)
		}
		*headPath = filepath.Join(*dir, "bench-"+*version+".json")
	}
	head, err := readSnapshot(*headPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%s not found: run `go run ./tools/benchreport run -version <version> -out %s` first", *headPath, *headPath)
	}
	if err != nil {
		return false, err
	}
	if *version == "" {
		*version = head.Version
	}

	var base *Snapshot
	if *basePath == "" {
		v, ok := parseVersion(*version)
		if !ok {
			return false, fmt.Errorf("head version %q is not semver", *version)
		}
		*basePath, err = selectBase(*dir, v)
		if err != nil {
			return false, err
		}
	}
	if *basePath != "" {
		b, err := readSnapshot(*basePath)
		if err != nil {
			return false, err
		}
		base = &b
	}

	rep := compare(head, base)
	md := rep.markdown()
	fmt.Fprint(stdout, md)
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return false, err
		}
	}
	if *summary != "" {
		f, err := os.OpenFile(*summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return false, err
		}
		_, werr := io.WriteString(f, md)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return false, werr
		}
	}
	return rep.failures() > 0, nil
}
