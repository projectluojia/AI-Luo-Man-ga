package main

import "testing"

func TestListenAddressFromArgs(t *testing.T) {
	address, err := listenAddressFromArgs([]string{"--listen", "127.0.0.1:50071"})
	if err != nil || address != "127.0.0.1:50071" {
		t.Fatalf("got %q %v, want 127.0.0.1:50071", address, err)
	}
	if _, err := listenAddressFromArgs(nil); err == nil {
		t.Fatal("missing --listen was accepted")
	}
	if _, err := listenAddressFromArgs([]string{"--listen"}); err == nil {
		t.Fatal("empty --listen was accepted")
	}
}
