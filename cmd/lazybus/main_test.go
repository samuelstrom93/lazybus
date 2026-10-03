package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/samuelstrom93/lazybus/internal/bus/azure"
)

func TestParseFlags(t *testing.T) {
	env := func(k string) string {
		if k == "LAZYBUS_CONNECTION_STRING" {
			return "from-env"
		}
		return ""
	}
	c, err := parseFlags([]string{"--emulator", "--emulator-amqp-port", "5682", "--emulator-admin-port", "5310"}, io.Discard, io.Discard, env)
	if err != nil {
		t.Fatal(err)
	}
	if !c.emulator || c.emulatorAMQPPort != 5682 || c.emulatorAdminPort != 5310 || c.connectionString != "from-env" {
		t.Fatalf("config = %+v", c)
	}
	c, _ = parseFlags(nil, io.Discard, io.Discard, func(string) string { return "" })
	if c.emulatorAMQPPort != 5672 || c.emulatorAdminPort != 5300 {
		t.Fatalf("default ports = %d/%d", c.emulatorAMQPPort, c.emulatorAdminPort)
	}
	c, _ = parseFlags([]string{"--connection-string", "flag"}, io.Discard, io.Discard, env)
	if c.connectionString != "flag" {
		t.Fatalf("flag did not win over env: %q", c.connectionString)
	}
	if _, err := parseFlags([]string{"stray"}, io.Discard, io.Discard, env); err == nil {
		t.Fatal("stray argument accepted")
	}
}

func TestHelpAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	_, err := parseFlags([]string{"--help"}, &out, &errOut, func(string) string { return "" })
	if !errors.Is(err, flag.ErrHelp) || errOut.Len() != 0 {
		t.Fatalf("--help: err %v, stderr %q", err, errOut.String())
	}
	for _, want := range []string{"Usage:", "-read-only", "-version", "--demo", "press ? for the keybindings"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if _, err := parseFlags([]string{"--bogus"}, &out, &errOut, func(string) string { return "" }); err == nil ||
		out.Len() != 0 || !strings.Contains(errOut.String(), "flag provided but not defined") {
		t.Fatalf("unknown flag: err %v, stdout %q, stderr %q", err, out.String(), errOut.String())
	}
	c, err := parseFlags([]string{"--version"}, io.Discard, io.Discard, func(string) string { return "" })
	if err != nil || !c.version {
		t.Fatalf("--version: %+v, %v", c, err)
	}

	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
	version, commit, date = "0.1.0", "5f83d80abcdef0123456789", "2026-10-03T12:00:00Z"
	if got, want := versionString(), "lazybus 0.1.0 (5f83d80abcde, 2026-10-03T12:00:00Z)"; got != want {
		t.Fatalf("versionString = %q, want %q", got, want)
	}
	version, commit, date = "", "", ""
	if got := versionString(); !strings.HasPrefix(got, "lazybus ") {
		t.Fatalf("versionString without ldflags = %q", got)
	}
}

// TestBackendWiring checks which namespaces each flag opens and when ARM
// discovery runs. Nothing here touches the network or runs az: clients
// connect lazily, and the test credential fails before any request.
func TestBackendWiring(t *testing.T) {
	ctx := context.Background()
	noCred := func() (azcore.TokenCredential, error) { t.Fatal("credential requested"); return nil, nil }
	credCalls := 0
	cred := func() (azcore.TokenCredential, error) { credCalls++; return failingCred{}, nil }

	// --demo: the fake backend, with demo discovery.
	be, closeFn, err := backend(config{demo: true}, noCred)
	if err != nil {
		t.Fatal(err)
	}
	closeFn()
	if subs, _ := be.Subscriptions(ctx); len(subs) == 0 {
		t.Fatal("--demo: no demo subscriptions")
	}

	// No flags: discovery only.
	be, closeFn, err = backend(config{}, cred)
	if err != nil {
		t.Fatal(err)
	}
	nss, _ := be.Namespaces(ctx)
	_, subErr := be.Subscriptions(ctx)
	closeFn()
	if credCalls != 1 || len(nss) != 0 || !errors.Is(subErr, errNoToken) {
		t.Fatalf("no flags: namespaces %+v, subscriptions err %v, credential calls %d", nss, subErr, credCalls)
	}

	// --emulator: the emulator plus discovery.
	be, closeFn, err = backend(config{emulator: true, emulatorAMQPPort: 5682, emulatorAdminPort: 5310}, cred)
	if err != nil {
		t.Fatal(err)
	}
	nss, _ = be.Namespaces(ctx)
	_, subErr = be.Subscriptions(ctx)
	closeFn()
	if credCalls != 2 || len(nss) != 1 || nss[0].Name != "emulator" || nss[0].FQDN != "localhost:5682" || subErr == nil {
		t.Fatalf("--emulator: namespaces %+v, subscriptions err %v", nss, subErr)
	}

	// --namespace skips discovery.
	credCalls = 0
	be, closeFn, err = backend(config{
		namespace:         "sb-prod-weu.servicebus.windows.net",
		connectionString:  azure.EmulatorConnectionString("localhost", 5682),
		emulatorAdminPort: 5310,
	}, cred)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	nss, _ = be.Namespaces(ctx)
	subs, subErr := be.Subscriptions(ctx)
	if credCalls != 1 || len(nss) != 2 || nss[0].Name != "emulator" || nss[1].Name != "sb-prod-weu" {
		t.Fatalf("namespaces = %+v, credential calls %d", nss, credCalls)
	}
	if len(subs) != 0 || subErr != nil {
		t.Fatalf("--namespace ran discovery: %+v, %v", subs, subErr)
	}

	if _, _, err := backend(config{connectionString: "garbage"}, cred); err == nil {
		t.Fatal("bad connection string accepted")
	}
	noAz := func() (azcore.TokenCredential, error) { return nil, errNoToken }
	if _, _, err := backend(config{}, noAz); !errors.Is(err, errNoToken) {
		t.Fatalf("credential error = %v", err)
	}
	// The emulator still opens without the az credential, minus discovery.
	be, closeFn, err = backend(config{emulator: true, emulatorAMQPPort: 5682, emulatorAdminPort: 5310}, noAz)
	if err != nil {
		t.Fatalf("--emulator without az: %v", err)
	}
	defer closeFn()
	if subs, err := be.Subscriptions(ctx); len(subs) != 0 || err != nil {
		t.Fatalf("--emulator without az ran discovery: %v, %v", subs, err)
	}
}

var errNoToken = errors.New("no token in tests")

// failingCred fails every token request, so no request leaves the host.
type failingCred struct{}

func (failingCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errNoToken
}
