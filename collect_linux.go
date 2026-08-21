//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// GETFSMAP ioctl. See docs/adr/0002. Struct layout from include/uapi/linux/fsmap.h,
// ABI-frozen since Linux 4.12. No CAP_SYS_ADMIN is needed for free space: the
// kernel selects the unprivileged bnobt backend, which reports FMR_OWN_FREE.
const (
	fsIocGetFsMap     = 0xc0c0583b
	fmrOfSpecialOwner = 0x10
	fmrOfLast         = 0x20
	fmrOwnFree        = uint64('X')<<32 | 1 // FMR_OWNER('X', 1) = 0x5800000001
	recsPerCall       = 512
)

type fsmap struct {
	Device   uint32
	Flags    uint32
	Physical uint64
	Owner    uint64
	Offset   uint64
	Length   uint64
	Reserved [3]uint64
}

type fsmapHead struct {
	IFlags   uint32
	OFlags   uint32
	Count    uint32
	Entries  uint32
	Reserved [6]uint64
	Keys     [2]fsmap // low key [0], high key [1]; records follow in the buffer
}

const (
	headSize = int(unsafe.Sizeof(fsmapHead{})) // 192
	recSize  = int(unsafe.Sizeof(fsmap{}))     // 64
)

// collectFreeExtents issues GETFSMAP against a mounted XFS path and returns the
// byte length of every free extent. It walks the whole device in batches,
// advancing the low key past the last record each round until FMR_OF_LAST.
func collectFreeExtents(path string) ([]uint64, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer syscall.Close(fd)

	buf := make([]byte, headSize+recsPerCall*recSize)
	head := (*fsmapHead)(unsafe.Pointer(&buf[0]))
	recs := unsafe.Slice((*fsmap)(unsafe.Pointer(&buf[headSize])), recsPerCall)

	// High key: match everything up to the end of every device.
	head.Keys[1] = fsmap{
		Device:   ^uint32(0),
		Flags:    ^uint32(0),
		Physical: ^uint64(0),
		Owner:    ^uint64(0),
		Offset:   ^uint64(0),
	}

	var out []uint64
	for {
		head.Count = recsPerCall
		head.Entries = 0
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
			uintptr(fsIocGetFsMap), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
			return nil, fmt.Errorf("GETFSMAP ioctl on %s: %w", path, errno)
		}
		n := int(head.Entries)
		if n == 0 {
			break
		}
		for i := 0; i < n; i++ {
			r := recs[i]
			if r.Flags&fmrOfSpecialOwner != 0 && r.Owner == fmrOwnFree {
				out = append(out, r.Length)
			}
		}
		last := recs[n-1]
		if last.Flags&fmrOfLast != 0 {
			break
		}
		head.Keys[0] = last // continue after the last record
	}
	return out, nil
}

// xfsMount is one discovered XFS filesystem.
type xfsMount struct {
	mountpoint string // host-absolute path; used as the metric label
	device     string // backing device; used as the metric label
	openPath   string // path to open from inside the container (hostRoot + mountpoint)
}

// discoverXFSMounts returns one entry per host XFS filesystem. With the node
// root bind-mounted at hostRoot (mountPropagation HostToContainer), the host's
// filesystems appear in our OWN mountinfo under hostRoot (e.g. /host/var) — which
// is always readable, unlike the host pid-1 mountinfo a non-root pod may not
// access. Free space is a property of the filesystem, not the mount, so we dedupe
// by device (maj:min): RHCOS binds one XFS at /, /var, /etc, /sysroot and many
// kubelet subpaths — all the same free space.
func discoverXFSMounts(hostRoot string) ([]xfsMount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	defer f.Close()

	var mounts []xfsMount
	seenDev := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// mountinfo: "<id> <pid> <maj:min> <root> <mountpoint> ... - <fstype> <source> <opts>"
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+len(" - "):])
		if len(left) < 5 || len(right) < 2 || right[0] != "xfs" {
			continue
		}
		openPath := unescapeMountinfo(left[4])
		// Only host filesystems (under hostRoot); skip our own container mounts
		// like /etc/hosts and /dev/termination-log.
		if openPath != hostRoot && !strings.HasPrefix(openPath, hostRoot+"/") {
			continue
		}
		majmin := left[2]
		if seenDev[majmin] {
			continue // same filesystem, same free space
		}
		seenDev[majmin] = true

		label := strings.TrimPrefix(openPath, hostRoot)
		if label == "" {
			label = "/"
		}
		mounts = append(mounts, xfsMount{
			mountpoint: label, // host-absolute path
			device:     unescapeMountinfo(right[1]),
			openPath:   openPath, // already under hostRoot
		})
	}
	return mounts, sc.Err()
}

// unescapeMountinfo decodes the octal escapes the kernel uses in mountinfo for
// space (\040), tab (\011), newline (\012) and backslash (\134).
func unescapeMountinfo(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) &&
			s[i+1] >= '0' && s[i+1] <= '7' &&
			s[i+2] >= '0' && s[i+2] <= '7' &&
			s[i+3] >= '0' && s[i+3] <= '7' {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
