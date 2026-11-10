package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Message is one backed-up message. The backup file is a JSON array of
// these, field names as written here (the format of the first, hand-made
// backup of 2026-11-10).
//
// The format is lossy, which is fine for demo data: Body is text (a body
// that is not UTF-8 fails the backup), and Props go through JSON, so a
// property keeps only its JSON type. On restore a number without fraction
// or exponent becomes int64, any other number float64; times, byte arrays
// and the like come back as strings. System properties other than the ones
// below (TimeToLive, CorrelationID, enqueued time, ...) are not kept.
type Message struct {
	Entity                string // queue, topic/subscription, or either + "/$DLQ"
	MessageID             string
	ContentType           string
	Subject               string
	SessionID             string
	DeadLetterReason      string
	DeadLetterDescription string
	Seq                   int64 // sequence number at backup time; informational
	Body                  string
	Props                 map[string]any
}

const dlqSuffix = "/$DLQ"

// splitEntity returns the entity a backup entry belongs to and whether the
// entry was in its dead-letter queue.
func splitEntity(entity string) (base string, dlq bool) {
	if b, ok := strings.CutSuffix(entity, dlqSuffix); ok {
		return b, true
	}
	return entity, false
}

// parseBackup decodes a backup file. Numbers in Props become int64 when
// they are written as integers and fit, float64 otherwise.
func parseBackup(data []byte) ([]Message, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var msgs []Message
	if err := dec.Decode(&msgs); err != nil {
		return nil, fmt.Errorf("parse backup: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse backup: data after the message array")
	}
	for i, m := range msgs {
		if m.Entity == "" {
			return nil, fmt.Errorf("parse backup: message %d (%q) has no Entity", i, m.MessageID)
		}
		for k, v := range m.Props {
			if n, ok := v.(json.Number); ok {
				m.Props[k] = number(n)
			}
		}
	}
	return msgs, nil
}

func number(n json.Number) any {
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return n.String()
}

// The broker adds these two application properties when it dead-letters a
// message; DeadLetter sets them again on restore.
const (
	propDeadLetterReason      = "DeadLetterReason"
	propDeadLetterDescription = "DeadLetterErrorDescription"
)

// restoreProps returns the application properties to send for m: for a
// dead-lettered entry, without the two the broker adds itself.
func restoreProps(m Message) map[string]any {
	_, dlq := splitEntity(m.Entity)
	out := make(map[string]any, len(m.Props))
	for k, v := range m.Props {
		if dlq && (k == propDeadLetterReason || k == propDeadLetterDescription) {
			continue
		}
		out[k] = v
	}
	return out
}

// deadLetterInfo returns the reason and description to dead-letter m with,
// falling back to the broker-set application properties.
func deadLetterInfo(m Message) (reason, description string) {
	reason, description = m.DeadLetterReason, m.DeadLetterDescription
	if s, ok := m.Props[propDeadLetterReason].(string); ok && reason == "" {
		reason = s
	}
	if s, ok := m.Props[propDeadLetterDescription].(string); ok && description == "" {
		description = s
	}
	return reason, description
}

// backupDir is $XDG_STATE_HOME/lazybus/testns, by default
// ~/.local/state/lazybus/testns.
func backupDir(getenv func(string) string, home string) string {
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "lazybus", "testns")
}

// backupName is <namespace>-<UTC time>.json. Names sort by time, and after
// the hand-made <namespace>-2026-11-10.json.
func backupName(namespace string, now time.Time) string {
	return namespace + "-" + now.UTC().Format("2006-01-02T150405Z") + ".json"
}

// newestBackup returns the backup of namespace in dir whose name sorts
// last, or "" when there is none.
func newestBackup(dir, namespace string) (string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, namespace+"-") && strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	return filepath.Join(dir, slices.Max(names)), nil
}

// writeBackup writes msgs to a new file (mode 0600, never overwriting) and
// reads it back to check that it parses to the same number of messages.
func writeBackup(path string, msgs []Message) error {
	if msgs == nil {
		msgs = []Message{}
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	back, err := readBackup(path)
	if err != nil {
		return fmt.Errorf("verify %s: %w", path, err)
	}
	if len(back) != len(msgs) {
		return fmt.Errorf("verify %s: read back %d messages, wrote %d", path, len(back), len(msgs))
	}
	return nil
}

func readBackup(path string) ([]Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseBackup(data)
}
