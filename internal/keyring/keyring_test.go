package keyring

import (
	"errors"
	"testing"
)

func TestMemory_SetGetDelete(t *testing.T) {
	k := NewMemory()
	if _, err := k.Get("prod"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on empty keyring: err = %v, want ErrNotFound", err)
	}
	if err := k.Set("prod", "s3cret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := k.Set("prod", "rotated"); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if got, err := k.Get("prod"); err != nil || got != "rotated" {
		t.Fatalf("Get = %q, %v; want %q, nil", got, err, "rotated")
	}
	if err := k.Delete("prod"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := k.Delete("prod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete twice: err = %v, want ErrNotFound", err)
	}
	if _, err := k.Get("prod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete: err = %v, want ErrNotFound", err)
	}
}

func TestMemory_FailInjection(t *testing.T) {
	k := NewMemory()
	boom := errors.New("keyring locked")
	k.Fail = boom
	if err := k.Set("prod", "x"); !errors.Is(err, boom) {
		t.Errorf("Set: err = %v, want %v", err, boom)
	}
	if _, err := k.Get("prod"); !errors.Is(err, boom) {
		t.Errorf("Get: err = %v, want %v", err, boom)
	}
	if err := k.Delete("prod"); !errors.Is(err, boom) {
		t.Errorf("Delete: err = %v, want %v", err, boom)
	}
	if len(k.Secrets()) != 0 {
		t.Errorf("secrets = %v, want none after failed Set", k.Secrets())
	}
}
