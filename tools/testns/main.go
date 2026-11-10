// Command testns switches the standing Service Bus test namespace between
// Basic (the default: queues only, no base fee) and Standard (topics,
// sessions, duplicate detection, forwarding; billed per hour). It is an
// optional backup path for bigger integration work and is never run by
// go test, CI, the hooks or the release script. Use it through
// scripts/test-namespace.sh:
//
//	./scripts/test-namespace.sh status
//	./scripts/test-namespace.sh up   [--dry-run] [--yes] [--from <backup.json>]
//	./scripts/test-namespace.sh down [--dry-run] [--yes]
//
// up changes the SKU to Standard, creates the demo entities that need it
// (topic lazybus-events with subscription lazybus-billing, session queue
// lazybus-sessions) when they are missing, and restores their messages
// from a backup file (default: the newest in the backup directory), only
// into entities it just created or that hold no message.
//
// down backs up the messages of the demo entities, deletes the
// Standard-only entities (the list is in plan.go), caps the default TTL of
// every queue at P14D and changes the SKU to Basic. Nothing is deleted
// when the backup fails.
//
// Both are idempotent and print their plan first; they ask before changing
// anything unless --yes is given. The lazybus-e2e-* entities belong to the
// E2E test: down deletes the Standard-only ones, nothing creates or
// restores them here.
//
// Backups are JSON files (mode 0600) in $XDG_STATE_HOME/lazybus/testns,
// by default ~/.local/state/lazybus/testns. The format and what it loses
// are described on Message in backup.go.
//
// Environment (all optional):
//
//	LAZYBUS_TESTNS_NAMESPACE       default sb-lazybus-test-81abd072
//	LAZYBUS_TESTNS_SUBSCRIPTION    default lazybus-dev
//	LAZYBUS_TESTNS_RESOURCE_GROUP  default rg-lazybus-test
//
// Needs the az CLI, logged in, with rights to update the namespace (ARM)
// and Azure Service Bus Data Owner on it.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `usage: testns status
       testns up   [--dry-run] [--yes] [--from <backup.json>]
       testns down [--dry-run] [--yes]`

type options struct {
	dryRun, yes bool
	from        string
	timeout     time.Duration
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	var opt options
	fs := flag.NewFlagSet("testns "+cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	fs.DurationVar(&opt.timeout, "timeout", 20*time.Minute, "overall deadline")
	if cmd == "up" || cmd == "down" {
		fs.BoolVar(&opt.dryRun, "dry-run", false, "print the plan and change nothing")
		fs.BoolVar(&opt.yes, "yes", false, "do not ask for confirmation")
	}
	if cmd == "up" {
		fs.StringVar(&opt.from, "from", "", "backup file to restore from (default: the newest in the backup directory)")
	}
	if cmd != "status" && cmd != "up" && cmd != "down" {
		fs.Usage()
		os.Exit(2)
	}
	_ = fs.Parse(os.Args[2:])
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "testns: unexpected argument %q\n", fs.Arg(0))
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()
	if err := run(ctx, cmd, opt); err != nil {
		fmt.Fprintln(os.Stderr, "testns:", err)
		os.Exit(1)
	}
}

func configFromEnv(getenv func(string) string) config {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return config{
		Namespace:     strings.TrimSuffix(or("LAZYBUS_TESTNS_NAMESPACE", "sb-lazybus-test-81abd072"), ".servicebus.windows.net"),
		Subscription:  or("LAZYBUS_TESTNS_SUBSCRIPTION", "lazybus-dev"),
		ResourceGroup: or("LAZYBUS_TESTNS_RESOURCE_GROUP", "rg-lazybus-test"),
	}
}

type tool struct {
	*ops
	dir string // backup directory
	opt options
	out io.Writer
	in  io.Reader
}

func run(ctx context.Context, cmd string, opt options) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	o, err := newOps(configFromEnv(os.Getenv), os.Stdout)
	if err != nil {
		return err
	}
	defer o.close()
	t := &tool{ops: o, dir: backupDir(os.Getenv, home), opt: opt, out: os.Stdout, in: os.Stdin}
	switch cmd {
	case "up":
		return t.up(ctx)
	case "down":
		return t.down(ctx)
	}
	return t.status(ctx)
}

// proceed reports whether to carry out a printed plan.
func (t *tool) proceed() (bool, error) {
	if t.opt.dryRun {
		fmt.Fprintln(t.out, "dry run: nothing changed")
		return false, nil
	}
	if t.opt.yes {
		return true, nil
	}
	fmt.Fprint(t.out, "Proceed? [y/N] ")
	line, err := bufio.NewReader(t.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		fmt.Fprintln(t.out, "aborted: nothing changed")
		return false, nil
	}
	return true, nil
}

func (t *tool) status(ctx context.Context) error {
	st, a, err := t.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(t.out, "Namespace  %s (subscription %s, resource group %s)\n", t.cfg.Namespace, t.cfg.Subscription, t.cfg.ResourceGroup)
	fmt.Fprintf(t.out, "Tier       %s (provisioning %s, status %s)\n", a.SKU, a.Provisioning, a.Status)
	fmt.Fprintf(t.out, "Queues     %d\n", len(st.Queues))
	for _, q := range st.Queues {
		extra := ""
		if f := standardOnlyFeature(q); f != "" {
			extra = "  [" + f + "]"
		}
		fmt.Fprintf(t.out, "  %-34s active %-3d dead-lettered %-3d ttl %s%s\n", q.Name, q.Active, q.DeadLetter, q.TTL, extra)
	}
	fmt.Fprintf(t.out, "Topics     %d\n", len(st.Topics))
	for _, tp := range st.Topics {
		fmt.Fprintf(t.out, "  %s\n", tp.Name)
		for _, s := range tp.Subscriptions {
			fmt.Fprintf(t.out, "    %-32s active %-3d dead-lettered %-3d\n", s.Name, s.Active, s.DeadLetter)
		}
	}
	files, err := backups(t.dir, t.cfg.Namespace)
	if err != nil {
		return err
	}
	newest := "none"
	if len(files) > 0 {
		newest = filepath.Base(files[0])
	}
	fmt.Fprintf(t.out, "Backups    %s (newest: %s)\n", t.dir, newest)
	return nil
}

func (t *tool) up(ctx context.Context) error {
	st, _, err := t.state(ctx)
	if err != nil {
		return err
	}
	from := t.opt.from
	var older []string
	if from == "" {
		files, err := backups(t.dir, t.cfg.Namespace)
		if err != nil {
			return err
		}
		if len(files) > 0 {
			from, older = files[0], files[1:]
		}
	}
	var backup []Message
	if from != "" {
		if backup, err = readBackup(from); err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
	}
	plan, err := planUp(st, backup)
	if err != nil {
		return err
	}
	plan.print(t.out, t.cfg.Namespace, from)
	t.warnOlderBackups(older, backup)
	if plan.empty() {
		return nil
	}
	if ok, err := t.proceed(); !ok {
		return err
	}
	if plan.ChangeSKU {
		if err := t.setSKU(ctx, tierStandard); err != nil {
			return err
		}
	}
	if err := t.createDemoEntities(ctx, plan); err != nil {
		return err
	}
	for _, target := range plan.Restore {
		if err := t.restore(ctx, target); err != nil {
			return fmt.Errorf("%w (a rerun skips %s once it holds messages; empty it first to restore it again)", err, target.path())
		}
	}
	fmt.Fprintln(t.out)
	return t.status(ctx)
}

// warnOlderBackups points at older backups holding entities the newest one
// lacks; only --from restores those.
func (t *tool) warnOlderBackups(older []string, newest []Message) {
	for _, file := range older {
		msgs, err := readBackup(file)
		if err != nil {
			fmt.Fprintf(t.out, "  note: cannot read older backup %s: %v\n", file, err)
			continue
		}
		if only := entitiesOnlyIn(msgs, newest); len(only) > 0 {
			fmt.Fprintf(t.out, "  note: the newest backup has no messages for %s, but %s has; restore those with --from %s\n",
				strings.Join(only, ", "), filepath.Base(file), file)
			newest = append(newest, msgs...) // name each entity once, for its newest file
		}
	}
}

func (t *tool) down(ctx context.Context) error {
	st, _, err := t.state(ctx)
	if err != nil {
		return err
	}
	plan, err := planDown(st)
	if err != nil {
		return err
	}
	file := filepath.Join(t.dir, backupName(t.cfg.Namespace, time.Now()))
	plan.print(t.out, t.cfg.Namespace, file)
	if plan.empty() {
		return nil
	}
	if ok, err := t.proceed(); !ok {
		return err
	}
	if len(plan.Backup) > 0 {
		msgs, err := t.backup(ctx, plan.Backup)
		if err == nil {
			err = writeBackup(file, msgs)
		}
		if err != nil {
			return fmt.Errorf("backup failed, nothing was deleted: %w", err)
		}
		t.logf("wrote %d messages to %s", len(msgs), file)
	}
	if err := t.deleteEntities(ctx, plan); err != nil {
		return err
	}
	if err := t.capTTL(ctx, plan.SetTTL); err != nil {
		return err
	}
	if plan.ChangeSKU {
		if err := t.setSKU(ctx, tierBasic); err != nil {
			return err
		}
	}
	fmt.Fprintln(t.out)
	if err := t.status(ctx); err != nil {
		return err
	}
	if a, err := t.arm(ctx); err != nil {
		return err
	} else if a.SKU != tierBasic {
		return fmt.Errorf("the namespace is %s, not Basic", a.SKU)
	}
	return nil
}
