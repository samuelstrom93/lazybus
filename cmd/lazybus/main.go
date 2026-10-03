// Command lazybus is a lazygit-style terminal UI for Azure Service Bus.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
	"github.com/samuelstrom93/lazybus/internal/ui"
)

type config struct {
	demo              bool
	connectionString  string
	namespace         string
	emulator          bool
	emulatorAMQPPort  int
	emulatorAdminPort int
	readOnly          bool
}

func parseFlags(args []string, stderr io.Writer, getenv func(string) string) (config, error) {
	var c config
	fs := flag.NewFlagSet("lazybus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&c.demo, "demo", false, "run against built-in demo data (no Azure)")
	fs.StringVar(&c.connectionString, "connection-string", "", "open one namespace by SAS connection string (or LAZYBUS_CONNECTION_STRING)")
	fs.StringVar(&c.namespace, "namespace", "", "open this namespace (FQDN, or a bare name for *.servicebus.windows.net) with the az CLI credential")
	fs.BoolVar(&c.emulator, "emulator", false, "connect to the local Service Bus emulator on localhost")
	fs.IntVar(&c.emulatorAMQPPort, "emulator-amqp-port", azure.DefaultEmulatorAMQPPort, "emulator AMQP port")
	fs.IntVar(&c.emulatorAdminPort, "emulator-admin-port", azure.DefaultEmulatorAdminPort, "emulator admin (HTTP) port")
	fs.BoolVar(&c.readOnly, "read-only", false, "disable every state-changing key")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "lazybus: unexpected argument %q\n", fs.Arg(0))
		return c, errors.New("unexpected arguments")
	}
	if c.connectionString == "" {
		c.connectionString = getenv("LAZYBUS_CONNECTION_STRING")
	}
	return c, nil
}

// backend builds the bus backend for c. newCred creates the az CLI
// credential, used for --namespace or for ARM discovery, which
// runs unless --namespace or --demo is given (spec §4, §7: discovered
// namespaces plus any --connection-string/--emulator entry). The returned
// close func releases connections.
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
	if c.emulator {
		if _, err := b.AddConnectionString(azure.EmulatorConnectionString("localhost", c.emulatorAMQPPort), c.emulatorAdminPort); err != nil {
			return fail(err)
		}
	}
	if c.connectionString != "" {
		if _, err := b.AddConnectionString(c.connectionString, c.emulatorAdminPort); err != nil {
			return fail(err)
		}
	}
	raw, err := newCred()
	if err != nil {
		return fail(fmt.Errorf("az CLI credential: %w", err))
	}
	// One token per scope shared by every client, not one az run each.
	cred := azure.CachedCredential(raw)
	if c.namespace != "" {
		if _, err := b.AddNamespace(c.namespace, cred); err != nil {
			return fail(err)
		}
	} else {
		b.EnableDiscovery(cred)
	}
	return b, closeFn, nil
}

func cliCredential() (azcore.TokenCredential, error) {
	return azidentity.NewAzureCLICredential(nil)
}

func main() {
	c, err := parseFlags(os.Args[1:], os.Stderr, os.Getenv)
	if err != nil {
		os.Exit(2)
	}
	be, closeFn, err := backend(c, cliCredential)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lazybus:", err)
		os.Exit(2)
	}
	defer closeFn()
	m := ui.New(be, ui.Options{ReadOnly: c.readOnly})
	if _, err := tea.NewProgram(m).Run(); err != nil {
		closeFn()
		fmt.Fprintln(os.Stderr, "lazybus:", err)
		os.Exit(1)
	}
}
