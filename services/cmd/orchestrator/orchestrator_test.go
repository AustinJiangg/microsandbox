package main

import (
	"testing"

	"microsandbox/services/pkg/pool"
	"microsandbox/services/pkg/template"
)

// parsePoolSpecs + poolFor are pure map/string logic, so these run with no VM/KVM,
// like the pkg-level unit tests. They cover the pool wiring: --pool-size maps to the
// default template, --pool name=K to named ones, conflicts/bad input are rejected at
// startup, and poolFor selects the right pool (or none). Ported from
// control-plane/pools_test.go (Stage 8a).

func TestParsePoolSpecs(t *testing.T) {
	eq := func(got, want map[string]int) bool {
		if len(got) != len(want) {
			return false
		}
		for k, v := range want {
			if got[k] != v {
				return false
			}
		}
		return true
	}
	cases := []struct {
		name  string
		flags []string
		size  int
		want  map[string]int
	}{
		{"none", nil, 0, map[string]int{}},
		{"default via --pool-size", nil, 3, map[string]int{"default": 3}},
		{"named only", []string{"ml-env=2", "web=1"}, 0, map[string]int{"ml-env": 2, "web": 1}},
		{"default + named", []string{"ml-env=2"}, 4, map[string]int{"default": 4, "ml-env": 2}},
	}
	for _, c := range cases {
		got, err := parsePoolSpecs(c.flags, c.size)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if !eq(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParsePoolSpecsErrors(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		size  int
	}{
		{"no equals", []string{"mlenv"}, 0},
		{"bad int", []string{"ml=x"}, 0},
		{"zero K", []string{"ml=0"}, 0},
		{"negative K", []string{"ml=-1"}, 0},
		{"invalid name", []string{"../x=1"}, 0},
		{"duplicate name", []string{"ml=1", "ml=2"}, 0},
		{"default given via both flags", []string{"default=1"}, 2},
	}
	for _, c := range cases {
		if _, err := parsePoolSpecs(c.flags, c.size); err == nil {
			t.Errorf("%s: want error, got nil", c.name)
		}
	}
}

func TestPoolFor(t *testing.T) {
	s := &server{pools: map[string]*pool.Pool{"default": {}, "ml-env": {}}}
	if s.poolFor(template.Template{Name: "default"}) == nil {
		t.Error("default should be pooled")
	}
	if s.poolFor(template.Template{Name: "ml-env"}) == nil {
		t.Error("ml-env should be pooled")
	}
	if s.poolFor(template.Template{Name: "other"}) != nil {
		t.Error("other (unpooled) should return nil")
	}
}
