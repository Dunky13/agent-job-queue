//go:build darwin && cgo

package backend

/*
#cgo LDFLAGS: -lproc
#include <stdlib.h>
#include <stdint.h>
#include <sys/types.h>
#include <sys/sysctl.h>
#include <sys/statvfs.h>
#include <libproc.h>
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <mach/host_info.h>
#include <mach/vm_statistics.h>

static int queue_host_cpu(uint32_t *ticks) {
 mach_msg_type_number_t count = HOST_CPU_LOAD_INFO_COUNT;
 host_cpu_load_info_data_t cpu;
 if (host_statistics(mach_host_self(), HOST_CPU_LOAD_INFO, (host_info_t)&cpu, &count) != KERN_SUCCESS || count != HOST_CPU_LOAD_INFO_COUNT) return -1;
 for (int i=0;i<CPU_STATE_MAX;i++) ticks[i]=cpu.cpu_ticks[i];
 return 0;
}
static int queue_host_memory(uint64_t *values) {
 vm_statistics64_data_t vm;
 mach_msg_type_number_t count=HOST_VM_INFO64_COUNT;
 vm_size_t page;
 if (host_page_size(mach_host_self(),&page)!=KERN_SUCCESS || host_statistics64(mach_host_self(),HOST_VM_INFO64,(host_info64_t)&vm,&count)!=KERN_SUCCESS) return -1;
 if (count < HOST_VM_INFO64_COUNT) return -1;
 values[0]=page;values[1]=vm.free_count;values[2]=vm.inactive_count;values[3]=vm.swapins;values[4]=vm.swapouts;
 return 0;
}
static int queue_process_usage(int pid, uint64_t *values) {
 struct rusage_info_v2 usage;
 if (proc_pid_rusage(pid,RUSAGE_INFO_V2,(rusage_info_t *)&usage)!=0) return -1;
 values[0]=usage.ri_proc_start_abstime;values[1]=usage.ri_proc_exit_abstime;values[2]=usage.ri_phys_footprint;
 values[3]=usage.ri_user_time+usage.ri_system_time;values[4]=usage.ri_diskio_bytesread;values[5]=usage.ri_diskio_byteswritten;
 return 0;
}
static int queue_swap_used(uint64_t *used) {
 struct xsw_usage swap;size_t size=sizeof(swap);
 if (sysctlbyname("vm.swapusage",&swap,&size,NULL,0)!=0 || size!=sizeof(swap)) return -1;
 *used=swap.xsu_used;return 0;
}
static int queue_disk_free(const char *path,uint64_t *value) {
 struct statvfs disk;if(statvfs(path,&disk)!=0)return -1;
 *value=(uint64_t)disk.f_bavail*disk.f_frsize;return 0;
}
*/
import "C"
import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"syscall"
	"unsafe"
)

type darwinResourceBackend struct {
	diskPath, machine string
	total, cores      uint64
}

func resourceSysctl(name string) ([]byte, error) {
	key := C.CString(name)
	defer C.free(unsafe.Pointer(key))
	var size C.size_t
	if C.sysctlbyname(key, nil, &size, nil, 0) != 0 || size == 0 || size > 4096 {
		return nil, fmt.Errorf("A required host resource metric is unavailable")
	}
	raw := make([]byte, int(size))
	if C.sysctlbyname(key, unsafe.Pointer(&raw[0]), &size, nil, 0) != 0 {
		return nil, fmt.Errorf("A required host resource metric is unavailable")
	}
	return raw[:int(size)], nil
}
func resourceSysctlInt(name string) (uint64, error) {
	raw, e := resourceSysctl(name)
	if e != nil {
		return 0, e
	}
	switch len(raw) {
	case 4:
		return uint64(binary.LittleEndian.Uint32(raw)), nil
	case 8:
		return binary.LittleEndian.Uint64(raw), nil
	}
	return 0, fmt.Errorf("A host resource counter is invalid")
}
func newPlatformResourceBackend(path string) (ResourceBackend, error) {
	total, e := resourceSysctlInt("hw.memsize")
	if e != nil {
		return nil, e
	}
	cores, e := resourceSysctlInt("hw.logicalcpu")
	if e != nil {
		return nil, e
	}
	model, e := resourceSysctl("hw.model")
	if e != nil {
		return nil, e
	}
	machine, e := resourceSysctl("hw.machine")
	if e != nil {
		return nil, e
	}
	if total == 0 || cores == 0 {
		return nil, fmt.Errorf("Host resource capacity is invalid")
	}
	capacity := bytes.Join([][]byte{model, machine, []byte(strconv.FormatUint(total, 10)), []byte(strconv.FormatUint(cores, 10))}, []byte{0})
	hash := sha256.Sum256(capacity)
	return &darwinResourceBackend{path, hex.EncodeToString(hash[:])[:24], total, cores}, nil
}
func (b *darwinResourceBackend) HostCounters() (Object, error) {
	var ticks [4]C.uint32_t
	if C.queue_host_cpu(&ticks[0]) != 0 {
		return nil, fmt.Errorf("Host CPU counters are unavailable")
	}
	var vm [5]C.uint64_t
	if C.queue_host_memory(&vm[0]) != 0 || vm[0] == 0 {
		return nil, fmt.Errorf("Host memory counters are unavailable")
	}
	var used C.uint64_t
	if C.queue_swap_used(&used) != 0 {
		return nil, fmt.Errorf("Host swap usage is invalid")
	}
	pressure, e := resourceSysctlInt("kern.memorystatus_vm_pressure_level")
	if e != nil {
		return nil, e
	}
	level := map[uint64]string{1: "normal", 2: "warning", 4: "critical"}[pressure]
	if level == "" {
		return nil, fmt.Errorf("Host memory pressure is unavailable")
	}
	path := C.CString(b.diskPath)
	defer C.free(unsafe.Pointer(path))
	var free C.uint64_t
	if C.queue_disk_free(path, &free) != 0 {
		return nil, fmt.Errorf("Available disk capacity is unavailable")
	}
	available := min(b.total, uint64(vm[1]+vm[2])*uint64(vm[0]))
	return Object{"totalMemoryBytes": b.total, "availableMemoryBytes": available, "cpuCores": b.cores, "cpuTicks": []any{uint64(ticks[0]), uint64(ticks[1]), uint64(ticks[2]), uint64(ticks[3])}, "memoryPressure": level, "swapUsedBytes": uint64(used), "swapInBytes": uint64(vm[3]) * uint64(vm[0]), "swapOutBytes": uint64(vm[4]) * uint64(vm[0]), "diskFreeBytes": uint64(free), "machine": b.machine}, nil
}
func (b *darwinResourceBackend) Members(pgid int) ([]Object, error) {
	return resourceProcessListing(pgid)
}
func (b *darwinResourceBackend) Process(pid, pgid int) (Object, error) {
	group, e := syscall.Getpgid(pid)
	if e != nil || group != pgid {
		return nil, fmt.Errorf("A process resource identity is unavailable")
	}
	var values [6]C.uint64_t
	if C.queue_process_usage(C.int(pid), &values[0]) != 0 {
		return nil, fmt.Errorf("A process resource counter is unavailable")
	}
	group, e = syscall.Getpgid(pid)
	if e != nil || group != pgid || values[0] == 0 || values[1] != 0 {
		return nil, fmt.Errorf("A process resource identity changed")
	}
	return Object{"start": uint64(values[0]), "memoryBytes": uint64(values[2]), "cpuNanoseconds": uint64(values[3]), "readBytes": uint64(values[4]), "writeBytes": uint64(values[5])}, nil
}
