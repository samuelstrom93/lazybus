package bus

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PropertyTypes lists the property types in selector order.
func PropertyTypes() []PropertyType {
	return []PropertyType{TypeString, TypeInt, TypeLong, TypeDouble, TypeBool, TypeGUID, TypeDateTime}
}

// PropertyEdit is one Property Edit: set Key to Value of Type (add or
// change), or, with Remove, remove Key. Value holds the Go value that
// matches Type, as in Property.
type PropertyEdit struct {
	Key    string
	Type   PropertyType
	Value  any
	Remove bool
}

// Edits are the Pending Edits of one dead-letter message: Property Edits
// (at most one per key, in the order they were made), Subject and
// ContentType changes, and a body edit. The zero value changes nothing.
// Methods never modify the receiver; they return a changed copy.
type Edits struct {
	Properties []PropertyEdit
	// Subject and ContentType are the new values when set; an empty
	// string leaves the field unset on the copy.
	Subject     *string
	ContentType *string
	// Body is the new body when BodyEdited (it may be empty).
	Body       []byte
	BodyEdited bool
}

// IsZero reports whether e changes nothing.
func (e Edits) IsZero() bool {
	return len(e.Properties) == 0 && e.Subject == nil && e.ContentType == nil && !e.BodyEdited
}

// Count is the number of changes: one per Property Edit, Subject,
// ContentType and body.
func (e Edits) Count() int {
	n := len(e.Properties)
	for _, set := range []bool{e.Subject != nil, e.ContentType != nil, e.BodyEdited} {
		if set {
			n++
		}
	}
	return n
}

// Property returns the Property Edit of key.
func (e Edits) Property(key string) (PropertyEdit, bool) {
	for _, p := range e.Properties {
		if p.Key == key {
			return p, true
		}
	}
	return PropertyEdit{}, false
}

// ErrMarkerEdit refuses a Property Edit of a Dead-letter Marker.
var ErrMarkerEdit = errors.New("dead-letter markers are removed on resubmit and can't be edited")

func findProperty(props []Property, key string) (Property, bool) {
	for _, p := range props {
		if p.Key == key {
			return p, true
		}
	}
	return Property{}, false
}

// withProperty replaces key's edit with pe, or drops it when drop is set.
func (e Edits) withProperty(key string, pe PropertyEdit, drop bool) Edits {
	out := e
	out.Properties = slices.Clone(e.Properties)
	i := slices.IndexFunc(out.Properties, func(p PropertyEdit) bool { return p.Key == key })
	switch {
	case drop && i >= 0:
		out.Properties = slices.Delete(out.Properties, i, i+1)
	case drop:
	case i >= 0:
		out.Properties[i] = pe
	default:
		out.Properties = append(out.Properties, pe)
	}
	if len(out.Properties) == 0 {
		out.Properties = nil
	}
	return out
}

// SetProperty adds or changes the property pe.Key on a message whose
// application properties are orig. Setting a key that orig already has is
// a change (an add of an existing key included). Setting a property to
// the type and value it already has drops its edit: no change, no Pending
// Edit. Dead-letter Markers are refused.
func (e Edits) SetProperty(orig []Property, pe PropertyEdit) (Edits, error) {
	pe.Remove = false
	if err := validateEdit(pe); err != nil {
		return e, err
	}
	if o, ok := findProperty(orig, pe.Key); ok && o.Type == pe.Type && SameValue(o.Value, pe.Value) {
		return e.withProperty(pe.Key, pe, true), nil
	}
	return e.withProperty(pe.Key, pe, false), nil
}

// ToggleRemove marks key for removal, or undoes that. On a key orig does
// not have (a pending add) it drops the add. A pending change of key is
// replaced by the removal. Dead-letter Markers are refused.
func (e Edits) ToggleRemove(orig []Property, key string) (Edits, error) {
	if IsMarker(key) {
		return e, ErrMarkerEdit
	}
	cur, edited := e.Property(key)
	if _, ok := findProperty(orig, key); !ok {
		if !edited {
			return e, fmt.Errorf("%s is not a property of this message", key)
		}
		return e.withProperty(key, cur, true), nil
	}
	if edited && cur.Remove {
		return e.withProperty(key, cur, true), nil
	}
	return e.withProperty(key, PropertyEdit{Key: key, Remove: true}, false), nil
}

// ApplyProperties returns props with the Property Edits applied: removed
// keys dropped, changed keys replaced in place, added keys appended in
// edit order. Dead-letter Markers are never in the result.
func (e Edits) ApplyProperties(props []Property) []Property {
	var out []Property
	for _, p := range props {
		if IsMarker(p.Key) {
			continue
		}
		if pe, ok := e.Property(p.Key); ok {
			if pe.Remove {
				continue
			}
			p = Property{Key: pe.Key, Type: pe.Type, Value: pe.Value}
		}
		out = append(out, p)
	}
	for _, pe := range e.Properties {
		if _, ok := findProperty(props, pe.Key); !ok && !pe.Remove {
			out = append(out, Property{Key: pe.Key, Type: pe.Type, Value: pe.Value})
		}
	}
	return out
}

// Validate checks e as the service layer applies it: keys present, unique
// and never a Dead-letter Marker, each value of its type's Go type.
func (e Edits) Validate() error {
	seen := map[string]bool{}
	for _, pe := range e.Properties {
		if seen[pe.Key] {
			return fmt.Errorf("property %s is edited twice", pe.Key)
		}
		seen[pe.Key] = true
		if pe.Remove {
			if pe.Key == "" {
				return errors.New("property key is empty")
			}
			if IsMarker(pe.Key) {
				return ErrMarkerEdit
			}
			continue
		}
		if err := validateEdit(pe); err != nil {
			return err
		}
	}
	return nil
}

func validateEdit(pe PropertyEdit) error {
	switch {
	case pe.Key == "":
		return errors.New("key is empty")
	case strings.TrimSpace(pe.Key) != pe.Key:
		return errors.New("key has leading or trailing spaces")
	case IsMarker(pe.Key):
		return ErrMarkerEdit
	}
	ok := false
	switch v := pe.Value.(type) {
	case string:
		switch pe.Type {
		case TypeString:
			ok = true
		case TypeGUID:
			_, err := uuid.Parse(v)
			ok = err == nil
		}
	case int32:
		ok = pe.Type == TypeInt
	case int64:
		ok = pe.Type == TypeLong
	case float64:
		ok = pe.Type == TypeDouble
	case bool:
		ok = pe.Type == TypeBool
	case time.Time:
		ok = pe.Type == TypeDateTime
	}
	if !ok {
		return fmt.Errorf("property %s: value %v (%T) is not a %s", pe.Key, pe.Value, pe.Value, pe.Type)
	}
	return nil
}

// ParseValue parses text as a value of type t: the Go value a Property of
// that type holds. Int is a 32-bit and Long a 64-bit integer, Bool is
// true or false, Guid is normalized to lower-case 8-4-4-4-12 form,
// DateTime is RFC 3339 (with a zone offset or Z). The error says what was
// expected.
func ParseValue(t PropertyType, text string) (any, error) {
	s := strings.TrimSpace(text)
	switch t {
	case TypeString:
		return text, nil
	case TypeInt:
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, intErr(s, err, math.MinInt32, math.MaxInt32)
		}
		return int32(n), nil
	case TypeLong:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, intErr(s, err, math.MinInt64, math.MaxInt64)
		}
		return n, nil
	case TypeDouble:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%q is not a finite number (e.g. 12.5 or 1e-3)", s)
		}
		return f, nil
	case TypeBool:
		switch s {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not true or false", s)
	case TypeGUID:
		u, err := uuid.Parse(s)
		if err != nil || strings.HasPrefix(strings.ToLower(s), "urn:") {
			return nil, fmt.Errorf("%q is not a GUID (e.g. 6f1c2b9e-4d2a-4c1e-9b7a-000000000001)", s)
		}
		return u.String(), nil
	case TypeDateTime:
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an RFC 3339 time (e.g. 2026-09-18T22:15:00Z)", s)
		}
		return tm, nil
	}
	return nil, fmt.Errorf("unknown type %v", t)
}

func intErr(s string, err error, lo, hi int64) error {
	var ne *strconv.NumError
	if errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) {
		return fmt.Errorf("%s is out of range (%d to %d)", s, lo, hi)
	}
	return fmt.Errorf("%q is not a whole number", s)
}

// SameValue reports whether two property values are equal; times are
// compared as instants.
func SameValue(a, b any) bool {
	if ta, ok := a.(time.Time); ok {
		tb, ok := b.(time.Time)
		return ok && ta.Equal(tb)
	}
	if ba, ok := a.([]byte); ok {
		bb, ok := b.([]byte)
		return ok && bytes.Equal(ba, bb)
	}
	return a == b
}
