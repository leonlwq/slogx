package lumberjack

import (
	"reflect"
	"testing"
)

func assert(condition bool, t testing.TB, msg string, v ...any) {
	t.Helper()
	if !condition {
		t.Fatalf(msg, v...)
	}
}

func equals(exp, act any, t testing.TB) {
	t.Helper()
	if !reflect.DeepEqual(exp, act) {
		t.Fatalf("exp: %v (%T), got: %v (%T)", exp, exp, act, act)
	}
}

func isNil(obtained any, t testing.TB) {
	t.Helper()
	if !isNilValue(obtained) {
		t.Fatalf("expected nil, got: %v", obtained)
	}
}

func notNil(obtained any, t testing.TB) {
	t.Helper()
	if isNilValue(obtained) {
		t.Fatalf("expected non-nil, got: %v", obtained)
	}
}

func isNilValue(obtained any) bool {
	if obtained == nil {
		return true
	}
	switch v := reflect.ValueOf(obtained); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	}
	return false
}
