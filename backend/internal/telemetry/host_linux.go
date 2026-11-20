//go:build linux

package telemetry

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// readHost reads machine-wide counters from /proc. With network_mode: host the
// container sees the real NICs, so this is the true wire bandwidth.
func readHost() (h hostRaw, ok bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return h, false
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if skipIface(name) {
			continue
		}
		fl := strings.Fields(rest)
		if len(fl) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fl[0], 10, 64)
		tx, _ := strconv.ParseUint(fl[8], 10, 64)
		h.rx += rx
		h.tx += tx
	}
	f.Close()

	if b, err := os.ReadFile("/proc/stat"); err == nil {
		if line, _, _ := strings.Cut(string(b), "\n"); strings.HasPrefix(line, "cpu ") {
			fl := strings.Fields(line)[1:]
			var total, idle uint64
			for i, v := range fl {
				n, _ := strconv.ParseUint(v, 10, 64)
				if i < 8 { // user nice system idle iowait irq softirq steal
					total += n
				}
				if i == 3 || i == 4 {
					idle += n
				}
			}
			h.cpuTotal, h.cpuIdle = total, idle
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail uint64
		for _, l := range strings.Split(string(b), "\n") {
			fl := strings.Fields(l)
			if len(fl) < 2 {
				continue
			}
			switch fl[0] {
			case "MemTotal:":
				total, _ = strconv.ParseUint(fl[1], 10, 64)
			case "MemAvailable:":
				avail, _ = strconv.ParseUint(fl[1], 10, 64)
			}
		}
		h.memTotal, h.memAvail = total*1024, avail*1024
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fl := strings.Fields(string(b)); len(fl) > 0 {
			h.load1, _ = strconv.ParseFloat(fl[0], 64)
		}
	}
	return h, true
}

func skipIface(n string) bool {
	if n == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "cali", "tun", "tap"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
