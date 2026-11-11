//go:build linux

package memutils

import (
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
)

type memoryCgroup struct {
	directory string
	mount     string
	version   int
}

func effectiveMemory(host memorySnapshot) memorySnapshot {
	return cgroupMemory(host, readMemoryFile)
}

// Bound reads of procfs metadata and small numeric controller files. These
// reads occur on the monitor, never on the cached getter path.
func readMemoryFile(filename string) ([]byte, error) {
	maxBytes := int64(128)
	if filename == "/proc/self/cgroup" || filename == "/proc/self/mountinfo" {
		maxBytes = 1 << 20
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("memory metadata %q exceeds %d bytes", filename, maxBytes)
	}
	return data, nil
}

func cgroupMemory(host memorySnapshot, readFile func(string) ([]byte, error)) memorySnapshot {
	cgroups, err := readFile("/proc/self/cgroup")
	if err != nil {
		return host
	}
	mounts, err := readFile("/proc/self/mountinfo")
	if err != nil {
		return host
	}

	visited := map[string]bool{}
	for _, group := range memoryCgroups(string(cgroups), string(mounts)) {
		limitName, usageName := "memory.max", "memory.current"
		if group.version == 1 {
			limitName, usageName = "memory.limit_in_bytes", "memory.usage_in_bytes"
		}
		// All ancestors within the visible mount can restrict a child. Shared
		// ancestors' usage includes sibling workloads, which affects headroom.
		for directory := group.directory; ; directory = path.Dir(directory) {
			limitFile := path.Join(directory, limitName)
			if !visited[limitFile] {
				visited[limitFile] = true
				if limit, ok := readMemoryNumber(limitFile, readFile); ok && limit > 0 &&
					(group.version != 1 || limit < uint64(math.MaxInt64)-(1<<20)) {
					if host.totalBytes == 0 || limit < host.totalBytes {
						host.totalBytes = limit
					}
					if usage, ok := readMemoryNumber(path.Join(directory, usageName), readFile); ok {
						headroom := uint64(0)
						if usage < limit {
							headroom = limit - usage
						}
						if headroom < host.availableBytes {
							host.availableBytes = headroom
						}
					}
				}
			}
			if directory == group.mount {
				break
			}
		}
	}
	if host.availableBytes > host.totalBytes {
		host.availableBytes = host.totalBytes
	}
	return host
}

func readMemoryNumber(filename string, readFile func(string) ([]byte, error)) (uint64, bool) {
	data, err := readFile(filename)
	if err != nil {
		return 0, false
	}
	number, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return number, err == nil
}

var mountPathUnescaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func memoryCgroups(cgroups, mounts string) []memoryCgroup {
	groupPaths := map[int]string{}
	for _, line := range strings.Split(cgroups, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || !validMemoryPath(fields[2]) {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			groupPaths[2] = fields[2]
		} else if hasMemoryController(fields[1]) {
			groupPaths[1] = fields[2]
		}
	}

	var result []memoryCgroup
	for _, line := range strings.Split(mounts, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields, fs := strings.Fields(before), strings.Fields(after)
		if len(fields) < 6 || len(fs) < 3 {
			continue
		}
		version := 0
		if fs[0] == "cgroup2" {
			version = 2
		} else if fs[0] == "cgroup" && hasMemoryController(fs[2]) {
			version = 1
		} else {
			continue
		}
		groupPath, ok := groupPaths[version]
		if !ok {
			continue
		}
		root, mount := mountPathUnescaper.Replace(fields[3]), mountPathUnescaper.Replace(fields[4])
		if !validMemoryPath(root) || !validMemoryPath(mount) {
			continue
		}
		mount = path.Clean(mount)
		var relative string
		if root == "/" {
			relative = strings.TrimPrefix(groupPath, "/")
		} else if groupPath == root {
			relative = ""
		} else if strings.HasPrefix(groupPath, root+"/") {
			relative = strings.TrimPrefix(groupPath, root+"/")
		} else {
			continue
		}
		// A process outside its cgroup namespace may have leading /.. in
		// procfs. Strip the mount root before cleaning, and never escape the
		// visible mount by treating an unreachable group as its root.
		if hasParentPathSegment(relative) {
			continue
		}
		result = append(result, memoryCgroup{directory: path.Join(mount, relative), mount: mount, version: version})
	}
	return result
}

func hasParentPathSegment(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func validMemoryPath(value string) bool {
	return strings.HasPrefix(value, "/") && len(value) <= 4096 && !strings.ContainsRune(value, '\x00')
}

func hasMemoryController(controllers string) bool {
	for _, controller := range strings.Split(controllers, ",") {
		if controller == "memory" {
			return true
		}
	}
	return false
}
