// Command lazybus is a lazygit-style terminal UI for Azure Service Bus.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
	"github.com/samuelstrom93/lazybus/internal/ui"
)

// Set by the release build (goreleaser ldflags -X); a plain go build or go
// install leaves them empty and versionString falls back to the module and
// VCS info the Go toolchain embeds.
var (
	version = ""
	commit  = ""
	date    = ""
)

type config struct {
	version           bool
	demo              bool
	connectionString  string
	namespace         string
	emulator          bool
	emulatorAMQPPort  int
	emulatorAdminPort int
	readOnly          bool
	// entities (--entity) replace the entity listing of the one namespace
	// opened; nil lists them.
	entities []bus.Entity
}

const usageHead = `lazybus: a terminal UI for Azure Service Bus dead-letter queues.
Browse, peek, edit and resubmit dead-lettered messages.

Usage:
  lazybus [flags]

Without --namespace, --connection-string, --emulator or --demo, lazybus
discovers your namespaces from az login. With --entity it opens only the
entities named, without listing them (listing needs Manage rights) and
without discovery.

Examples:
  lazybus                                   discover namespaces from az login
  lazybus --namespace sb-prod-weu           one namespace, az login credential, no discovery
  lazybus --connection-string "$CS"         the namespace of a SAS connection string
  lazybus --emulator                        the local Service Bus emulator (ports 5672 / 5300)
  lazybus --demo                            built-in demo data, no Azure
  lazybus --read-only --namespace sb-prod   browse and peek only: r and c are off
  lazybus --connection-string "$CS" --entity orders --entity order-events/billing
                                            only these entities: works without Manage rights

Flags:
`

const usageTail = `
In lazybus, press ? for the keybindings and q to quit.
Pending Edits live in memory only: they are lost when lazybus quits.
`

func parseFlags(args []string, stdout, stderr io.Writer, getenv func(string) string) (config, error) {
	var c config
	fs := flag.NewFlagSet("lazybus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // printed below: --help to stdout, errors to stderr
	fs.BoolVar(&c.version, "version", false, "print the version and exit")
	fs.BoolVar(&c.demo, "demo", false, "run against built-in demo data (no Azure)")
	fs.StringVar(&c.connectionString, "connection-string", "", "open the namespace of a SAS connection string (or LAZYBUS_CONNECTION_STRING)")
	fs.StringVar(&c.namespace, "namespace", "", "open this namespace (FQDN, or a bare name for *.servicebus.windows.net) with the az CLI credential; skips discovery")
	fs.BoolVar(&c.emulator, "emulator", false, "open the local Service Bus emulator on localhost")
	fs.IntVar(&c.emulatorAMQPPort, "emulator-amqp-port", azure.DefaultEmulatorAMQPPort, "emulator AMQP port")
	fs.IntVar(&c.emulatorAdminPort, "emulator-admin-port", azure.DefaultEmulatorAdminPort, "emulator admin (HTTP) port")
	fs.BoolVar(&c.readOnly, "read-only", false, "disable every state-changing key (r resubmit, c finish cleanup)")
	fs.Func("entity", "open entity `path` (queue or topic/subscription) instead of listing the entities, which needs Manage rights; repeatable; needs exactly one of --connection-string, --namespace or --emulator", func(s string) error {
		e, err := bus.ParseEntity(s)
		if err == nil {
			c.entities = append(c.entities, e)
		}
		return err
	})
	usage := func(w io.Writer) {
		fmt.Fprint(w, usageHead)
		fs.SetOutput(w)
		fs.PrintDefaults()
		fs.SetOutput(stderr)
		fmt.Fprint(w, usageTail)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
		} else {
			fmt.Fprintln(stderr, "see lazybus --help")
		}
		return c, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "lazybus: unexpected argument %q (see lazybus --help)\n", fs.Arg(0))
		return c, errors.New("unexpected arguments")
	}
	if c.connectionString == "" {
		c.connectionString = getenv("LAZYBUS_CONNECTION_STRING")
	}
	if len(c.entities) > 0 {
		opened := 0
		for _, set := range []bool{c.connectionString != "", c.namespace != "", c.emulator} {
			if set {
				opened++
			}
		}
		var msg string
		switch {
		case opened == 0:
			msg = "--entity needs --connection-string, --namespace or --emulator"
		case opened > 1:
			msg = "--entity needs exactly one of --connection-string (or LAZYBUS_CONNECTION_STRING), --namespace or --emulator"
		}
		if msg != "" {
			fmt.Fprintf(stderr, "lazybus: %s (see lazybus --help)\n", msg)
			return c, errors.New(msg)
		}
	}
	return c, nil
}

// versionString is "lazybus <version> (<commit>, <date>)": the release
// build's ldflags, else what the Go toolchain embedded (the module version
// for go install …@version, VCS info for a build in a git checkout).
func versionString() string {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			}
		}
	}
	if v == "" {
		v = "dev"
	}
	var meta []string
	if c != "" {
		meta = append(meta, c[:min(len(c), 12)])
	}
	if d != "" {
		meta = append(meta, d)
	}
	if len(meta) == 0 {
		return "lazybus " + v
	}
	return "lazybus " + v + " (" + strings.Join(meta, ", ") + ")"
}

// backend builds the bus backend for c. newCred creates the az CLI
// credential, used for --namespace or for ARM discovery, which
// runs unless --namespace, --entity or --demo is given (spec §4, §7:
// discovered namespaces plus any --connection-string/--emulator entry).
// With --entity, the one namespace opened gets c.entities instead of a
// listing. The returned close func releases connections.
func backend(c config, newCred func() (azcore.TokenCredential, error)) (bus.Backend, func(), error) {
	if c.demo {
		return fake.New(fake.WithDiscovery()), func() {}, nil
	}
	b := azure.New()
	closeFn := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.Close(ctx)
	}
	fail := func(err error) (bus.Backend, func(), error) {
		closeFn()
		return nil, nil, err
	}
	// opened is the namespace --entity applies to (parseFlags allows one).
	var opened bus.Namespace
	if c.emulator {
		ns, err := b.AddConnectionString(azure.EmulatorConnectionString("localhost", c.emulatorAMQPPort), c.emulatorAdminPort)
		if err != nil {
			return fail(err)
		}
		opened = ns
	}
	if c.connectionString != "" {
		ns, err := b.AddConnectionString(c.connectionString, c.emulatorAdminPort)
		if err != nil {
			return fail(err)
		}
		opened = ns
	}
	useEntities := func() (bus.Backend, func(), error) {
		if err := b.UseEntities(opened, c.entities); err != nil {
			return fail(err)
		}
		return b, closeFn, nil
	}
	if len(c.entities) > 0 && c.namespace == "" {
		// No discovery, so no az credential needed.
		return useEntities()
	}
	raw, err := newCred()
	if err != nil {
		// Without the credential there is no discovery, but a namespace
		// from --connection-string or --emulator still opens.
		if c.namespace == "" && (c.emulator || c.connectionString != "") {
			return b, closeFn, nil
		}
		return fail(fmt.Errorf("az CLI credential: %w", err))
	}
	// One token per scope shared by every client, not one az run each.
	cred := azure.CachedCredential(raw)
	if c.namespace != "" {
		ns, err := b.AddNamespace(c.namespace, cred)
		if err != nil {
			return fail(err)
		}
		opened = ns
	} else {
		b.EnableDiscovery(cred)
	}
	if len(c.entities) > 0 {
		return useEntities()
	}
	return b, closeFn, nil
}

func cliCredential() (azcore.TokenCredential, error) {
	return azidentity.NewAzureCLICredential(nil)
}

func main() {
	c, err := parseFlags(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		os.Exit(2)
	}
	if c.version {
		fmt.Println(versionString())
		return
	}
	be, closeFn, err := backend(c, cliCredential)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lazybus:", err)
		os.Exit(2)
	}
	defer closeFn()
	m := ui.New(be, ui.Options{ReadOnly: c.readOnly})
	final, err := tea.NewProgram(m, tea.WithFilter(ui.Filter)).Run()
	if code := exitCode(final, err, os.Stderr); code != 0 {
		closeFn()
		os.Exit(code)
	}
}

// exitCode reports how the program ended, after the terminal is restored:
// the outcome of a call a signal's quit waited for (ui.Filter), which the
// alt screen no longer shows, and the program's error. 1 when either
// failed.
func exitCode(final tea.Model, runErr error, stderr io.Writer) int {
	code := 0
	if m, ok := final.(ui.Model); ok {
		if text, failed := m.ExitReport(); text != "" {
			fmt.Fprintln(stderr, text)
			if failed {
				code = 1
			}
		}
	}
	if runErr != nil {
		fmt.Fprintln(stderr, "lazybus:", runErr)
		code = 1
	}
	return code
}
