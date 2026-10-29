package main

import (
	"slices"
	"testing"
)

func TestSplitRemote(t *testing.T) {
	rf, rest, err := splitRemote([]string{"-on", "instabox", "-job", "build", "--listen=127.0.0.1:1", "-debug",
		"-controller-url", "https://x.example", "-snapshot=sn_1", "-install", "none", "w.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if rf.on != "instabox" || rf.listen != "127.0.0.1:1" || rf.controllerURL != "https://x.example" || rf.snapshot != "sn_1" {
		t.Errorf("%+v", rf)
	}
	want := []string{"-job", "build", "-debug", "-install", "none", "w.yml"}
	if !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
	if _, _, err := splitRemote([]string{"-on"}); err == nil {
		t.Error("a flag with no value was accepted")
	}
}
