package config

import "testing"

func TestParseScoutSpikeRepos(t *testing.T) {
	got, err := ParseScoutSpikeRepos(" mctl-api, .github ,")
	if err != nil || len(got) != 2 || got[0] != "mctl-api" || got[1] != ".github" {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := ParseScoutSpikeRepos(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"..", ".", "a/b", "a b", "ok,.."} {
		if _, err := ParseScoutSpikeRepos(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestParseOwnerUserID(t *testing.T) {
	if id, err := ParseOwnerUserID(""); err != nil || id != 0 {
		t.Fatal(id, err)
	}
	if id, err := ParseOwnerUserID("123"); err != nil || id != 123 {
		t.Fatal(id, err)
	}
	for _, bad := range []string{"abc", "-1", "0"} {
		if _, err := ParseOwnerUserID(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
