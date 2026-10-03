// Command seed fills the local Service Bus emulator with dead-lettered
// messages: JSON and non-JSON bodies with typed application properties in
// the DLQs of queue orders and subscription order-events/billing, and
// creates the topic empty-topic (no subscriptions) if it is missing.
//
//	go run ./tools/seed                  # lazybus emulator on 5682 / 5310
//	go run ./tools/seed -reset=false     # append instead of replacing
//
// Emulator only: a connection string without UseDevelopmentEmulator=true is
// refused.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/seed"
)

func main() {
	cs := azure.EmulatorConnectionString("localhost", 5682)
	if env := os.Getenv("LAZYBUS_CONNECTION_STRING"); azure.IsEmulatorConnectionString(env) {
		cs = env
	}
	flag.StringVar(&cs, "connection-string", cs, "emulator AMQP connection string (default: LAZYBUS_CONNECTION_STRING if it is an emulator one, else localhost:5682)")
	adminPort := flag.Int("admin-port", 5310, "emulator admin (HTTP) port")
	reset := flag.Bool("reset", true, "drain the seeded DLQs first, so a rerun ends in the same state")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall deadline")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := seed.Run(ctx, seed.Config{ConnectionString: cs, AdminPort: *adminPort, Reset: *reset, Log: os.Stdout}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
