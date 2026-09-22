package console

import "reflect"

// copyConsoleRecord owns the comparison image of a changed, JSON-validated
// console DTO. It preserves empty containers, time.Time's monotonic reading
// and material bytes excluded from JSON. A JSON round trip would make these
// unchanged values appear dirty on every subsequent save.
//
// This is confined to the owner's record types, not arbitrary application
// objects. Their mutable state is in exported fields; private time.Time state
// is immutable. Only changed records are copied, before entering the write Tx.
func copyConsoleRecord(value any) any {
	return copyConsoleValue(reflect.ValueOf(value)).Interface()
}

func copyConsoleValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(copyConsoleValue(value.Elem()))
		return out
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		for entries := value.MapRange(); entries.Next(); {
			out.SetMapIndex(entries.Key(), copyConsoleValue(entries.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(copyConsoleValue(value.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				out.Field(i).Set(copyConsoleValue(value.Field(i)))
			}
		}
		return out
	default:
		return value
	}
}
