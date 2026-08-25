//go:build linux

// xfs-frag-exporter issues XFS_IOC_GETFSMAP against every mounted XFS filesystem
// on the node and exposes free-space fragmentation metrics for Prometheus. See
// xfs-frag-exporter-SPEC.md.
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"xfs-frag-exporter/frag"
)

func main() {
	addr := env("LISTEN_ADDR", ":9101")
	hostRoot := env("HOST_ROOT", "/host")
	interval := envDuration("SCRAPE_INTERVAL", 300*time.Second)

	c := &collector{hostRoot: hostRoot, node: env("NODE_NAME", "")}
	c.refresh() // populate before serving so /metrics has data immediately

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			c.refresh()
		}
	}()

	http.HandleFunc("/metrics", c.handleMetrics)
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "xfs-frag-exporter\nsee /metrics\n")
	})

	log.Printf("listening on %s (node %q, host root %q, interval %s)", addr, c.node, hostRoot, interval)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// mountResult is one XFS mount's collection outcome for the current snapshot.
type mountResult struct {
	mount    xfsMount
	stats    frag.Stats
	geom     XFSGeometry
	duration float64 // seconds spent in the GETFSMAP walk
	success  bool
}

type collector struct {
	hostRoot string
	node     string // node name (downward-API NODE_NAME); labels every series
	mu       sync.RWMutex
	snapshot []mountResult
}

// refresh re-discovers XFS mounts and re-walks each, replacing the snapshot.
func (c *collector) refresh() {
	mounts, err := discoverXFSMounts(c.hostRoot)
	if err != nil {
		log.Printf("discover mounts: %v", err)
		return
	}
	results := make([]mountResult, 0, len(mounts))
	for _, m := range mounts {
		start := time.Now()
		ext, err := collectFreeExtents(m.openPaths)
		r := mountResult{mount: m, duration: time.Since(start).Seconds()}
		if err != nil {
			log.Printf("collect %s: %v", m.mountpoint, err)
		} else {
			r.stats = frag.Aggregate(ext)
			r.success = true
		}
		if geom, err := collectGeometry(m.openPaths); err != nil {
			log.Printf("geometry %s: %v", m.mountpoint, err)
		} else {
			r.geom = geom
		}
		results = append(results, r)
	}
	c.mu.Lock()
	c.snapshot = results
	c.mu.Unlock()
}

func (c *collector) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	c.mu.RLock()
	snap := c.snapshot
	c.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	writeExposition(bw, c.node, snap)
}

// writeExposition renders the snapshot in the Prometheus text format. Value
// gauges are emitted only for mounts whose walk succeeded; success/duration are
// always emitted so a failing mount is still visible.
func writeExposition(w io.Writer, node string, snap []mountResult) {
	gauge(w, "xfs_free_extent_avg_bytes",
		"Mean contiguous free-extent size; below ~64 KiB (16 blocks) XFS can ENOSPC with free space left (RHEL-82924). Mean over bytes — a few large extents keep it high, so pair it with xfs_free_extents_small.",
		node, snap, func(r mountResult) (float64, bool) { return r.stats.AvgExtentBytes(), r.success })

	gauge(w, "xfs_free_extents_small",
		"Free extents smaller than 64 KiB (16 blocks). As a fraction of xfs_free_extents this is the field-validated RHEL-82924 signal (live-incident nodes: 95-98%); it catches heavy-tail fragmentation the byte-mean xfs_free_extent_avg_bytes misses.",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.stats.SmallExtents), r.success })

	gauge(w, "xfs_free_extents_tiny",
		"Free extents smaller than 8 KiB (2 blocks). These cannot satisfy even a sparse inode cluster allocation (sparse=1 needs 2 contiguous blocks). When this approaches xfs_free_extents, creat() ENOSPC is imminent regardless of sparse inode support.",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.stats.TinyExtents), r.success })

	gauge(w, "xfs_frag_density_extents_per_gib",
		"Free extents per GiB of free space; the exact reciprocal of avg extent size (2^30/avg).",
		node, snap, func(r mountResult) (float64, bool) { return r.stats.Density(), r.success })

	gauge(w, "xfs_free_extent_max_bytes",
		"Largest contiguous free extent in bytes (gates large allocation; ceilinged at agsize).",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.stats.MaxBytes), r.success })

	gauge(w, "xfs_free_extents",
		"Number of free extents.",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.stats.Extents), r.success })

	gauge(w, "xfs_free_bytes",
		"Total free space in bytes (sum of free extents).",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.stats.FreeBytes), r.success })

	gauge(w, "xfs_free_extents_tiny",
		"Free extents smaller than 8 KiB (2 blocks). Cannot satisfy even a sparse inode cluster allocation. When this approaches xfs_free_extents, creat() ENOSPC is imminent regardless of sparse inode support.",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.stats.TinyExtents), r.success })

	gauge(w, "xfs_agsize_bytes",
		"Allocation group size in bytes (from XFS_IOC_FSGEOMETRY). The hard ceiling on any single free extent; used to evaluate xfs_free_extent_max_bytes.",
		node, snap, func(r mountResult) (float64, bool) { return float64(r.geom.AGSizeBytes()), r.geom.Valid() })

	gauge(w, "xfs_sparse_inodes_enabled",
		"1 if sparse inodes are enabled (sparse=1), 0 if not. sparse=1 lowers inode chunk allocation from 8 to 2 contiguous blocks but does NOT prevent ENOSPC under severe fragmentation.",
		node, snap, func(r mountResult) (float64, bool) { return b2f(r.geom.SparseInodes()), r.geom.Valid() })

	gauge(w, "xfs_freesp_scrape_duration_seconds",
		"Seconds spent in the GETFSMAP walk.",
		node, snap, func(r mountResult) (float64, bool) { return r.duration, true })

	gauge(w, "xfs_freesp_scrape_success",
		"1 if the last GETFSMAP walk succeeded, else 0.",
		node, snap, func(r mountResult) (float64, bool) { return b2f(r.success), true })
}

// gauge writes one gauge metric family: the HELP/TYPE header plus one sample per
// mount for which val reports ok.
func gauge(w io.Writer, name, help, node string, snap []mountResult, val func(mountResult) (float64, bool)) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, r := range snap {
		v, ok := val(r)
		if !ok {
			continue
		}
		fmt.Fprintf(w, "%s{%s} %s\n", name, r.labels(node), formatFloat(v))
	}
}

// labels renders the Prometheus label set shared by every series.
func (r mountResult) labels(node string) string {
	return fmt.Sprintf(`node="%s",mountpoint="%s",device="%s"`,
		escapeLabel(node), escapeLabel(r.mount.mountpoint), escapeLabel(r.mount.device))
}

// escapeLabel escapes a Prometheus label value: backslash, double-quote, newline.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("invalid %s=%q, using %s", key, v, def)
	}
	return def
}
