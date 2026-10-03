package bus

import (
	"errors"
	"testing"
)

func TestShortDetail(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		// S4: a Listen-only SAS rule; the azure classifier's Msg is the
		// SDK error text up to the TrackingId.
		{&Error{Kind: ErrUnauthorized, Op: "send to orders",
			Msg: "(unauthorized): *Error{Condition: amqp:unauthorized-access, Description: Unauthorized access. 'Send' claim(s) are required to perform this operation. Resource: 'sb://sb-lazybus-test-81abd072.servicebus.windows.net/lazybus-e2e-q'"},
			"unauthorized: 'Send' claim(s) are required to perform…"},
		// Unknown kind: the condition instead.
		{&Error{Msg: "(error): *Error{Condition: amqp:internal-error, Description: Internal failure.\nRetry later, Info: map[]}"},
			"amqp:internal-error: Internal failure"},
		{&Error{Kind: ErrNotAllowed, Msg: "amqp:not-allowed: SessionId not set"}, "amqp:not-allowed: SessionId not set"},
		{errors.New("link detached. reconnecting\r\nnow"), "link detached"},
	}
	for _, tc := range cases {
		if got := shortDetail(tc.err); got != tc.want {
			t.Errorf("shortDetail(%q)\n got %q\nwant %q", tc.err, got, tc.want)
		}
	}
}
