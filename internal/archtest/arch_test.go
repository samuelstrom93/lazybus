// Package archtest guards lazybus' package dependencies. It loads the module's import
// graph with `go list` and checks the rules in the tests below. The layering they
// protect (descriptive — only the tests are enforced):
//
//	cmd/lazybus ──wires──▶ bus, bus/azure, bus/fake, recent, ui
//	ui ──────────────────▶ bus, recent      (the bus.Backend interface, never a transport)
//	bus/azure, bus/fake ─▶ bus              (the transports)
//	seed ────────────────▶ bus/azure        (emulator seeder: tools/seed and E2E tests)
//	bus, recent: leaves
//	tools/* may use anything; nothing in internal/ or cmd/ imports tools/*.
//
// `go list` via exec rather than golang.org/x/tools/go/packages: same data (go/packages
// shells out to `go list` itself), no new go.mod dependency.
//
// Only non-test imports are checked: tests (including the emulator E2E tests behind
// -tags emulator) may wire whatever they need. Importing cmd/* needs no rule: they are
// package main, which the compiler refuses to import.
// A failing rule means fix the dependency. Widening an allow-list needs a comment
// saying why, and a mention in the change's report.
package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type pkg struct {
	ImportPath string
	Imports    []string
}

var loadGraph = sync.OnceValues(load)

// graph maps module-relative package paths (e.g. "internal/ui") to their non-test
// imports. Module-internal imports are module-relative too; others keep their full
// path ("charm.land/bubbletea/v2"). Loaded once per test binary.
func graph(t *testing.T) map[string][]string {
	t.Helper()
	g, err := loadGraph()
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func load() (map[string][]string, error) {
	out, err := goList("-m", "-f", "{{.Path}} {{.Dir}}")
	if err != nil {
		return nil, err
	}
	module, dir, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if err := trackSources(dir); err != nil {
		return nil, fmt.Errorf("walk module sources: %w", err)
	}

	out, err = goList("-json=ImportPath,Imports", module+"/...")
	if err != nil {
		return nil, err
	}
	g := map[string][]string{}
	rel := func(p string) string {
		if p == module {
			return "."
		}
		return strings.TrimPrefix(p, module+"/")
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list: %w", err)
		}
		imports := make([]string, 0, len(p.Imports))
		for _, imp := range p.Imports {
			imports = append(imports, rel(imp))
		}
		g[rel(p.ImportPath)] = imports
	}
	// A rule or allow-list entry naming a package that no longer exists would pass vacuously.
	for _, want := range []string{"cmd/lazybus", busPkg, azurePkg, fakePkg, recentPkg, uiPkg, seedPkg} {
		if _, ok := g[want]; !ok {
			return nil, fmt.Errorf("package %s not found in the import graph; update the rules in arch_test.go", want)
		}
	}
	return g, nil
}

// goList runs `go list args...`; a failure carries go list's stderr.
func goList(args ...string) ([]byte, error) {
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil, fmt.Errorf("go list %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(ee.Stderr))
	} else if err != nil {
		return nil, fmt.Errorf("go list %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// trackSources stats every Go file of the module from this process. go test caches
// results keyed on the files the test binary touches, and the `go list` subprocess is
// invisible to it: without this, `go test ./...` would replay a cached PASS after
// someone adds a forbidden import.
func trackSources(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			_, err = os.Stat(path)
		}
		return err
	})
}

const (
	busPkg    = "internal/bus"
	azurePkg  = "internal/bus/azure"
	fakePkg   = "internal/bus/fake"
	recentPkg = "internal/recent"
	uiPkg     = "internal/ui"
	seedPkg   = "internal/seed"
)

func isModule(p string) bool {
	return p == "." || hasPathPrefix(p, "cmd") || hasPathPrefix(p, "internal") || hasPathPrefix(p, "tools")
}

func isProduction(p string) bool { return hasPathPrefix(p, "cmd") || hasPathPrefix(p, "internal") }

func hasPathPrefix(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// TestLeavesImportNoModulePackage: bus is the core abstraction every other package
// builds on, and recent is a self-contained store for the recent-entities list.
func TestLeavesImportNoModulePackage(t *testing.T) {
	g := graph(t)
	for _, p := range []string{busPkg, recentPkg} {
		for _, imp := range g[p] {
			if isModule(imp) {
				t.Errorf("%s imports %s, but %s is a leaf and imports no lazybus package. "+
					"Move the shared code into %s (or a new leaf), or define a small interface in %s "+
					"and let cmd/lazybus pass the implementation in.", p, imp, p, p, p)
			}
		}
	}
}

// TestUIImportsOnlyBusAndRecent: the UI talks to a bus.Backend, never to a concrete
// transport, so it runs the same against Azure, the emulator and --demo (fake).
func TestUIImportsOnlyBusAndRecent(t *testing.T) {
	allowed := []string{busPkg, recentPkg}
	for _, imp := range graph(t)[uiPkg] {
		if isModule(imp) && !slices.Contains(allowed, imp) {
			t.Errorf("%s imports %s, but the UI may only import %v from lazybus. "+
				"Add what the UI needs to the bus.Backend interface (or a type in internal/bus), "+
				"implement it in the transports, and let cmd/lazybus choose the transport.",
				uiPkg, imp, allowed)
		}
	}
}

// TestTransportsImportOnlyBus: azure and fake implement bus.Backend side by side and
// know nothing of each other, the UI or the recent list.
func TestTransportsImportOnlyBus(t *testing.T) {
	g := graph(t)
	for _, p := range []string{azurePkg, fakePkg} {
		for _, imp := range g[p] {
			if isModule(imp) && imp != busPkg {
				t.Errorf("%s imports %s, but a transport may only import %s from lazybus. "+
					"Shared transport logic belongs in %s; anything else is wired in cmd/lazybus.",
					p, imp, busPkg, busPkg)
			}
		}
	}
}

// TestOnlyEntrypointsImportTransports: choosing Azure or fake is wiring, done by
// cmd/lazybus (tools/* may wire one too). Everything else depends on the bus.Backend
// interface.
func TestOnlyEntrypointsImportTransports(t *testing.T) {
	allowed := map[string][]string{
		azurePkg: {
			"cmd/lazybus",
			// The emulator seeder reuses the transport's emulator admin client and connection-string checks.
			seedPkg,
		},
		fakePkg: {"cmd/lazybus"},
	}
	for p, imports := range graph(t) {
		if hasPathPrefix(p, "tools") {
			continue
		}
		for transport, ok := range allowed {
			if slices.Contains(imports, transport) && !slices.Contains(ok, p) {
				t.Errorf("%s imports %s. Only %v may import a concrete transport: depend on "+
					"bus.Backend from internal/bus and let cmd/lazybus pass the transport in.",
					p, transport, ok)
			}
		}
	}
}

// TestProductionDoesNotImportDevCode: tools/* and the emulator seeder are for
// development and the E2E tests only; the lazybus binary never ships them.
func TestProductionDoesNotImportDevCode(t *testing.T) {
	for p, imports := range graph(t) {
		if !isProduction(p) || p == seedPkg {
			continue
		}
		for _, imp := range imports {
			if hasPathPrefix(imp, "tools") || imp == seedPkg {
				t.Errorf("%s imports %s, which is dev/test-only code. Import it only from a _test.go "+
					"file or tools/*; if production needs the logic, move it into a real internal package.",
					p, imp)
			}
		}
	}
}

// TestAzureSDKOnlyInAzureTransport: everything Azure-specific (SDK types, errors,
// AMQP) is translated into bus types in internal/bus/azure.
func TestAzureSDKOnlyInAzureTransport(t *testing.T) {
	allowed := []string{
		azurePkg,
		// Builds the token credential (azidentity) passed to azure.New; see backend() in main.go.
		"cmd/lazybus",
		// Creates entities and dead-letters messages in the emulator, which needs the SDK directly.
		seedPkg,
	}
	for p, imports := range graph(t) {
		if slices.Contains(allowed, p) || hasPathPrefix(p, "tools") {
			continue
		}
		for _, imp := range imports {
			if hasPathPrefix(imp, "github.com/Azure") {
				t.Errorf("%s imports %s. The Azure SDK is only used in %s: add what you need to "+
					"bus.Backend (or a bus type) and implement it there, mapping SDK errors to bus errors.",
					p, imp, azurePkg)
			}
		}
	}
}

// isCharmV1TUI matches the pre-v2 import paths of the same framework.
func isCharmV1TUI(p string) bool {
	for _, m := range []string{"bubbletea", "bubbles", "lipgloss"} {
		if hasPathPrefix(p, "github.com/charmbracelet/"+m) {
			return true
		}
	}
	return false
}

// TestTUIFrameworkOnlyInUI: bubbletea, bubbles and lipgloss stay in internal/ui;
// cmd/lazybus only starts the program.
func TestTUIFrameworkOnlyInUI(t *testing.T) {
	allowed := []string{
		uiPkg,
		// Runs tea.NewProgram and reads the final model for the exit code.
		"cmd/lazybus",
	}
	for p, imports := range graph(t) {
		if slices.Contains(allowed, p) || hasPathPrefix(p, "tools") {
			continue
		}
		for _, imp := range imports {
			if hasPathPrefix(imp, "charm.land") || isCharmV1TUI(imp) {
				t.Errorf("%s imports %s. The TUI framework belongs in %s: return data or a bus type "+
					"and let the UI render it.", p, imp, uiPkg)
			}
		}
	}
}
