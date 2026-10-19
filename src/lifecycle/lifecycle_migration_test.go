package lifecycle

import (
	"errors"
	"testing"
)

func TestStartLayoutMigration(t *testing.T) {
	SetLayoutMigration(nil)
	if started, err := StartLayoutMigration(); started || err != nil {
		t.Fatalf("nothing registered: %v %v", started, err)
	}
	calls := 0
	SetLayoutMigration(func() (bool, error) { calls++; return true, nil })
	if started, err := StartLayoutMigration(); !started || err != nil || calls != 1 {
		t.Fatalf("%v %v %d", started, err, calls)
	}
	SetLayoutMigration(func() (bool, error) { return false, errors.New("boom") })
	if _, err := StartLayoutMigration(); err == nil {
		t.Fatal("the error is lost")
	}
	SetLayoutMigration(nil)
}
