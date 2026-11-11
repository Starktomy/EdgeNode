//go:build go1.19

package memutils

import (
	"math"
	"testing"
)

func TestMemoryLimitPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial int64
		env     string
		bytes   uint64
		want    int64
		sets    int
	}{
		{name: "512 MiB exact bytes", initial: math.MaxInt64, bytes: 512 << 20, want: (512 << 20) * 4 / 5, sets: 1},
		{name: "explicit GOMEMLIMIT", initial: 256 << 20, env: "256MiB", bytes: 8 << 30, want: 256 << 20},
		{name: "explicit off", initial: math.MaxInt64, env: "off", bytes: 512 << 20, want: math.MaxInt64},
		{name: "preexisting API limit", initial: 128 << 20, bytes: 512 << 20, want: 128 << 20},
		{name: "unknown budget", initial: math.MaxInt64, bytes: 0, want: math.MaxInt64},
		{name: "one byte cannot set zero", initial: math.MaxInt64, bytes: 1, want: math.MaxInt64},
		{name: "large budget does not overflow", initial: math.MaxInt64, bytes: math.MaxUint64, want: math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			var policy memoryLimitPolicy
			current, sets := test.initial, 0
			policy.apply(test.bytes, test.env, func(limit int64) int64 {
				previous := current
				if limit >= 0 {
					current = limit
					sets++
				}
				return previous
			})
			if current != test.want || sets != test.sets {
				t.Fatalf("limit=%d, sets=%d; want %d, %d", current, sets, test.want, test.sets)
			}
		})
	}
}

func TestMemoryLimitPolicyRefreshAndExternalOverride(t *testing.T) {
	var policy memoryLimitPolicy
	current, sets := int64(math.MaxInt64), 0
	setLimit := func(limit int64) int64 {
		previous := current
		if limit >= 0 {
			current = limit
			sets++
		}
		return previous
	}
	policy.apply(512<<20, "", setLimit)
	policy.apply(512<<20, "", setLimit)
	if sets != 1 {
		t.Fatalf("unchanged budget reapplied: %d writes", sets)
	}
	policy.apply(256<<20, "", setLimit)
	if current != (256<<20)*4/5 || sets != 2 {
		t.Fatal("new cgroup budget did not update the default limit")
	}
	current = 64 << 20 // another component takes ownership
	policy.apply(1<<30, "", setLimit)
	if current != 64<<20 || sets != 2 {
		t.Fatal("automatic default overwrote an external caller")
	}
	current = int64(math.MaxInt64)
	policy.apply(1<<30, "", setLimit)
	if current != math.MaxInt64 || sets != 2 {
		t.Fatal("automatic default resumed after external ownership")
	}
}
