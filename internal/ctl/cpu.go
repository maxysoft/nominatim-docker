package ctl

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// AvailableCPUs returns the number of CPUs this container may actually use.
//
// runtime.NumCPU (like nproc) honours the CPU affinity mask but is blind to the
// CFS quota, so `--cpus=2` on a 64-core host reports 64. Sizing osm2pgsql
// threads and Gunicorn workers off that number oversubscribes the container and
// exhausts the database connection limit.
func AvailableCPUs() int {
	n := runtime.NumCPU()
	if q := cgroupCPUQuota(); q > 0 && q < n {
		n = q
	}
	if n < 1 {
		n = 1
	}
	return n
}

// cgroupCPUQuota returns the CFS quota rounded up to whole CPUs, or 0 when no
// quota is set.
func cgroupCPUQuota() int {
	// cgroup v2: "<quota> <period>", or "max <period>" when unlimited.
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		return parseCPUMax(string(b))
	}
	// cgroup v1: quota and period live in separate files, quota -1 = unlimited.
	quota, err1 := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period, err2 := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int(math.Ceil(float64(quota) / float64(period)))
}

func parseCPUMax(s string) int {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.Atoi(f[0])
	period, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int(math.Ceil(float64(quota) / float64(period)))
}

func readIntFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
