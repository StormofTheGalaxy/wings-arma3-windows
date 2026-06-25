//go:build windows

package windows

import (
	"runtime"
	"time"
	"unsafe"

	"github.com/pterodactyl/wings/environment"
	winapi "golang.org/x/sys/windows"
)

type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type networkCounters struct {
	rx uint64
	tx uint64
}

type mibIfTable2 struct {
	numEntries uint32
	table      [1]winapi.MibIfRow2
}

var (
	psapiDLL             = winapi.NewLazySystemDLL("psapi.dll")
	getProcessMemoryInfo = psapiDLL.NewProc("GetProcessMemoryInfo")
	iphlpapiDLL          = winapi.NewLazySystemDLL("iphlpapi.dll")
	getIfTable2          = iphlpapiDLL.NewProc("GetIfTable2")
)

func (e *Environment) resourceStats(prevCPU uint64, prevAt time.Time, netBaseline networkCounters) (environment.Stats, uint64, time.Time) {
	pids := e.processTreePIDs()
	now := time.Now()
	var memory uint64
	var cpuTime uint64
	for _, pid := range pids {
		mem, cpu := processUsage(pid)
		memory += mem
		cpuTime += cpu
	}

	stats := environment.Stats{Memory: memory}
	if !prevAt.IsZero() && now.After(prevAt) && cpuTime >= prevCPU {
		elapsed := float64(now.Sub(prevAt).Nanoseconds() / 100)
		if elapsed > 0 {
			stats.CpuAbsolute = (float64(cpuTime-prevCPU) / elapsed) * 100 / float64(runtime.NumCPU())
		}
	}

	if counters, err := currentNetworkCounters(); err == nil {
		if counters.rx >= netBaseline.rx {
			stats.Network.RxBytes = counters.rx - netBaseline.rx
		}
		if counters.tx >= netBaseline.tx {
			stats.Network.TxBytes = counters.tx - netBaseline.tx
		}
	}

	return stats, cpuTime, now
}

func (e *Environment) processTreePIDs() []uint32 {
	e.mu.RLock()
	var roots []uint32
	if e.cmd != nil && e.cmd.Process != nil {
		roots = append(roots, uint32(e.cmd.Process.Pid))
	}
	for _, cmd := range e.hc {
		if cmd != nil && cmd.Process != nil {
			roots = append(roots, uint32(cmd.Process.Pid))
		}
	}
	e.mu.RUnlock()
	if len(roots) == 0 {
		return nil
	}

	snapshot, err := winapi.CreateToolhelp32Snapshot(winapi.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return roots
	}
	defer winapi.CloseHandle(snapshot)

	children := make(map[uint32][]uint32)
	var entry winapi.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := winapi.Process32First(snapshot, &entry); err == nil {
		for {
			children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
			if err := winapi.Process32Next(snapshot, &entry); err != nil {
				break
			}
		}
	}

	seen := make(map[uint32]struct{})
	queue := append([]uint32(nil), roots...)
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		queue = append(queue, children[pid]...)
	}

	out := make([]uint32, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	return out
}

func processUsage(pid uint32) (uint64, uint64) {
	h, err := winapi.OpenProcess(winapi.PROCESS_QUERY_LIMITED_INFORMATION|winapi.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, 0
	}
	defer winapi.CloseHandle(h)

	var mem processMemoryCounters
	mem.Cb = uint32(unsafe.Sizeof(mem))
	r1, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mem)), uintptr(mem.Cb))
	var memory uint64
	if r1 != 0 {
		memory = uint64(mem.WorkingSetSize)
	}

	var creation, exit, kernel, user winapi.Filetime
	if err := winapi.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return memory, 0
	}
	return memory, filetimeToUint64(kernel) + filetimeToUint64(user)
}

func filetimeToUint64(ft winapi.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func currentNetworkCounters() (networkCounters, error) {
	var table *mibIfTable2
	r1, _, err := getIfTable2.Call(uintptr(unsafe.Pointer(&table)))
	if r1 != 0 {
		return networkCounters{}, err
	}
	defer winapi.FreeMibTable(unsafe.Pointer(table))

	var counters networkCounters
	rows := unsafe.Slice(&table.table[0], int(table.numEntries))
	for _, row := range rows {
		if row.OperStatus != winapi.IfOperStatusUp {
			continue
		}
		counters.rx += row.InOctets
		counters.tx += row.OutOctets
	}
	return counters, nil
}
