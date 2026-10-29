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

func TestTakeSecretsReadsTheEnvironment(t *testing.T) {
	t.Setenv("MY_TOKEN", "v")
	t.Setenv("GITHUB_TOKEN", "")
	rest, secrets, err := takeSecrets([]string{"-job", "a", "-secret", "MY_TOKEN", "-var", "X=1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rest, []string{"-job", "a", "-var", "X=1"}) || secrets["MY_TOKEN"] != "v" {
		t.Errorf("rest = %v, secrets = %v", rest, secrets)
	}
	if _, _, err := takeSecrets([]string{"-secret", "NOPE_NOT_SET"}); err == nil {
		t.Error("a missing variable was accepted")
	}
}
