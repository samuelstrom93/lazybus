package main

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Row statuses. regression and missing fail the gate.
const (
	statusOK         = "ok"
	statusRegression = "regression"
	statusAccepted   = "accepted"
	statusMissing    = "missing"
	statusNew        = "new"
	statusInfo       = "info"
)

type row struct {
	name       string
	unit       string
	base, head *float64 // medians; nil when absent
	threshold  *Threshold
	status     string
	reason     string // for accepted
}

type report struct {
	head Snapshot
	base *Snapshot
	rows []row
}

func (r report) failures() int {
	n := 0
	for _, row := range r.rows {
		if row.status == statusRegression || row.status == statusMissing {
			n++
		}
	}
	return n
}

// compare compares head with base (nil: first baseline) median by median.
func compare(head Snapshot, base *Snapshot) report {
	rep := report{head: head, base: base}
	if base == nil {
		for _, h := range head.Results {
			rep.rows = append(rep.rows, row{name: h.Name, unit: h.Unit, head: &h.Median, threshold: h.Threshold, status: statusInfo})
		}
		return rep
	}

	heads, bases := byName(head.Results), byName(base.Results)
	accepted := map[string]string{}
	for _, a := range head.AcceptedRegressions {
		accepted[a.Name] = a.Reason
	}
	var names []string
	for n := range heads {
		names = append(names, n)
	}
	for n := range bases {
		if _, ok := heads[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	for _, n := range names {
		h, inHead := heads[n]
		b, inBase := bases[n]
		r := row{name: n}
		switch {
		case !inBase:
			r.unit, r.head, r.threshold, r.status = h.Unit, &h.Median, h.Threshold, statusNew
		case !inHead:
			r.unit, r.base, r.threshold, r.status = b.Unit, &b.Median, b.Threshold, statusInfo
			if b.Comparable {
				r.status = statusMissing
			}
		default:
			r.unit, r.base, r.head, r.threshold = h.Unit, &b.Median, &h.Median, h.Threshold
			if r.threshold == nil {
				r.threshold = b.Threshold
			}
			switch {
			case !h.Comparable || r.threshold == nil:
				r.status = statusInfo
			case regressed(b.Median, h.Median, *r.threshold):
				r.status = statusRegression
				if reason, ok := accepted[n]; ok {
					r.status, r.reason = statusAccepted, reason
				}
			default:
				r.status = statusOK
			}
		}
		rep.rows = append(rep.rows, r)
	}
	return rep
}

// regressed: head grew by more than the absolute floor AND by more than
// the ratio. Growth of exactly the floor or exactly the ratio passes.
func regressed(base, head float64, t Threshold) bool {
	delta := head - base
	if delta <= 0 || delta <= t.MinAbsoluteRegression {
		return false
	}
	if base == 0 {
		return true
	}
	return head/base > t.MaxRegressionRatio
}

func byName(rs []Result) map[string]*Result {
	m := make(map[string]*Result, len(rs))
	for i := range rs {
		m[rs[i].Name] = &rs[i]
	}
	return m
}

func (r report) markdown() string {
	var b strings.Builder
	b.WriteString("## Release benchmark gate\n\n")
	if r.base == nil {
		b.WriteString("✅ First baseline — nothing to compare against.\n\n")
		fmt.Fprintf(&b, "Head: %s (%s)\n\n", r.head.Version, r.head.GitSHA)
		b.WriteString("| Metric | Head median | Threshold |\n|---|---:|---|\n")
		for _, row := range r.rows {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", row.name, human(*row.head, row.unit), thresholdText(row.threshold, row.unit))
		}
		return b.String()
	}

	if n := r.failures(); n > 0 {
		fmt.Fprintf(&b, "❌ %d material %s\n\n", n, plural(n, "regression", "regressions"))
	} else {
		b.WriteString("✅ No material regressions\n\n")
	}
	fmt.Fprintf(&b, "Base: %s (%s)  \nHead: %s (%s)\n\n", r.base.Version, r.base.GitSHA, r.head.Version, r.head.GitSHA)
	if r.base.Runtime != r.head.Runtime {
		fmt.Fprintf(&b, "> **Warning:** base and head were measured on different runtimes, timings may not be comparable.  \n> Base: %s  \n> Head: %s\n\n",
			runtimeText(r.base.Runtime), runtimeText(r.head.Runtime))
	}
	b.WriteString("| Status | Metric | Base median | Head median | Δ | Threshold |\n|---|---|---:|---:|---:|---|\n")
	var accepted []row
	for _, row := range r.rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			row.status, row.name, medianText(row.base, row.unit), medianText(row.head, row.unit),
			deltaText(row.base, row.head), thresholdText(row.threshold, row.unit))
		if row.status == statusAccepted {
			accepted = append(accepted, row)
		}
	}
	if len(accepted) > 0 {
		b.WriteString("\nAccepted regressions:\n\n")
		for _, row := range accepted {
			fmt.Fprintf(&b, "- %s: %s\n", row.name, row.reason)
		}
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func runtimeText(rt Runtime) string {
	return fmt.Sprintf("%s %s/%s, %s", rt.GoVersion, rt.GOOS, rt.GOARCH, rt.CPU)
}

func medianText(v *float64, unit string) string {
	if v == nil {
		return "—"
	}
	return human(*v, unit)
}

func deltaText(base, head *float64) string {
	switch {
	case base == nil || head == nil:
		return "—"
	case *base == 0 && *head == 0:
		return "+0.0%"
	case *base == 0:
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", (*head-*base) / *base * 100)
}

func thresholdText(t *Threshold, unit string) string {
	if t == nil {
		return "—"
	}
	pct := strconv.FormatFloat(math.Round((t.MaxRegressionRatio-1)*1000)/10, 'f', -1, 64)
	return fmt.Sprintf("+%s%% and +%s", pct, human(t.MinAbsoluteRegression, unit))
}

// human formats v in unit: ns as ns/µs/ms/s, bytes as B/KiB/MiB/GiB.
func human(v float64, unit string) string {
	switch unit {
	case unitNs:
		switch a := math.Abs(v); {
		case a < 1e3:
			return fmt.Sprintf("%.0f ns", v)
		case a < 1e6:
			return fmt.Sprintf("%.1f µs", v/1e3)
		case a < 1e9:
			return fmt.Sprintf("%.2f ms", v/1e6)
		default:
			return fmt.Sprintf("%.2f s", v/1e9)
		}
	case unitBytes:
		switch a := math.Abs(v); {
		case a < kib:
			return fmt.Sprintf("%.0f B", v)
		case a < mib:
			return fmt.Sprintf("%.1f KiB", v/kib)
		case a < 1024*mib:
			return fmt.Sprintf("%.1f MiB", v/mib)
		default:
			return fmt.Sprintf("%.2f GiB", v/(1024*mib))
		}
	case unitCount:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64) + " " + unit
}

// semverRE is a release version; group 4 is the prerelease.
var semverRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

type version struct {
	major, minor, patch int
	pre                 string
}

func parseVersion(s string) (version, bool) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}
	var v version
	var err1, err2, err3 error
	v.major, err1 = strconv.Atoi(m[1])
	v.minor, err2 = strconv.Atoi(m[2])
	v.patch, err3 = strconv.Atoi(m[3])
	v.pre = m[4]
	return v, err1 == nil && err2 == nil && err3 == nil
}

// less orders versions by precedence as far as picking a base needs: a
// prerelease is below its stable version. Two prereleases of one version
// are not ordered: selectBase only ever compares with stable versions.
func (v version) less(w version) bool {
	if v.major != w.major {
		return v.major < w.major
	}
	if v.minor != w.minor {
		return v.minor < w.minor
	}
	if v.patch != w.patch {
		return v.patch < w.patch
	}
	return v.pre != "" && w.pre == ""
}

// selectBase returns the snapshot in dir of the latest stable version below
// head, or "" when there is none.
func selectBase(dir string, head version) (string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "bench-v*.json"))
	if err != nil {
		return "", err
	}
	best, bestPath := version{}, ""
	for _, p := range paths {
		v, ok := parseVersion(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "bench-"), ".json"))
		if !ok || v.pre != "" || !v.less(head) {
			continue
		}
		if bestPath == "" || best.less(v) {
			best, bestPath = v, p
		}
	}
	return bestPath, nil
}
