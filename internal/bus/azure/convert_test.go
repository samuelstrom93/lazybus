package azure

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

func TestToProperty(t *testing.T) {
	at := time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC)
	id := amqp.UUID{0x6f, 0x1c, 0x2b, 0x9e, 0x4d, 0x2a, 0x4c, 0x1e, 0x9b, 0x7a, 0, 0, 0, 0, 0, 1}
	type point struct{ X, Y int }
	for _, tc := range []struct {
		name  string
		in    any
		typ   bus.PropertyType
		value any
	}{
		{"string", "eu-north", bus.TypeString, "eu-north"},
		{"int32", int32(-7), bus.TypeInt, int32(-7)},
		{"int8", int8(-8), bus.TypeInt, int32(-8)},
		{"int16", int16(-16), bus.TypeInt, int32(-16)},
		{"uint8", uint8(255), bus.TypeInt, int32(255)},
		{"uint16", uint16(65535), bus.TypeInt, int32(65535)},
		{"int64", int64(-1 << 40), bus.TypeLong, int64(-1 << 40)},
		{"int", 42, bus.TypeLong, int64(42)},
		{"uint32", uint32(math.MaxUint32), bus.TypeLong, int64(math.MaxUint32)},
		{"uint64 max long", uint64(math.MaxInt64), bus.TypeLong, int64(math.MaxInt64)},
		{"uint64 above long", uint64(math.MaxInt64) + 1, bus.TypeString, "9223372036854775808"},
		{"float64", 12.5, bus.TypeDouble, 12.5},
		{"float32", float32(0.25), bus.TypeDouble, 0.25},
		{"bool", true, bus.TypeBool, true},
		{"uuid", id, bus.TypeGUID, "6f1c2b9e-4d2a-4c1e-9b7a-000000000001"},
		{"time", at, bus.TypeDateTime, at},
		{"nil", nil, bus.TypeString, nil},
		{"unsupported", point{1, 2}, bus.TypeString, "{1 2}"},
		{"unsupported bytes", []byte("hi"), bus.TypeString, "[104 105]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toProperty("k", tc.in)
			want := bus.Property{Key: "k", Type: tc.typ, Value: tc.value}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("toProperty(%T %v) = %#v, want %#v", tc.in, tc.in, got, want)
			}
		})
	}
}
