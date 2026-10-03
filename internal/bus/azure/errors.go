package azure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Safe runs f, maps its error to a *bus.Error and turns an SDK panic into
// an error. The admin client's subscription runtime calls panic against
// the emulator in SDK v1.10.0 (S−1); a panic must never take the UI down.
// Every admin call goes through it, tools/seed's too.
func Safe(op string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &bus.Error{Kind: bus.ErrUnknown, Op: op, Msg: fmt.Sprintf("SDK panic: %v", r)}
		}
	}()
	return mapErr(op, f())
}

// mapErr wraps an SDK error in a *bus.Error with a kind and a short,
// single-line message. Errors that are already *bus.Error pass through.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var be *bus.Error
	if errors.As(err, &be) {
		if be.Op == "" {
			be.Op = op
		}
		return be
	}
	kind, msg := classify(err)
	return &bus.Error{Kind: kind, Op: op, Msg: msg, Err: err}
}

func classify(err error) (bus.ErrorKind, string) {
	switch {
	case errors.Is(err, context.Canceled):
		return bus.ErrCanceled, "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return bus.ErrTimeout, "timed out"
	}

	var ae *amqp.Error
	if errors.As(err, &ae) {
		kind := bus.ErrUnknown
		switch ae.Condition {
		case amqp.ErrCondNotFound:
			kind = bus.ErrNotFound
		case amqp.ErrCondNotAllowed:
			kind = bus.ErrNotAllowed
		case amqp.ErrCondUnauthorizedAccess:
			kind = bus.ErrUnauthorized
		case "com.microsoft:server-busy":
			kind = bus.ErrThrottled
		case "com.microsoft:timeout":
			kind = bus.ErrTimeout
		}
		msg := string(ae.Condition)
		if d := brief(ae.Description); d != "" {
			msg += ": " + d
		}
		return kind, msg
	}

	var se *azservicebus.Error
	if errors.As(err, &se) {
		switch se.Code {
		case azservicebus.CodeUnauthorizedAccess:
			return bus.ErrUnauthorized, brief(err.Error())
		case azservicebus.CodeNotFound:
			return bus.ErrNotFound, brief(err.Error())
		case azservicebus.CodeTimeout:
			return bus.ErrTimeout, brief(err.Error())
		case azservicebus.CodeConnectionLost, azservicebus.CodeClosed:
			return bus.ErrConnection, brief(err.Error())
		}
	}

	var re *azcore.ResponseError
	if errors.As(err, &re) {
		msg := fmt.Sprintf("HTTP %d", re.StatusCode)
		if re.ErrorCode != "" {
			msg += " " + re.ErrorCode
		}
		switch re.StatusCode {
		case 401, 403:
			return bus.ErrUnauthorized, msg
		case 404:
			return bus.ErrNotFound, msg
		case 429, 503:
			return bus.ErrThrottled, msg
		}
		return bus.ErrUnknown, msg
	}

	var af *azidentity.AuthenticationFailedError
	if errors.As(err, &af) || strings.Contains(err.Error(), "AzureCLICredential") {
		return bus.ErrUnauthorized, "az CLI credential: " + brief(err.Error())
	}

	var oe *net.OpError
	var de *net.DNSError
	if errors.As(err, &oe) || errors.As(err, &de) {
		return bus.ErrConnection, brief(err.Error())
	}
	return bus.ErrUnknown, brief(err.Error())
}

// maxMsg caps an error message; the UI truncates further to fit.
const maxMsg = 300

// brief returns the first non-empty line of s without Service Bus tracking
// noise ("TrackingId:…, SystemTracker:…, Timestamp:…"), capped at maxMsg.
func brief(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.Index(line, "TrackingId:"); i > 0 {
			line = strings.TrimRight(line[:i], " ,.;")
		}
		if r := []rune(line); len(r) > maxMsg {
			line = string(r[:maxMsg-1]) + "…"
		}
		return line
	}
	return ""
}
