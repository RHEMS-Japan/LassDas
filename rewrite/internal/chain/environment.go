package chain

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func cgroupFacts(root, membership string) (string, string) {
	memory, cpu, group := "unknown", "unknown", ""
	for _, line := range strings.Split(membership, "\n") {
		if strings.HasPrefix(line, "0::") {
			group = strings.TrimPrefix(line, "0::")
			break
		}
	}
	// Only the standard visible v2 mount is observed. Never guess a path for
	// v1, an unavailable mount, or a cgroup outside this namespace.
	if !strings.HasPrefix(group, "/") || path.Clean(group) != group {
		return memory, cpu
	}
	directory := filepath.Join(root, strings.TrimPrefix(group, "/"))
	if data, err := os.ReadFile(filepath.Join(directory, "memory.max")); err == nil {
		value := strings.TrimSpace(string(data))
		if value == "max" {
			memory = "none set"
		} else if bytes, err := strconv.ParseUint(value, 10, 64); err == nil && bytes%(1<<20) == 0 {
			memory = fmt.Sprintf("%d MiB", bytes>>20)
		} else if err == nil {
			memory = fmt.Sprintf("%d bytes", bytes)
		}
	}
	if data, err := os.ReadFile(filepath.Join(directory, "cpu.max")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) == 2 {
			period, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil && period > 0 {
				if fields[0] == "max" {
					cpu = "none set"
				} else if quota, err := strconv.ParseUint(fields[0], 10, 64); err == nil && quota > 0 {
					cpu = fmt.Sprintf("%g CPUs", float64(quota)/float64(period))
				}
			}
		}
	}
	return memory, cpu
}

// ExecutionEnvironment describes observations, never a feasibility verdict.
// timeLimit is the sentence about the request's saved active-work cap.
func ExecutionEnvironment(timeLimit string) string {
	membership, _ := os.ReadFile("/proc/self/cgroup")
	memory, cpu := cgroupFacts("/sys/fs/cgroup", string(membership))
	return fmt.Sprintf("Execution environment facts (observed when this run started; not a verdict on whether the work fits):\n"+
		"Memory limit: %s (memory.max of the controller's own cgroup v2; free memory and the limits of enclosing cgroups are not measured).\n"+
		"CPUs: %d logical CPUs available; CPU quota of that cgroup: %s.\n%s\n"+
		"Every local role runs inside these same limits together with the controller, with no separate allowance per role; limits of remote services are unknown. "+
		"Where the role launcher's memory guard runs, it stops the largest role process before the limit is reached (by default once less than an eighth of the limit, and at least 512 MiB, is left) and names the stopped process on standard error; "+
		"without it, going over the memory limit can stop the controller and every role at once.\n",
		memory, runtime.NumCPU(), cpu, timeLimit)
}
