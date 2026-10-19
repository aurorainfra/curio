package nodediag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Disk inventory and SMART health, built for machines with hundreds of
// drives: one compact line per disk with a health verdict, mapped to mounts,
// storage paths and enclosure slots. SMART data is cached and refreshed in
// parallel in the background.

const (
	smartTTL         = 15 * time.Minute
	smartParallel    = 8
	smartCmdTimeout  = 60 * time.Second
	defaultDiskLimit = 500
	maxDiskLimit     = 5000
)

// physical disk names diagnostics run on
var physDiskName = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+)$`)

// SmartSummary is the health-relevant subset of smartctl's JSON output.
type SmartSummary struct {
	// Verdict: ok, warn, fail or unknown.
	Verdict   string
	Reasons   []string `json:",omitempty"`
	CheckedAt time.Time
	Passed    *bool  `json:",omitempty"`
	TempC     *int64 `json:",omitempty"`
	PowerOnH  *int64 `json:",omitempty"`
	// ATA
	Reallocated   *int64 `json:",omitempty"`
	Pending       *int64 `json:",omitempty"`
	Uncorrectable *int64 `json:",omitempty"`
	CRCErrors     *int64 `json:",omitempty"`
	// SCSI/SAS
	GrownDefects      *int64 `json:",omitempty"`
	ReadUncorrected   *int64 `json:",omitempty"`
	WriteUncorrected  *int64 `json:",omitempty"`
	VerifyUncorrected *int64 `json:",omitempty"`
	NonMediumErrors   *int64 `json:",omitempty"`
	// NVMe
	CriticalWarning *int64 `json:",omitempty"`
	MediaErrors     *int64 `json:",omitempty"`
	PercentUsed     *int64 `json:",omitempty"`
	AvailableSpare  *int64 `json:",omitempty"`
	ErrorLogEntries *int64 `json:",omitempty"`
	// SmartctlExit is smartctl's exit status bit mask.
	SmartctlExit int    `json:",omitempty"`
	Error        string `json:",omitempty"`
}

type Disk struct {
	Name       string
	Model      string `json:",omitempty"`
	Vendor     string `json:",omitempty"`
	Serial     string `json:",omitempty"`
	WWN        string `json:",omitempty"`
	Firmware   string `json:",omitempty"`
	SizeBytes  int64
	Rotational bool
	// Transport: nvme, sas, sata (ATA behind a SAS HBA/expander or AHCI),
	// virtio or other.
	Transport  string
	HCTL       string `json:",omitempty"`
	Host       string `json:",omitempty"`
	SASAddress string `json:",omitempty"`
	ByPath     string `json:",omitempty"`
	// Enclosure and Slot locate the disk in a SES enclosure (JBOD).
	Enclosure string `json:",omitempty"`
	Slot      string `json:",omitempty"`
	// Mounts and PathIDs use this disk (directly, through partitions, or as
	// a md/dm/LVM member).
	Mounts  []string      `json:",omitempty"`
	PathIDs []string      `json:",omitempty"`
	Smart   *SmartSummary `json:",omitempty"`

	sysPath string
}

// ---- inventory ----

func readAttr(p string) string {
	b, err := readCapped(p, 4096)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var hctlRe = regexp.MustCompile(`/(\d+:\d+:\d+:\d+)/block/`)
var hostRe = regexp.MustCompile(`/(host\d+)/`)

func listDisks() ([]*Disk, error) {
	ents, err := os.ReadDir(filepath.Join(sysRoot, "block"))
	if err != nil {
		return nil, err
	}
	byPath := devLinks("/dev/disk/by-path")
	var out []*Disk
	for _, e := range ents {
		name := e.Name()
		if !physDiskName.MatchString(name) {
			continue
		}
		sp, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "block", name))
		if err != nil {
			continue
		}
		d := &Disk{Name: name, sysPath: sp, ByPath: byPath[name]}
		if sectors, err := strconv.ParseInt(readAttr(filepath.Join(sp, "size")), 10, 64); err == nil {
			d.SizeBytes = sectors * 512
		}
		d.Rotational = readAttr(filepath.Join(sp, "queue/rotational")) == "1"
		dev := filepath.Join(sp, "device")
		d.Model = readAttr(filepath.Join(dev, "model"))
		d.Vendor = readAttr(filepath.Join(dev, "vendor"))
		d.Firmware = firstNonEmpty(readAttr(filepath.Join(dev, "rev")), readAttr(filepath.Join(dev, "firmware_rev")))
		d.Serial = firstNonEmpty(readAttr(filepath.Join(dev, "serial")), vpdSerial(filepath.Join(dev, "vpd_pg80")))
		d.WWN = firstNonEmpty(readAttr(filepath.Join(sp, "wwid")), readAttr(filepath.Join(dev, "wwid")))
		d.SASAddress = readAttr(filepath.Join(dev, "sas_address"))
		if m := hctlRe.FindStringSubmatch(sp); m != nil {
			d.HCTL = m[1]
		}
		if m := hostRe.FindStringSubmatch(sp); m != nil {
			d.Host = m[1]
		}
		switch {
		case strings.HasPrefix(name, "nvme"):
			d.Transport = "nvme"
		case strings.HasPrefix(name, "vd"):
			d.Transport = "virtio"
		case d.Vendor == "ATA":
			d.Transport = "sata"
		case strings.Contains(sp, "/end_device-") || d.SASAddress != "":
			d.Transport = "sas"
		default:
			d.Transport = "other"
		}
		out = append(out, d)
	}
	sort.Slice(out, func(a, b int) bool { return diskLess(out[a].Name, out[b].Name) })
	return out, nil
}

// diskLess orders sda < sdz < sdaa.
func diskLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// vpdSerial decodes the unit serial number VPD page (0x80).
func vpdSerial(p string) string {
	b, err := readCapped(p, 512)
	if err != nil || len(b) < 4 {
		return ""
	}
	n := int(b[2])<<8 | int(b[3])
	if 4+n > len(b) {
		n = len(b) - 4
	}
	return strings.TrimSpace(string(b[4 : 4+n]))
}

// devLinks maps device names to the link names pointing at them in a
// /dev/disk/by-* directory (whole disks only).
func devLinks(dir string) map[string]string {
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), "-part") {
			continue
		}
		t, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		out[filepath.Base(t)] = e.Name()
	}
	return out
}

// mountsByDisk maps each disk to the mount points that live on it (directly,
// through partitions, or as a md/dm/LVM member), and to the local storage
// paths on those mounts. paths maps storage path IDs to their mount points.
func mountsByDisk(paths map[string]string) (map[string][]string, map[string][]string) {
	mounts, _ := readMounts(filepath.Join(procRoot, "self/mountinfo"))
	byDisk := map[string][]string{}
	for _, m := range mounts {
		if !strings.HasPrefix(m.Source, "/dev/") {
			continue
		}
		_, disks := resolveDevices(m.MajorMinor)
		for _, d := range disks {
			byDisk[d] = append(byDisk[d], m.MountPoint)
		}
	}
	pathsByDisk := map[string][]string{}
	for id, mp := range paths {
		for d, mps := range byDisk {
			for _, x := range mps {
				if x == mp {
					pathsByDisk[d] = append(pathsByDisk[d], id)
				}
			}
		}
	}
	for d := range pathsByDisk {
		sort.Strings(pathsByDisk[d])
	}
	return byDisk, pathsByDisk
}

// ---- SMART ----

type smartEntry struct {
	sum     *SmartSummary
	at      time.Time
	running bool
}

type smartCache struct {
	mu      sync.Mutex
	entries map[string]*smartEntry
	sem     chan struct{}
}

func newSmartCache() *smartCache {
	return &smartCache{entries: map[string]*smartEntry{}, sem: make(chan struct{}, smartParallel)}
}

// get returns cached summaries for the disks, starting background refreshes
// for stale ones, and waits for those refreshes until ctx is done. Disks
// still being checked are returned with Verdict "pending".
func (c *smartCache) get(ctx context.Context, disks []*Disk, force bool) {
	var wg sync.WaitGroup
	c.mu.Lock()
	for _, d := range disks {
		e, ok := c.entries[d.Name]
		if !ok {
			e = &smartEntry{}
			c.entries[d.Name] = e
		}
		if e.running || (!force && e.sum != nil && time.Since(e.at) < smartTTL) {
			continue
		}
		e.running = true
		wg.Add(1)
		go func(name string, e *smartEntry) {
			defer wg.Done()
			c.sem <- struct{}{}
			defer func() { <-c.sem }()
			sctx, cancel := context.WithTimeout(context.Background(), smartCmdTimeout)
			sum := runSmart(sctx, name)
			cancel()
			c.mu.Lock()
			e.sum, e.at, e.running = sum, time.Now(), false
			c.mu.Unlock()
		}(d.Name, e)
	}
	c.mu.Unlock()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range disks {
		e := c.entries[d.Name]
		switch {
		case e.sum != nil:
			d.Smart = e.sum
		case e.running:
			d.Smart = &SmartSummary{Verdict: "pending", Reasons: []string{"SMART check still running; call again"}}
		}
	}
}

func runSmart(ctx context.Context, name string) *SmartSummary {
	res := runCmd(ctx, true, "smartctl", "-a", "-j", "/dev/"+name)
	sum := &SmartSummary{Verdict: "unknown", CheckedAt: time.Now()}
	if res.Unavailable != "" {
		sum.Error = res.Unavailable
		return sum
	}
	parseSmart([]byte(res.Output), sum)
	if res.ExitCode&0x3 != 0 && sum.Error == "" {
		sum.Error = fmt.Sprintf("smartctl failed (exit %d)", res.ExitCode)
	}
	return sum
}

// jget walks a decoded JSON document.
func jget(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func jint(v any, path ...string) *int64 {
	switch x := jget(v, path...).(type) {
	case float64:
		n := int64(x)
		return &n
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return nil
		}
		return &n
	}
	return nil
}

func parseSmart(out []byte, s *SmartSummary) {
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		s.Error = "unparseable smartctl output"
		return
	}
	if e := jint(doc, "smartctl", "exit_status"); e != nil {
		s.SmartctlExit = int(*e)
	}
	if p, ok := jget(doc, "smart_status", "passed").(bool); ok {
		s.Passed = &p
	}
	s.TempC = jint(doc, "temperature", "current")
	s.PowerOnH = jint(doc, "power_on_time", "hours")

	if tbl, ok := jget(doc, "ata_smart_attributes", "table").([]any); ok {
		for _, a := range tbl {
			id := jint(a, "id")
			raw := jint(a, "raw", "value")
			if id == nil || raw == nil {
				continue
			}
			switch *id {
			case 5:
				s.Reallocated = raw
			case 197:
				s.Pending = raw
			case 198:
				s.Uncorrectable = raw
			case 199:
				s.CRCErrors = raw
			}
		}
	}

	s.GrownDefects = jint(doc, "scsi_grown_defect_list")
	s.ReadUncorrected = jint(doc, "scsi_error_counter_log", "read", "total_uncorrected_errors")
	s.WriteUncorrected = jint(doc, "scsi_error_counter_log", "write", "total_uncorrected_errors")
	s.VerifyUncorrected = jint(doc, "scsi_error_counter_log", "verify", "total_uncorrected_errors")
	for k := range doc {
		if strings.Contains(k, "non_medium") || strings.Contains(k, "nonmedium") {
			if n := jint(doc, k, "count"); n != nil {
				s.NonMediumErrors = n
			} else if n := jint(doc, k); n != nil {
				s.NonMediumErrors = n
			}
		}
	}
	if s.PercentUsed == nil {
		s.PercentUsed = jint(doc, "scsi_percentage_used_endurance_indicator")
	}

	nv := jget(doc, "nvme_smart_health_information_log")
	if nv != nil {
		s.CriticalWarning = jint(nv, "critical_warning")
		s.MediaErrors = jint(nv, "media_errors")
		s.PercentUsed = jint(nv, "percentage_used")
		s.AvailableSpare = jint(nv, "available_spare")
		s.ErrorLogEntries = jint(nv, "num_err_log_entries")
		if s.TempC == nil {
			s.TempC = jint(nv, "temperature")
		}
	}

	s.Verdict = "ok"
	fail := func(r string) { s.Verdict = "fail"; s.Reasons = append(s.Reasons, r) }
	warn := func(r string) {
		if s.Verdict != "fail" {
			s.Verdict = "warn"
		}
		s.Reasons = append(s.Reasons, r)
	}
	pos := func(v *int64) bool { return v != nil && *v > 0 }

	if s.Passed != nil && !*s.Passed {
		fail("SMART overall health FAILED")
	}
	if s.SmartctlExit&0x08 != 0 {
		fail("smartctl: disk failing")
	}
	if pos(s.CriticalWarning) {
		fail(fmt.Sprintf("NVMe critical warning 0x%x", *s.CriticalWarning))
	}
	if spare := jint(nv, "available_spare"); spare != nil {
		if thr := jint(nv, "available_spare_threshold"); thr != nil && *spare < *thr {
			fail(fmt.Sprintf("available spare %d%% below threshold %d%%", *spare, *thr))
		}
	}
	if s.PercentUsed != nil && *s.PercentUsed >= 100 {
		fail(fmt.Sprintf("endurance used %d%%", *s.PercentUsed))
	} else if s.PercentUsed != nil && *s.PercentUsed >= 90 {
		warn(fmt.Sprintf("endurance used %d%%", *s.PercentUsed))
	}
	for _, c := range []struct {
		v    *int64
		what string
	}{
		{s.Reallocated, "reallocated sectors"},
		{s.Pending, "pending sectors"},
		{s.Uncorrectable, "offline uncorrectable sectors"},
		{s.CRCErrors, "interface CRC errors (cabling/backplane)"},
		{s.GrownDefects, "grown defects"},
		{s.ReadUncorrected, "uncorrected read errors"},
		{s.WriteUncorrected, "uncorrected write errors"},
		{s.VerifyUncorrected, "uncorrected verify errors"},
		{s.MediaErrors, "media errors"},
	} {
		if pos(c.v) {
			warn(fmt.Sprintf("%d %s", *c.v, c.what))
		}
	}
	if s.SmartctlExit&0x10 != 0 {
		warn("prefail attribute at or below threshold")
	}
	if s.SmartctlExit&0x20 != 0 {
		warn("usage attribute was at or below threshold in the past")
	}
	if s.SmartctlExit&0x40 != 0 {
		warn("device error log has errors")
	}
	if s.SmartctlExit&0x80 != 0 {
		warn("self-test log has errors")
	}
	if s.TempC != nil && *s.TempC >= 65 {
		warn(fmt.Sprintf("temperature %d°C", *s.TempC))
	}
	if s.Passed == nil && nv == nil && s.GrownDefects == nil && s.Reallocated == nil {
		s.Verdict = "unknown"
		s.Reasons = append(s.Reasons, "no SMART health data")
	}
}

// ---- ops ----

type DisksRequest struct {
	// Filter selects disks: a disk name, serial, WWN, enclosure:slot, or
	// a storage path ID; empty for all.
	Filter string `json:",omitempty"`
	// Verdicts keeps disks with these SMART verdicts (ok, warn, fail,
	// unknown, pending).
	Verdicts []string `json:",omitempty"`
	Offset   int      `json:",omitempty"`
	Limit    int      `json:",omitempty"`
	// Refresh re-runs smartctl even when cached results are fresh.
	Refresh bool `json:",omitempty"`
	// WaitMS bounds how long to wait for SMART checks.
	WaitMS int `json:",omitempty"`
}

type DisksResponse struct {
	Total     int
	ByVerdict map[string]int
	// Problems lists every disk with a warn/fail verdict, regardless of
	// paging.
	Problems []string `json:",omitempty"`
	Offset   int
	Disks    []*Disk
}

func (h *handler) localPathMounts(ctx context.Context) map[string]string {
	out := map[string]string{}
	if h.d == nil || h.d.LocalStore == nil {
		return out
	}
	locals, err := h.d.LocalStore.Local(ctx)
	if err != nil {
		return out
	}
	mounts, _ := readMounts(filepath.Join(procRoot, "self/mountinfo"))
	for _, l := range locals {
		real, err := filepath.EvalSymlinks(l.LocalPath)
		if err != nil {
			real = l.LocalPath
		}
		if m := mountFor(mounts, real); m != nil {
			out[string(l.ID)] = m.MountPoint
		}
	}
	return out
}

// inventory lists disks with their mounts, paths and enclosure slots.
func (h *handler) inventory(ctx context.Context) ([]*Disk, error) {
	disks, err := listDisks()
	if err != nil {
		return nil, err
	}
	pm := h.localPathMounts(ctx)
	mb, pb := mountsByDisk(pm)
	slots := enclosureSlotsByDisk()
	for _, d := range disks {
		d.Mounts = mb[d.Name]
		d.PathIDs = pb[d.Name]
		if s, ok := slots[d.Name]; ok {
			d.Enclosure, d.Slot = s.enclosure, s.slot
		}
	}
	return disks, nil
}

func (d *Disk) matches(f string) bool {
	if f == "" {
		return true
	}
	if d.Name == f || d.Serial == f || d.WWN == f || d.Enclosure+":"+d.Slot == f || d.SASAddress == f {
		return true
	}
	for _, p := range d.PathIDs {
		if p == f {
			return true
		}
	}
	return false
}

func (h *handler) disks(ctx context.Context, req DisksRequest) (*DisksResponse, error) {
	all, err := h.inventory(ctx)
	if err != nil {
		return nil, err
	}
	var sel []*Disk
	for _, d := range all {
		if d.matches(req.Filter) {
			sel = append(sel, d)
		}
	}
	wait := time.Duration(clampInt(req.WaitMS, 20_000, maxWaitMS)) * time.Millisecond
	wctx, cancel := context.WithTimeout(ctx, wait)
	h.smart.get(wctx, sel, req.Refresh)
	cancel()

	resp := &DisksResponse{ByVerdict: map[string]int{}, Offset: req.Offset}
	keep := map[string]bool{}
	for _, v := range req.Verdicts {
		keep[v] = true
	}
	var filtered []*Disk
	for _, d := range sel {
		v := "unknown"
		if d.Smart != nil {
			v = d.Smart.Verdict
		}
		resp.ByVerdict[v]++
		if v == "warn" || v == "fail" {
			loc := d.Name
			if d.Enclosure != "" {
				loc += " @" + d.Enclosure + ":" + d.Slot
			}
			resp.Problems = append(resp.Problems, fmt.Sprintf("%s %s: %s", loc, v, strings.Join(d.Smart.Reasons, "; ")))
		}
		if len(keep) > 0 && !keep[v] {
			continue
		}
		filtered = append(filtered, d)
	}
	resp.Total = len(filtered)
	limit := clampInt(req.Limit, defaultDiskLimit, maxDiskLimit)
	if req.Offset < len(filtered) {
		resp.Disks = filtered[req.Offset:min(len(filtered), req.Offset+limit)]
	}
	return resp, nil
}

type DiskDetailRequest struct {
	// Disk is a disk name, serial or WWN.
	Disk string
}

type DiskDetailResponse struct {
	Disk     *Disk
	Commands []CommandResult
	Kernel   string `json:",omitempty"`
}

func (h *handler) diskDetail(ctx context.Context, req DiskDetailRequest) (*DiskDetailResponse, error) {
	all, err := h.inventory(ctx)
	if err != nil {
		return nil, err
	}
	var d *Disk
	for _, x := range all {
		if req.Disk != "" && (x.Name == req.Disk || x.Serial == req.Disk || x.WWN == req.Disk) {
			d = x
			break
		}
	}
	if d == nil || !physDiskName.MatchString(d.Name) {
		return nil, badRequest(fmt.Sprintf("no disk %q on this node", req.Disk))
	}
	h.smart.get(ctx, []*Disk{d}, false)
	dev := "/dev/" + d.Name
	resp := &DiskDetailResponse{Disk: d}
	resp.Commands = append(resp.Commands, runCmdOut(ctx, true, &cappedBuffer{max: 1 << 20}, "smartctl", "-x", "-j", dev))
	if strings.HasPrefix(d.Name, "nvme") {
		resp.Commands = append(resp.Commands,
			runCmd(ctx, true, "nvme", "smart-log", "-o", "json", dev),
			runCmd(ctx, true, "nvme", "error-log", "-o", "json", dev))
	}
	resp.Commands = append(resp.Commands, runCmd(ctx, false, "lsblk", "-J", "-O", dev))
	dm := dmesgFor(ctx, []BlockDevice{{Name: d.Name}})
	resp.Commands = append(resp.Commands, dm)
	return resp, nil
}
