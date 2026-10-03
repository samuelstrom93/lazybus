package seed

import (
	"strings"
	"testing"

	"github.com/samuelstrom93/lazybus/internal/bus/azure"
)

func TestCheckEmulator(t *testing.T) {
	ok := []string{
		azure.EmulatorConnectionString("localhost", 5682),
		azure.EmulatorConnectionString("127.0.0.1", 5682),
		azure.EmulatorConnectionString("[::1]", 5682),
	}
	for _, cs := range ok {
		if err := checkEmulator(cs); err != nil {
			t.Errorf("%s: %v", cs, err)
		}
	}
	bad := map[string]string{
		"Endpoint=sb://sb-prod.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=x":                             "UseDevelopmentEmulator",
		"Endpoint=sb://sb-prod.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=x;UseDevelopmentEmulator=true": "loopback",
		azure.EmulatorConnectionString("10.0.0.5", 5672):                                                                    "loopback",
	}
	for cs, want := range bad {
		if err := checkEmulator(cs); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", cs, err, want)
		}
	}
}
