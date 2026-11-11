package nodes

import "testing"

func TestApplyMaxCPU(t *testing.T) {
	for _, test := range []struct {
		name        string
		maxCPU      int32
		numCPU      int
		env         string
		want        int
		wantDefault bool
	}{
		{name: "automatic", numCPU: 8, want: 2, wantDefault: true},
		{name: "explicit cap", maxCPU: 3, numCPU: 8, want: 3},
		{name: "cap at physical CPUs", maxCPU: 16, numCPU: 8, want: 8},
		{name: "config overrides environment", maxCPU: 1, numCPU: 8, env: "4", want: 1},
		{name: "restore environment", numCPU: 8, env: "4", want: 4},
		{name: "preserve env above physical", numCPU: 2, env: "4", want: 4},
		{name: "zero env restores automatic", numCPU: 8, env: "0", want: 2, wantDefault: true},
		{name: "invalid env restores automatic", numCPU: 8, env: "garbage", want: 2, wantDefault: true},
		{name: "overflow env restores automatic", numCPU: 8, env: "2147483648", want: 2, wantDefault: true},
		{name: "negative config restores automatic", maxCPU: -1, numCPU: 8, want: 2, wantDefault: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current, defaultCalls, explicitCalls := 8, 0, 0
			got := applyMaxCPU(test.maxCPU, test.numCPU, test.env, func(n int) int {
				previous := current
				if n > 0 {
					current = n
					explicitCalls++
				}
				return previous
			}, func() { current = 2; defaultCalls++ })
			if got != test.want {
				t.Errorf("GOMAXPROCS=%d, want %d", got, test.want)
			}
			if test.wantDefault && (defaultCalls != 1 || explicitCalls != 0) {
				t.Errorf("automatic mode was disabled: default=%d explicit=%d", defaultCalls, explicitCalls)
			}
			if !test.wantDefault && (defaultCalls != 0 || explicitCalls != 1) {
				t.Errorf("explicit mode was ignored: default=%d explicit=%d", defaultCalls, explicitCalls)
			}
		})
	}
}

func TestApplyMaxCPURestoreAutomaticAfterCap(t *testing.T) {
	current, automatic := 8, true
	setProcs := func(n int) int {
		previous := current
		if n > 0 {
			current, automatic = n, false
		}
		return previous
	}
	useDefault := func() { current, automatic = 2, true }
	applyMaxCPU(1, 8, "", setProcs, useDefault)
	if automatic || current != 1 {
		t.Fatal("explicit node cap was not installed")
	}
	applyMaxCPU(0, 8, "", setProcs, useDefault)
	if !automatic || current != 2 {
		t.Fatal("clearing node cap did not restore container-aware runtime updates")
	}
}
