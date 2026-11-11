//go:build linux

package memutils

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCgroupMemory(t *testing.T) {
	const v2Mount = "30 20 0:28 / /cg rw - cgroup2 cgroup rw\n"
	const v1Mount = "30 20 0:28 / /cg rw - cgroup cgroup rw,memory\n"
	host := memorySnapshot{totalBytes: 8 << 30, availableBytes: 6 << 30}
	for _, test := range []struct {
		name       string
		membership string
		mounts     string
		files      map[string]string
		want       memorySnapshot
	}{
		{
			name: "v2 exact sub-GiB budget", membership: "0::/pod/node\n", mounts: v2Mount,
			files: map[string]string{"/cg/pod/node/memory.max": "536870912\n", "/cg/pod/node/memory.current": "134217728\n"},
			want:  memorySnapshot{totalBytes: 512 << 20, availableBytes: 384 << 20},
		},
		{
			name: "v2 unlimited child inherits ancestor", membership: "0::/pod/node\n", mounts: v2Mount,
			files: map[string]string{
				"/cg/pod/node/memory.max": "max\n", "/cg/pod/memory.max": "1073741824",
				"/cg/pod/memory.current": "805306368", "/cg/memory.max": "max",
			},
			want: memorySnapshot{totalBytes: 1 << 30, availableBytes: 256 << 20},
		},
		{
			name: "sibling usage at ancestor limits available", membership: "0::/pod/node\n", mounts: v2Mount,
			files: map[string]string{
				"/cg/pod/node/memory.max": "536870912", "/cg/pod/node/memory.current": "134217728",
				"/cg/pod/memory.max": "1073741824", "/cg/pod/memory.current": "1006632960",
			},
			want: memorySnapshot{totalBytes: 512 << 20, availableBytes: 64 << 20},
		},
		{
			name: "host remains tighter than cgroup", membership: "0::/node\n", mounts: v2Mount,
			files: map[string]string{"/cg/node/memory.max": "17179869184", "/cg/node/memory.current": "0"},
			want:  host,
		},
		{
			name: "equal usage has zero available", membership: "0::/node\n", mounts: v2Mount,
			files: map[string]string{"/cg/node/memory.max": "536870912", "/cg/node/memory.current": "536870912"},
			want:  memorySnapshot{totalBytes: 512 << 20},
		},
		{
			name: "usage over limit cannot wrap", membership: "0::/node\n", mounts: v2Mount,
			files: map[string]string{"/cg/node/memory.max": "536870912", "/cg/node/memory.current": "1073741824"},
			want:  memorySnapshot{totalBytes: 512 << 20},
		},
		{
			name: "bad usage preserves limit", membership: "0::/node\n", mounts: v2Mount,
			files: map[string]string{"/cg/node/memory.max": "536870912", "/cg/node/memory.current": "invalid"},
			want:  memorySnapshot{totalBytes: 512 << 20, availableBytes: 512 << 20},
		},
		{
			name: "v1 memory controller in mixed membership", membership: "7:cpu,cpuacct:/unrelated\n5:memory,hugetlb:/node\n0::/unified\n", mounts: v1Mount,
			files: map[string]string{"/cg/node/memory.limit_in_bytes": "536870912", "/cg/node/memory.usage_in_bytes": "134217728"},
			want:  memorySnapshot{totalBytes: 512 << 20, availableBytes: 384 << 20},
		},
		{
			name: "v1 unlimited sentinel ignored", membership: "5:memory:/node\n", mounts: v1Mount,
			files: map[string]string{"/cg/node/memory.limit_in_bytes": "9223372036854771712", "/cg/node/memory.usage_in_bytes": "9223372036854771712"},
			want:  host,
		},
		{
			name: "v1 ancestor budget", membership: "5:memory:/pod/node\n", mounts: v1Mount,
			files: map[string]string{
				"/cg/pod/node/memory.limit_in_bytes": "9223372036854771712",
				"/cg/pod/memory.limit_in_bytes":      "536870912", "/cg/pod/memory.usage_in_bytes": "268435456",
			},
			want: memorySnapshot{totalBytes: 512 << 20, availableBytes: 256 << 20},
		},
		{
			name: "bind mount rooted above current group", membership: "0::/tenant/node\n",
			mounts: "30 20 0:28 /tenant /cg ro - cgroup2 cgroup rw\n",
			files:  map[string]string{"/cg/node/memory.max": "536870912", "/cg/node/memory.current": "134217728"},
			want:   memorySnapshot{totalBytes: 512 << 20, availableBytes: 384 << 20},
		},
		{
			name: "namespace current group is mount root", membership: "0::/\n", mounts: v2Mount,
			files: map[string]string{"/cg/memory.max": "536870912", "/cg/memory.current": "134217728"},
			want:  memorySnapshot{totalBytes: 512 << 20, availableBytes: 384 << 20},
		},
		{
			name: "escaped mount root and mountpoint", membership: "0::/tenant one/node\n",
			mounts: "30 20 0:28 /tenant\\040one /cg\\040memory rw - cgroup2 cgroup rw\n",
			files:  map[string]string{"/cg memory/node/memory.max": "536870912", "/cg memory/node/memory.current": "134217728"},
			want:   memorySnapshot{totalBytes: 512 << 20, availableBytes: 384 << 20},
		},
		{
			name: "no path-prefix confusion", membership: "0::/tenant-other/node\n",
			mounts: "30 20 0:28 /tenant /cg rw - cgroup2 cgroup rw\n",
			files:  map[string]string{"/cg/node/memory.max": "536870912"}, want: host,
		},
		{
			name: "outside namespace and outside visible mount", membership: "0::/../../other\n", mounts: v2Mount,
			files: map[string]string{"/cg/other/memory.max": "536870912"}, want: host,
		},
		{
			name: "outside namespace but visible mount", membership: "0::/../../other\n",
			mounts: "30 20 0:28 /../.. /cg rw - cgroup2 cgroup rw\n",
			files:  map[string]string{"/cg/other/memory.max": "536870912", "/cg/other/memory.current": "134217728"},
			want:   memorySnapshot{totalBytes: 512 << 20, availableBytes: 384 << 20},
		},
		{
			name: "unrelated controller is ignored", membership: "5:nomemory:/node\n", mounts: v1Mount,
			files: map[string]string{"/cg/node/memory.limit_in_bytes": "536870912"}, want: host,
		},
		{
			name: "memory high alone is not a hard limit", membership: "0::/node\n", mounts: v2Mount,
			files: map[string]string{"/cg/node/memory.max": "max", "/cg/node/memory.high": "536870912"}, want: host,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := test.files
			files["/proc/self/cgroup"], files["/proc/self/mountinfo"] = test.membership, test.mounts
			got := cgroupMemory(host, memoryFixture(files))
			if got != test.want {
				t.Fatalf("memory=%+v, want %+v", got, test.want)
			}
		})
	}
}

func TestCgroupMemoryInvalidLimitFailsSoft(t *testing.T) {
	host := memorySnapshot{totalBytes: 8 << 30, availableBytes: 4 << 30}
	for _, limit := range []string{"", "max", "0", "-1", "invalid", "18446744073709551616"} {
		t.Run(fmt.Sprintf("limit=%q", limit), func(t *testing.T) {
			got := cgroupMemory(host, memoryFixture(map[string]string{
				"/proc/self/cgroup": "0::/node\n", "/proc/self/mountinfo": "30 20 0:28 / /cg rw - cgroup2 cgroup rw\n",
				"/cg/node/memory.max": limit, "/cg/node/memory.current": "0",
			}))
			if got != host {
				t.Fatalf("invalid or unlimited memory must preserve host budget: %+v", got)
			}
		})
	}
	if got := cgroupMemory(host, memoryFixture(nil)); got != host {
		t.Fatal("missing proc metadata did not fall back to host")
	}
}

func TestCgroupMemoryDynamicChanges(t *testing.T) {
	files := map[string]string{
		"/proc/self/cgroup": "0::/node\n", "/proc/self/mountinfo": "30 20 0:28 / /cg rw - cgroup2 cgroup rw\n",
		"/cg/node/memory.max": "1073741824", "/cg/node/memory.current": "134217728",
	}
	host := memorySnapshot{totalBytes: 8 << 30, availableBytes: 6 << 30}
	if got := cgroupMemory(host, memoryFixture(files)); got.totalBytes != 1<<30 {
		t.Fatalf("unexpected initial cgroup total: %+v", got)
	}
	files["/cg/node/memory.max"] = "536870912"
	files["/cg/node/memory.current"] = "268435456"
	if got := cgroupMemory(host, memoryFixture(files)); got.totalBytes != 512<<20 || got.availableBytes != 256<<20 {
		t.Fatalf("changed quota was not read: %+v", got)
	}
	files["/proc/self/cgroup"] = "0::/next\n"
	files["/cg/next/memory.max"], files["/cg/next/memory.current"] = "268435456", "134217728"
	if got := cgroupMemory(host, memoryFixture(files)); got.totalBytes != 256<<20 || got.availableBytes != 128<<20 {
		t.Fatalf("cgroup migration was not read: %+v", got)
	}
}

func TestCgroupMemoryDoesNotWalkAboveMount(t *testing.T) {
	files := map[string]string{
		"/proc/self/cgroup": "0::/tenant/node\n", "/proc/self/mountinfo": "30 20 0:28 /tenant /cg rw - cgroup2 cgroup rw\n",
		"/cg/node/memory.max": "max", "/cg/memory.max": "max", "/memory.max": "1",
	}
	readFile := memoryFixture(files)
	_ = cgroupMemory(memorySnapshot{totalBytes: 8 << 30, availableBytes: 4 << 30}, func(filename string) ([]byte, error) {
		if filename == "/memory.max" {
			t.Error("walk escaped the visible cgroup mount")
		}
		return readFile(filename)
	})
}

func TestReadMemoryFileBound(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(filename, []byte(strings.Repeat("1", 129)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMemoryFile(filename); err == nil {
		t.Fatal("oversized numeric controller file was accepted")
	}
}

func memoryFixture(files map[string]string) func(string) ([]byte, error) {
	return func(filename string) ([]byte, error) {
		if data, ok := files[filename]; ok {
			return []byte(data), nil
		}
		return nil, os.ErrNotExist
	}
}
