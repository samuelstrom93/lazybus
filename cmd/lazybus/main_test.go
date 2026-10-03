package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/samuelstrom93/lazybus/internal/bus/azure"
)

func TestParseFlags(t *testing.T) {
	env := func(k string) string {
		if k == "LAZYBUS_CONNECTION_STRING" {
			return "from-env"
		}
		return ""
	}
	c, err := parseFlags([]string{"--emulator", "--emulator-amqp-port", "5682", "--emulator-admin-port", "5310"}, io.Discard, env)
	if err != nil {
		t.Fatal(err)
	}
	if !c.emulator || c.emulatorAMQPPort != 5682 || c.emulatorAdminPort != 5310 || c.connectionString != "from-env" {
		t.Fatalf("config = %+v", c)
	}
	c, _ = parseFlags(nil, io.Discard, func(string) string { return "" })
	if c.emulatorAMQPPort != 5672 || c.emulatorAdminPort != 5300 {
		t.Fatalf("default ports = %d/%d", c.emulatorAMQPPort, c.emulatorAdminPort)
	}
	c, _ = parseFlags([]string{"--connection-string", "flag"}, io.Discard, env)
	if c.connectionString != "flag" {
		t.Fatalf("flag did not win over env: %q", c.connectionString)
	}
	if _, err := parseFlags([]string{"stray"}, io.Discard, env); err == nil {
		t.Fatal("stray argument accepted")
	}
}

// TestBackendWiring checks which namespaces each flag opens. Nothing here
// touches the network or runs az: clients connect lazily.
func TestBackendWiring(t *testing.T) {
	noCred := func() (azcore.TokenCredential, error) { t.Fatal("credential requested"); return nil, nil }
	if _, _, err := backend(config{}, noCred); !errors.Is(err, errNoTarget) {
		t.Fatalf("no flags: err = %v", err)
	}

	be, closeFn, err := backend(config{emulator: true, emulatorAMQPPort: 5682, emulatorAdminPort: 5310}, noCred)
	if err != nil {
		t.Fatal(err)
	}
	nss, _ := be.Namespaces(context.Background())
	closeFn()
	if len(nss) != 1 || nss[0].Name != "emulator" || nss[0].FQDN != "localhost:5682" {
		t.Fatalf("--emulator namespaces = %+v", nss)
	}

	credCalls := 0
	cred := func() (azcore.TokenCredential, error) { credCalls++; return cliCredential() }
	be, closeFn, err = backend(config{
		namespace:         "sb-prod-weu.servicebus.windows.net",
		connectionString:  azure.EmulatorConnectionString("localhost", 5682),
		emulatorAdminPort: 5310,
	}, cred)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	nss, _ = be.Namespaces(context.Background())
	if credCalls != 1 || len(nss) != 2 || nss[0].Name != "emulator" || nss[1].Name != "sb-prod-weu" {
		t.Fatalf("namespaces = %+v, credential calls %d", nss, credCalls)
	}

	if _, _, err := backend(config{connectionString: "garbage"}, noCred); err == nil {
		t.Fatal("bad connection string accepted")
	}
}
