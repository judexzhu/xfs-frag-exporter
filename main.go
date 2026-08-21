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

	c := &collector{hostRoot: hostRoot}
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

	log.Printf("listening on %s (host root %q, interval %s)", addr, hostRoot, interval)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// mountResult is one XFS mount's collection outcome for the current snapshot.
type mountResult struct {
	mount    xfsMount
	stats    frag.Stats
	duration float64 // seconds spent in the GETFSMAP walk
	success  bool
}

type collector struct {
	hostRoot string
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
		ext, err := collectFreeExtents(m.openPath)
		r := mountResult{mount: m, duration: time.Since(start).Seconds()}
		if err != nil {
			log.Printf("collect %s: %v", m.openPath, err)
		} else {
			r.stats = frag.Aggregate(ext)
			r.success = true
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
	writeExposition(bw, snap)
}

// writeExposition renders the snapshot in the Prometheus text format. Value
// gauges are emitted only for mounts whose walk succeeded; success/duration are
// always emitted so a failing mount is still visible.
func writeExposition(w io.Writer, snap []mountResult) {
	gauge(w, "xfs_frag_density_extents_per_gib",
		"Free extents per GiB of free space; its rate of change identifies sick nodes.",
		snap, func(r mountResult) (float64, bool) { return r.stats.Density(), r.success })

	gauge(w, "xfs_free_extent_max_bytes",
		"Largest contiguous free extent in bytes (gates large allocation; ceilinged at agsize).",
		snap, func(r mountResult) (float64, bool) { return float64(r.stats.MaxBytes), r.success })

	gauge(w, "xfs_free_extents",
		"Number of free extents.",
		snap, func(r mountResult) (float64, bool) { return float64(r.stats.Extents), r.success })

	gauge(w, "xfs_free_bytes",
		"Total free space in bytes (sum of free extents).",
		snap, func(r mountResult) (float64, bool) { return float64(r.stats.FreeBytes), r.success })

	gauge(w, "xfs_freesp_scrape_duration_seconds",
		"Seconds spent in the GETFSMAP walk.",
		snap, func(r mountResult) (float64, bool) { return r.duration, true })

	gauge(w, "xfs_freesp_scrape_success",
		"1 if the last GETFSMAP walk succeeded, else 0.",
		snap, func(r mountResult) (float64, bool) { return b2f(r.success), true })
}

// gauge writes one gauge metric family: the HELP/TYPE header plus one sample per
// mount for which val reports ok.
func gauge(w io.Writer, name, help string, snap []mountResult, val func(mountResult) (float64, bool)) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	for _, r := range snap {
		v, ok := val(r)
		if !ok {
			continue
		}
		fmt.Fprintf(w, "%s{mountpoint=\"%s\",device=\"%s\"} %s\n",
			name, escapeLabel(r.mount.mountpoint), escapeLabel(r.mount.device), formatFloat(v))
	}
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
