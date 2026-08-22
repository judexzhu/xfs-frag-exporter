//go:build linux

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sort"
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
	// FMR_OWN_FREE. The docs define it as FMR_OWNER('X',1) = 0x5800000001, but the
	// running kernel reports the bare code (0x1). Match the low 32 bits so both
	// encodings work.
	fmrOwnFreeCode = 1
	ownerCodeMask  = 0xffffffff
	recsPerCall    = 512
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

// collectFreeExtents issues GETFSMAP against an XFS filesystem and returns the
// byte length of every free extent. GETFSMAP reports the whole filesystem
// regardless of which mount is opened, so it tries the candidate paths (all bind
// mounts of the same device) and uses the first that opens — some host mounts
// (e.g. /var) may be unreadable to the container under SELinux while another
// (e.g. /etc) is fine. It walks the device in batches, advancing the low key
// past the last record each round until FMR_OF_LAST.
func collectFreeExtents(paths []string) ([]uint64, error) {
	fd, path := -1, ""
	var openErr error
	for _, p := range paths {
		f, err := syscall.Open(p, syscall.O_RDONLY, 0)
		if err == nil {
			fd, path = f, p
			break
		}
		openErr = err
	}
	if fd < 0 {
		return nil, fmt.Errorf("open (tried %v): %w", paths, openErr)
	}
	defer syscall.Close(fd)

	buf := make([]byte, headSize+recsPerCall*recSize)
	head := (*fsmapHead)(unsafe.Pointer(&buf[0]))
	recs := unsafe.Slice((*fsmap)(unsafe.Pointer(&buf[headSize])), recsPerCall)

	// High key: match everything up to the end of every device. Do NOT set
	// fmr_flags here — the kernel validates key flags against FMR_OF_ALL.
	head.Keys[1] = fsmap{
		Device:   ^uint32(0),
		Physical: ^uint64(0),
		Owner:    ^uint64(0),
		Offset:   ^uint64(0),
	}

	debug := os.Getenv("DEBUG_GETFSMAP") != ""
	var out []uint64
	var raw uint64
	var sampleFlags uint32
	var sampleOwner uint64
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
			if raw == 0 {
				sampleFlags, sampleOwner = r.Flags, r.Owner
			}
			raw++
			if r.Flags&fmrOfSpecialOwner != 0 && r.Owner&ownerCodeMask == fmrOwnFreeCode {
				out = append(out, r.Length)
			}
		}
		last := recs[n-1]
		if last.Flags&fmrOfLast != 0 {
			break
		}
		head.Keys[0] = last // continue after the last record
	}
	if debug {
		log.Printf("GETFSMAP %s: raw=%d free=%d sampleFlags=%#x sampleOwner=%#x", path, raw, len(out), sampleFlags, sampleOwner)
	}
	return out, nil
}

// xfsMount is one discovered XFS filesystem.
type xfsMount struct {
	mountpoint string   // host-absolute path; used as the metric label
	device     string   // backing device; used as the metric label
	openPaths  []string // candidate paths to open; any works (GETFSMAP is whole-fs)
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

	// One entry per filesystem (maj:min): the same XFS is bind-mounted at many
	// paths, all reporting the same free space. Collect every path as an open
	// candidate and keep the best label.
	byDev := map[string]*xfsMount{}
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
		label := strings.TrimPrefix(openPath, hostRoot)
		if label == "" {
			label = "/"
		}
		majmin := left[2]
		m := byDev[majmin]
		if m == nil {
			m = &xfsMount{device: unescapeMountinfo(right[1])}
			byDev[majmin] = m
		}
		m.openPaths = append(m.openPaths, openPath)
		if m.mountpoint == "" || preferLabel(label, m.mountpoint) {
			m.mountpoint = label
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	mounts := make([]xfsMount, 0, len(byDev))
	for _, m := range byDev {
		sort.Strings(m.openPaths) // stable, and shorter/likelier-readable paths first
		mounts = append(mounts, *m)
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].mountpoint < mounts[j].mountpoint })
	return mounts, nil
}

// preferLabel reports whether label a is a better representative than b for the
// same filesystem (one XFS is bind-mounted at many paths): "/var" wins — it's
// where container churn lands — then the shorter path.
func preferLabel(a, b string) bool {
	if (a == "/var") != (b == "/var") {
		return a == "/var"
	}
	return len(a) < len(b)
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
