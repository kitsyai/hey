//go:build linux

package sysmem

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func available() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("sysmem: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			break
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("sysmem: MemAvailable not found in /proc/meminfo")
}
