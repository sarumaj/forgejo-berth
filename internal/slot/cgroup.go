// SPDX-License-Identifier: GPL-3.0-or-later

package slot

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CPUCounters returns a cgroup's CPU time (cpu.stat usage_usec) and the time some of its
// tasks waited for a CPU (cpu.pressure, some total), in microseconds. Without pressure
// accounting (no cpu.pressure) the pressure is 0.
func CPUCounters(cgroup string) (int64, int64, bool) {
	return CgroupCPU(dirReader(cgroup))
}

// CgroupReader reads one of a cgroup's files by its name.
type CgroupReader func(name string) ([]byte, error)

func dirReader(dir string) CgroupReader {
	return func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
}

// CgroupCPU is CPUCounters of a cgroup read through read.
func CgroupCPU(read CgroupReader) (int64, int64, bool) {
	stat, err := read("cpu.stat")
	if err != nil {
		return 0, 0, false
	}
	usage, ok := field(string(stat), "usage_usec ")
	if !ok {
		return 0, 0, false
	}
	var pressure int64
	if data, err := read("cpu.pressure"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "some ") {
				pressure, _ = field(strings.ReplaceAll(line, " ", "\n"), "total=")
			}
		}
	}
	return usage, pressure, true
}

// field reads the number after prefix at a line's start.
func field(text, prefix string) (int64, bool) {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// PeakMiB is memory.peak plus memory.swap.peak of the cgroup, in MiB rounded up.
func PeakMiB(cgroup string) int {
	return ToMiB(CgroupPeak(dirReader(cgroup)))
}

// CgroupPeak is memory.peak plus memory.swap.peak of a cgroup read through read, in bytes.
func CgroupPeak(read CgroupReader) int64 {
	var bytes int64
	for _, counter := range []string{"memory.peak", "memory.swap.peak"} {
		data, err := read(counter)
		if err != nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			bytes += n
		}
	}
	return bytes
}

// ToMiB rounds bytes up to MiB.
func ToMiB(bytes int64) int {
	return int((bytes + 1<<20 - 1) >> 20)
}

// CgroupScript prints a container's own cgroup files the cycle reads, each after a line
// "@<name>" (a missing one is empty), when run in the container: cgroup v2 with a cgroup
// namespace shows a container its own cgroup at /sys/fs/cgroup.
const CgroupScript = `cd /sys/fs/cgroup 2>/dev/null || exit 0
for f in memory.peak memory.swap.peak cpu.stat cpu.pressure; do echo "@$f"; cat "$f" 2>/dev/null; done`

// CgroupDump reads the output of CgroupScript.
func CgroupDump(out string) CgroupReader {
	files := map[string]string{}
	current := ""
	for _, line := range strings.SplitAfter(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "@"); ok && !strings.Contains(name, " ") {
			current = name
			files[current] = ""
			continue
		}
		if current != "" {
			files[current] += line
		}
	}
	return func(name string) ([]byte, error) {
		text, ok := files[name]
		if !ok || strings.TrimSpace(text) == "" {
			return nil, os.ErrNotExist
		}
		return []byte(text), nil
	}
}
