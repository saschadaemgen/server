//go:build linux

package sysmetrics

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// platformMetrics is the candidate metric set on Linux. Probe keeps only
// the ones whose reader succeeds on this host, so a machine without a
// thermal zone simply does not offer CPU-temp. Readers are pure standard
// library over /proc, /sys and statfs - general Linux, not RPi-specific.
func platformMetrics() []metricDef {
	return []metricDef{
		{addr: "cpu_temp", label: "CPU-Temperatur", unit: "°C", read: readCPUTemp},
		{addr: "cpu_load", label: "CPU-Last (1 min)", unit: "", read: readCPULoad},
		{addr: "ram", label: "RAM-Auslastung", unit: "%", read: readRAM},
		{addr: "disk_root", label: "Plattenplatz /", unit: "%", read: readDiskRoot},
	}
}

// readCPUTemp reads the first thermal zone (millidegree Celsius) and
// returns degrees C. Absent on hosts without a thermal zone (VPS).
func readCPUTemp() (float64, error) {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, err
	}
	milli, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	return float64(milli) / 1000.0, nil
}

// readCPULoad returns the 1-minute load average from /proc/loadavg.
func readCPULoad() (float64, error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, errors.New("sysmetrics: empty /proc/loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// readRAM returns used RAM as a percentage, from /proc/meminfo
// (MemTotal - MemAvailable) / MemTotal.
func readRAM() (float64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var total, avail float64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := meminfoKB(line, "MemTotal:"); ok {
			total, haveTotal = v, true
		} else if v, ok := meminfoKB(line, "MemAvailable:"); ok {
			avail, haveAvail = v, true
		}
	}
	if !haveTotal || !haveAvail || total == 0 {
		return 0, errors.New("sysmetrics: incomplete /proc/meminfo")
	}
	return (1 - avail/total) * 100, nil
}

// meminfoKB parses the kB value of a "Key: <n> kB" /proc/meminfo line.
func meminfoKB(line, key string) (float64, bool) {
	if !strings.HasPrefix(line, key) {
		return 0, false
	}
	fields := strings.Fields(line[len(key):])
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	return v, err == nil
}

// readDiskRoot returns used space on / as a percentage, via statfs.
func readDiskRoot() (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, err
	}
	total := float64(st.Blocks)
	if total == 0 {
		return 0, errors.New("sysmetrics: statfs reports zero blocks")
	}
	avail := float64(st.Bavail)
	return (1 - avail/total) * 100, nil
}
