// Command lazybus is a lazygit-style terminal UI for Azure Service Bus.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus/fake"
	"github.com/samuelstrom93/lazybus/internal/ui"
)

type config struct {
	demo             bool
	connectionString string
	namespace        string
	emulator         bool
	readOnly         bool
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("lazybus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&c.demo, "demo", false, "run against built-in demo data (no Azure)")
	fs.StringVar(&c.connectionString, "connection-string", "", "open one namespace by SAS connection string (or LAZYBUS_CONNECTION_STRING)")
	fs.StringVar(&c.namespace, "namespace", "", "open this namespace FQDN with the az CLI credential")
	fs.BoolVar(&c.emulator, "emulator", false, "connect to the local Service Bus emulator")
	fs.BoolVar(&c.readOnly, "read-only", false, "disable every state-changing key")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.connectionString == "" {
		c.connectionString = os.Getenv("LAZYBUS_CONNECTION_STRING")
	}
	return c, nil
}

func main() {
	c, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	if !c.demo {
		fmt.Fprintln(os.Stderr, "lazybus: connecting to Azure is not implemented in this build; try --demo")
		os.Exit(1)
	}
	m := ui.New(fake.New(), ui.Options{ReadOnly: c.readOnly})
	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "lazybus:", err)
		os.Exit(1)
	}
}
