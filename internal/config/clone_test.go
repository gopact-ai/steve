package config

import (
	"fmt"
	"reflect"
	"testing"
)

// fill sets every exported field reachable from v. Each map gets one
// filled entry and each pointer a filled value, so every nested field is
// reached; each slice gets one filled element, or is left empty or nil as
// slices says.
func fill(v reflect.Value, slices sliceShape) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), slices)
			}
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(key, slices)
		fill(value, slices)
		v.SetMapIndex(key, value)
	case reflect.Slice:
		switch slices {
		case filledSlices:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			fill(v.Index(0), slices)
		case emptySlices:
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), slices)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	default:
		panic(fmt.Sprintf("fill: add a case for %s", v.Type()))
	}
}

type sliceShape int

const (
	filledSlices sliceShape = iota
	emptySlices
	nilSlices
)

// shared names every map, slice or pointer reachable from both a and b.
func shared(a, b reflect.Value, path string) []string {
	var out []string
	switch a.Kind() {
	case reflect.Struct:
		for i := range a.NumField() {
			out = append(out, shared(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)...)
		}
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			out = append(out, path)
		}
		for _, key := range a.MapKeys() {
			if other := b.MapIndex(key); other.IsValid() {
				out = append(out, shared(a.MapIndex(key), other, fmt.Sprintf("%s[%v]", path, key))...)
			}
		}
	case reflect.Slice:
		if a.Cap() > 0 && b.Cap() > 0 && a.UnsafePointer() == b.UnsafePointer() {
			out = append(out, path)
		}
		for i := range min(a.Len(), b.Len()) {
			out = append(out, shared(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.UnsafePointer() == b.UnsafePointer() {
			out = append(out, path)
		}
		out = append(out, shared(a.Elem(), b.Elem(), path)...)
	}
	return out
}

// Clone must cover every field, including ones added later: the copy is
// edited while readers use the original.
func TestCloneSharesNothingWithTheOriginal(t *testing.T) {
	var original Config
	fill(reflect.ValueOf(&original).Elem(), filledSlices)
	original.rememberFileRevision("config.json", []byte("{}"))
	clone := original.Clone()
	if !reflect.DeepEqual(clone, &original) {
		t.Fatalf("clone differs from the original:\n%+v\n%+v", clone, &original)
	}
	if paths := shared(reflect.ValueOf(original), reflect.ValueOf(*clone), "Config"); len(paths) > 0 {
		t.Fatalf("clone shares %v with the original", paths)
	}
}

// A clone saves to the same file: empty and absent collections stay apart.
func TestCloneKeepsEmptyAndAbsentCollectionsApart(t *testing.T) {
	for name, slices := range map[string]sliceShape{"empty": emptySlices, "nil": nilSlices} {
		var original Config
		fill(reflect.ValueOf(&original).Elem(), slices)
		if clone := original.Clone(); !reflect.DeepEqual(clone, &original) {
			t.Errorf("clone with %s slices differs:\n%+v\n%+v", name, clone, &original)
		}
	}
	var absent Config
	if clone := absent.Clone(); !reflect.DeepEqual(clone, &absent) {
		t.Errorf("clone of a zero configuration differs:\n%+v", clone)
	}
}

// unexportedReferences names every unexported field reachable from t that
// is not a plain value. fill cannot set such a field, so the tests above
// would not notice a clone that shares it.
func unexportedReferences(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return unexportedReferences(t.Elem(), path+"[]", seen)
	case reflect.Map:
		return append(unexportedReferences(t.Key(), path+"{key}", seen), unexportedReferences(t.Elem(), path+"{}", seen)...)
	case reflect.Struct:
		if seen[t] {
			return nil
		}
		seen[t] = true
		var out []string
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				switch field.Type.Kind() {
				case reflect.Bool, reflect.String,
					reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
					reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
					reflect.Float32, reflect.Float64:
				default:
					out = append(out, path+"."+field.Name)
				}
				continue
			}
			out = append(out, unexportedReferences(field.Type, path+"."+field.Name, seen)...)
		}
		return out
	}
	return nil
}

func TestConfigurationKeepsOnlyPlainValuesUnexported(t *testing.T) {
	if paths := unexportedReferences(reflect.TypeFor[Config](), "Config", map[reflect.Type]bool{}); len(paths) > 0 {
		t.Fatalf("unexported fields the clone tests cannot fill: %v", paths)
	}
}
