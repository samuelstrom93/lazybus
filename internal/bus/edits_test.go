package bus

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseValue(t *testing.T) {
	at := time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC)
	cases := []struct {
		typ  PropertyType
		in   string
		want any
		err  string // substring of the error; empty: valid
	}{
		{TypeString, " keep spaces ", " keep spaces ", ""},
		{TypeString, "", "", ""},
		{TypeInt, "42", int32(42), ""},
		{TypeInt, " -2147483648 ", int32(-2147483648), ""},
		{TypeInt, "2147483648", nil, "out of range (-2147483648 to 2147483647)"},
		{TypeInt, "4.5", nil, "not a whole number"},
		{TypeInt, "", nil, "not a whole number"},
		{TypeLong, "9223372036854775807", int64(9223372036854775807), ""},
		{TypeLong, "9223372036854775808", nil, "out of range"},
		{TypeLong, "1e3", nil, "not a whole number"},
		{TypeDouble, "12.5", 12.5, ""},
		{TypeDouble, "1e-3", 1e-3, ""},
		{TypeDouble, "NaN", nil, "not a finite number"},
		{TypeDouble, "inf", nil, "not a finite number"},
		{TypeDouble, "1e400", nil, "not a finite number"},
		{TypeDouble, "abc", nil, "not a finite number"},
		{TypeBool, "true", true, ""},
		{TypeBool, "false", false, ""},
		{TypeBool, "True", nil, "not true or false"},
		{TypeBool, "1", nil, "not true or false"},
		{TypeGUID, "6F1C2B9E-4D2A-4C1E-9B7A-000000000001", "6f1c2b9e-4d2a-4c1e-9b7a-000000000001", ""},
		{TypeGUID, "{6f1c2b9e-4d2a-4c1e-9b7a-000000000001}", "6f1c2b9e-4d2a-4c1e-9b7a-000000000001", ""},
		{TypeGUID, "urn:uuid:6f1c2b9e-4d2a-4c1e-9b7a-000000000001", nil, "not a GUID"},
		{TypeGUID, "6f1c2b9e", nil, "not a GUID"},
		{TypeDateTime, "2026-09-18T22:15:00Z", at, ""},
		{TypeDateTime, "2026-09-19T00:15:00+02:00", at, ""},
		{TypeDateTime, "2026-09-18 22:15:00", nil, "not an RFC 3339 time"},
		{TypeDateTime, "2026-09-18T22:15:00", nil, "not an RFC 3339 time"},
	}
	for _, tc := range cases {
		got, err := ParseValue(tc.typ, tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParseValue(%v, %q) error = %v, want %q", tc.typ, tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || !SameValue(got, tc.want) || reflect.TypeOf(got) != reflect.TypeOf(tc.want) {
			t.Errorf("ParseValue(%v, %q) = %#v, %v; want %#v", tc.typ, tc.in, got, err, tc.want)
		}
	}
}

var origProps = []Property{
	{Key: "tenant", Type: TypeString, Value: "contoso"},
	{Key: "orderId", Type: TypeLong, Value: int64(1001)},
	{Key: "isRetry", Type: TypeBool, Value: false},
}

func keysOf(e Edits) string {
	var s []string
	for _, p := range e.Properties {
		mark := "="
		if p.Remove {
			mark = "-"
		}
		s = append(s, mark+p.Key)
	}
	return strings.Join(s, " ")
}

func TestEditsModel(t *testing.T) {
	var e Edits
	if !e.IsZero() {
		t.Fatal("zero Edits not IsZero")
	}
	must := func(e Edits, err error) Edits {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	// Change, add, remove.
	e = must(e.SetProperty(origProps, PropertyEdit{Key: "orderId", Type: TypeLong, Value: int64(2002)}))
	e = must(e.SetProperty(origProps, PropertyEdit{Key: "region", Type: TypeString, Value: "eu"}))
	e = must(e.ToggleRemove(origProps, "isRetry"))
	if got := keysOf(e); got != "=orderId =region -isRetry" || e.Count() != 3 {
		t.Fatalf("edits = %s (%d)", got, e.Count())
	}

	// An add of an existing key is a change of it, in place.
	e2 := must(e.SetProperty(origProps, PropertyEdit{Key: "orderId", Type: TypeInt, Value: int32(7)}))
	if pe, _ := e2.Property("orderId"); pe.Type != TypeInt || keysOf(e2) != keysOf(e) {
		t.Fatalf("re-set orderId: %+v, %s", pe, keysOf(e2))
	}
	// Setting the original type and value back drops the edit.
	e2 = must(e2.SetProperty(origProps, PropertyEdit{Key: "orderId", Type: TypeLong, Value: int64(1001)}))
	if got := keysOf(e2); got != "=region -isRetry" {
		t.Fatalf("set back to original: %s", got)
	}
	// Toggle removal: again undoes it; on a pending add drops the add; on
	// a change replaces it.
	e2 = must(e2.ToggleRemove(origProps, "isRetry"))
	e2 = must(e2.ToggleRemove(origProps, "region"))
	if !e2.IsZero() {
		t.Fatalf("after undoing everything: %s", keysOf(e2))
	}
	e3 := must(e.ToggleRemove(origProps, "orderId"))
	if pe, _ := e3.Property("orderId"); !pe.Remove {
		t.Fatalf("d on a changed key: %+v", pe)
	}
	if _, err := e.ToggleRemove(origProps, "nope"); err == nil {
		t.Fatal("removal of a key the message does not have accepted")
	}
	// The receiver never changes.
	if got := keysOf(e); got != "=orderId =region -isRetry" {
		t.Fatalf("receiver changed: %s", got)
	}

	// Markers are never a Property Edit target.
	for _, k := range Markers() {
		if _, err := e.SetProperty(origProps, PropertyEdit{Key: k, Type: TypeString, Value: "x"}); !errors.Is(err, ErrMarkerEdit) {
			t.Errorf("set %s: %v", k, err)
		}
		if _, err := e.ToggleRemove(origProps, k); !errors.Is(err, ErrMarkerEdit) {
			t.Errorf("remove %s: %v", k, err)
		}
	}
	// A value of the wrong Go type, or an empty key, is refused.
	for _, pe := range []PropertyEdit{
		{Key: "a", Type: TypeInt, Value: int64(1)},
		{Key: "a", Type: TypeGUID, Value: "nope"},
		{Key: "", Type: TypeString, Value: "x"},
		{Key: " a", Type: TypeString, Value: "x"},
	} {
		if _, err := e.SetProperty(origProps, pe); err == nil {
			t.Errorf("SetProperty(%+v) accepted", pe)
		}
	}

	got := e.ApplyProperties(append([]Property{{Key: MarkerDeadLetterReason, Type: TypeString, Value: "r"}}, origProps...))
	want := []Property{
		{Key: "tenant", Type: TypeString, Value: "contoso"},
		{Key: "orderId", Type: TypeLong, Value: int64(2002)},
		{Key: "region", Type: TypeString, Value: "eu"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplyProperties = %+v", got)
	}
}

func TestEditsValidate(t *testing.T) {
	ok := Edits{Properties: []PropertyEdit{
		{Key: "a", Type: TypeInt, Value: int32(1)},
		{Key: "b", Remove: true},
		{Key: "c", Type: TypeDateTime, Value: time.Now()},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Edits{
		{Properties: []PropertyEdit{{Key: MarkerDeadLetterErrorDescription, Remove: true}}},
		{Properties: []PropertyEdit{{Key: MarkerDeadLetterReason, Type: TypeString, Value: ""}}},
		{Properties: []PropertyEdit{{Key: "a", Remove: true}, {Key: "a", Type: TypeString, Value: ""}}},
		{Properties: []PropertyEdit{{Key: "a", Type: TypeDouble, Value: int32(1)}}},
		{Properties: []PropertyEdit{{Key: "", Remove: true}}},
	} {
		if err := e.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil", e.Properties)
		}
	}
}
