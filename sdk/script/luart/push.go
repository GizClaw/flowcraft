package luart

import (
	"fmt"
	"os"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/script"
	lua "github.com/yuin/gopher-lua"
)

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

const signalRaiseMarker = "__flowcraft_signal__"

// pushGoValue converts v into an LValue (tables, numbers, strings, bools, nil,
// nested maps, slices, and Go functions via reflection).
func pushGoValue(L *lua.LState, v any) lua.LValue {
	if v == nil {
		return lua.LNil
	}

	switch x := v.(type) {
	case lua.LValue:
		return x
	case bool:
		return lua.LBool(x)
	case string:
		return lua.LString(x)
	case float32:
		return lua.LNumber(x)
	case float64:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case int8:
		return lua.LNumber(x)
	case int16:
		return lua.LNumber(x)
	case int32:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case uint:
		return lua.LNumber(x)
	case uint8:
		return lua.LNumber(x)
	case uint16:
		return lua.LNumber(x)
	case uint32:
		return lua.LNumber(x)
	case uint64:
		return lua.LNumber(x)
	case map[string]any:
		t := L.NewTable()
		for k, val := range x {
			L.SetField(t, k, pushGoValue(L, val))
		}
		return t
	case []any:
		t := L.NewTable()
		for i, val := range x {
			L.SetTable(t, lua.LNumber(i+1), pushGoValue(L, val))
		}
		return t
	case []string:
		t := L.NewTable()
		for i, s := range x {
			L.SetTable(t, lua.LNumber(i+1), lua.LString(s))
		}
		return t
	case *script.Signal:
		if x == nil {
			return lua.LNil
		}
		t := L.NewTable()
		L.SetField(t, "type", lua.LString(x.Type))
		L.SetField(t, "message", lua.LString(x.Message))
		return t
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Func:
		return goFuncToLG(L, rv)
	case reflect.Map:
		keyKind := rv.Type().Key().Kind()
		if keyKind == reflect.String {
			t := L.NewTable()
			for _, key := range rv.MapKeys() {
				L.SetField(t, key.String(), pushGoValue(L, rv.MapIndex(key).Interface()))
			}
			return t
		}
		if isIntKind(keyKind) {
			t := L.NewTable()
			for _, key := range rv.MapKeys() {
				var n float64
				switch keyKind {
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					n = float64(key.Uint())
				default:
					n = float64(key.Int())
				}
				L.SetTable(t, lua.LNumber(n), pushGoValue(L, rv.MapIndex(key).Interface()))
			}
			return t
		}
		fmt.Fprintf(os.Stderr, "luart: pushGoValue: unsupported map key type %s, serializing as string\n", rv.Type().Key())
	case reflect.Slice, reflect.Array:
		t := L.NewTable()
		n := rv.Len()
		for i := 0; i < n; i++ {
			L.SetTable(t, lua.LNumber(i+1), pushGoValue(L, rv.Index(i).Interface()))
		}
		return t
	}

	return lua.LString(fmt.Sprintf("%v", v))
}

func goFuncToLG(L *lua.LState, fn reflect.Value) *lua.LFunction {
	ft := fn.Type()
	if ft.Kind() != reflect.Func {
		panic("luart: goFuncToLG expects func value")
	}
	return L.NewFunction(func(L *lua.LState) int {
		narg := L.GetTop()
		minIn := ft.NumIn()
		if ft.IsVariadic() {
			minIn--
		}
		if narg < minIn {
			L.RaiseError("expected at least %d arguments, got %d", minIn, narg)
		}
		args, err := buildCallArgs(L, ft, narg)
		if err != nil {
			L.RaiseError("%v", err)
		}
		rets := fn.Call(args)
		return pushReturns(L, ft, rets)
	})
}

func buildCallArgs(L *lua.LState, ft reflect.Type, narg int) ([]reflect.Value, error) {
	numIn := ft.NumIn()
	if !ft.IsVariadic() {
		if narg != numIn {
			return nil, fmt.Errorf("expected %d arguments, got %d", numIn, narg)
		}
		args := make([]reflect.Value, numIn)
		for i := 0; i < numIn; i++ {
			v, err := luaToReflect(L, i+1, ft.In(i))
			if err != nil {
				return nil, err
			}
			args[i] = v
		}
		return args, nil
	}

	if narg < numIn-1 {
		return nil, fmt.Errorf("expected at least %d arguments, got %d", numIn-1, narg)
	}
	args := make([]reflect.Value, numIn)
	for i := 0; i < numIn-1; i++ {
		v, err := luaToReflect(L, i+1, ft.In(i))
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	elem := ft.In(numIn - 1).Elem()
	sliceLen := narg - (numIn - 1)
	slice := reflect.MakeSlice(ft.In(numIn-1), sliceLen, sliceLen)
	for j := 0; j < sliceLen; j++ {
		v, err := luaToReflect(L, numIn+j, elem)
		if err != nil {
			return nil, err
		}
		slice.Index(j).Set(v)
	}
	args[numIn-1] = slice
	return args, nil
}

func luaToReflect(L *lua.LState, n int, want reflect.Type) (reflect.Value, error) {
	lv := L.Get(n)
	if lv == lua.LNil {
		if want.Kind() == reflect.Interface || want.Kind() == reflect.Pointer || want.Kind() == reflect.Map || want.Kind() == reflect.Slice {
			return reflect.Zero(want), nil
		}
		return reflect.Value{}, fmt.Errorf("arg %d: got nil", n)
	}

	switch want.Kind() {
	case reflect.Interface:
		if want.NumMethod() == 0 {
			return luaToInterface(L, n, lv)
		}
	case reflect.String:
		if str, ok := lv.(lua.LString); ok {
			return reflect.ValueOf(string(str)), nil
		}
	case reflect.Bool:
		if b, ok := lv.(lua.LBool); ok {
			return reflect.ValueOf(bool(b)), nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if num, ok := lv.(lua.LNumber); ok {
			return reflect.ValueOf(int64(num)).Convert(want), nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if num, ok := lv.(lua.LNumber); ok {
			return reflect.ValueOf(uint64(num)).Convert(want), nil
		}
	case reflect.Float32, reflect.Float64:
		if num, ok := lv.(lua.LNumber); ok {
			return reflect.ValueOf(float64(num)).Convert(want), nil
		}
	case reflect.Map:
		if want.Key().Kind() == reflect.String && want.Elem().Kind() == reflect.Interface {
			tb, ok := lv.(*lua.LTable)
			if !ok {
				break
			}
			m, err := luaTableToMapAny(L, tb)
			if err != nil {
				return reflect.Value{}, err
			}
			return reflect.ValueOf(m), nil
		}
	}

	return reflect.Value{}, fmt.Errorf("arg %d: cannot convert %v to %s", n, lv, want)
}

func luaToInterface(L *lua.LState, n int, lv lua.LValue) (reflect.Value, error) {
	if lv == lua.LNil {
		return reflect.ValueOf(nil), nil
	}
	switch x := lv.(type) {
	case lua.LBool:
		return reflect.ValueOf(bool(x)), nil
	case lua.LNumber:
		return reflect.ValueOf(float64(x)), nil
	case lua.LString:
		return reflect.ValueOf(string(x)), nil
	case *lua.LTable:
		return reflect.ValueOf(luaTableToAny(L, x)), nil
	default:
		return reflect.Value{}, fmt.Errorf("arg %d: unsupported type %T", n, lv)
	}
}

func luaTableToMapAny(L *lua.LState, tb *lua.LTable) (map[string]any, error) {
	out := make(map[string]any)
	var ferr error
	L.ForEach(tb, func(k, v lua.LValue) {
		if ferr != nil {
			return
		}
		sk, ok := k.(lua.LString)
		if !ok {
			ferr = fmt.Errorf("table keys must be strings for host maps")
			return
		}
		out[string(sk)] = luaValueToAny(L, v)
	})
	return out, ferr
}

func luaTableToAny(L *lua.LState, tb *lua.LTable) any {
	if tb.Len() > 0 {
		arr := make([]any, tb.Len())
		for i := 1; i <= tb.Len(); i++ {
			arr[i-1] = luaValueToAny(L, L.GetTable(tb, lua.LNumber(i)))
		}
		return arr
	}
	m, _ := luaTableToMapAny(L, tb)
	return m
}

func luaValueToAny(L *lua.LState, v lua.LValue) any {
	if v == lua.LNil {
		return nil
	}
	switch x := v.(type) {
	case lua.LBool:
		return bool(x)
	case lua.LNumber:
		return float64(x)
	case lua.LString:
		return string(x)
	case *lua.LTable:
		return luaTableToAny(L, x)
	default:
		return v.String()
	}
}

func pushReturns(L *lua.LState, ft reflect.Type, rets []reflect.Value) int {
	switch len(rets) {
	case 0:
		return 0
	case 1:
		L.Push(pushReflectValue(L, rets[0]))
		return 1
	case 2:
		if ft.NumOut() == 2 && ft.Out(1).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
			L.Push(pushReflectValue(L, rets[0]))
			if rets[1].IsNil() {
				L.Push(lua.LNil)
			} else {
				e := rets[1].Interface().(error)
				L.Push(lua.LString(e.Error()))
			}
			return 2
		}
		L.Push(pushReflectValue(L, rets[0]))
		L.Push(pushReflectValue(L, rets[1]))
		return 2
	default:
		for _, rv := range rets {
			L.Push(pushReflectValue(L, rv))
		}
		return len(rets)
	}
}

func pushReflectValue(L *lua.LState, rv reflect.Value) lua.LValue {
	if !rv.IsValid() {
		return lua.LNil
	}
	if rv.Kind() == reflect.Interface && rv.IsNil() {
		return lua.LNil
	}
	if rv.Kind() == reflect.Pointer && rv.IsNil() {
		return lua.LNil
	}
	return pushGoValue(L, rv.Interface())
}
