//go:build azure

package azure

import (
	"context"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// SplitService returns a repair service that reads target info (admin
// calls) through info and does everything else (peek, receive, send)
// through data. Both must hold the same namespace. The real-namespace E2E
// uses it to reach the send with a Listen-only SAS rule, which can't make
// admin calls.
func SplitService(data, info *Backend) *bus.Service {
	return bus.NewService(splitDriver{driver{data}, driver{info}}, nil, nil)
}

type splitDriver struct {
	driver
	info driver
}

func (s splitDriver) TargetInfo(ctx context.Context, ns bus.Namespace, t bus.Target) (bus.TargetInfo, error) {
	return s.info.TargetInfo(ctx, ns, t)
}
