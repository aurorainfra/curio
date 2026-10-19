package nodediag

import (
	"context"
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

// SAS topology and SES enclosure (JBOD) health from sysfs: HBAs, expanders,
// phys with link rates and error counters, enclosure slots and the disks in
// them. All of it is readable without root. Detailed enclosure element
// status (PSUs, fans, temperatures) comes from sg_ses when allowed by sudo.

type SASHost struct {
	Name     string
	Driver   string `json:",omitempty"`
	Board    string `json:",omitempty"`
	Firmware string `json:",omitempty"`
	BIOS     string `json:",omitempty"`
	Phys     int
	Disks    int
}

type SASExpander struct {
	Name     string
	Parent   string
	Vendor   string `json:",omitempty"`
	Product  string `json:",omitempty"`
	Revision string `json:",omitempty"`
	Level    string `json:",omitempty"`
	Phys     int
	Disks    int
}

type PhyCounters struct {
	InvalidDword  int64
	Disparity     int64
	LossOfSync    int64
	ResetProblems int64
}

type SASPhy struct {
	Name           string
	Parent         string
	SASAddress     string `json:",omitempty"`
	Attached       string `json:",omitempty"`
	NegotiatedRate string `json:",omitempty"`
	MaxRate        string `json:",omitempty"`
	Counters       PhyCounters
	// Delta is the counter change since the previous sas call on this node.
	Delta    *PhyCounters `json:",omitempty"`
	DeltaS   int64        `json:",omitempty"`
	Problems []string     `json:",omitempty"`
}

type EnclosureSlot struct {
	Name   string
	Slot   string `json:",omitempty"`
	Type   string `json:",omitempty"`
	Status string `json:",omitempty"`
	Fault  bool   `json:",omitempty"`
	Locate bool   `json:",omitempty"`
	Active bool   `json:",omitempty"`
	Disk   string `json:",omitempty"`
}

type Enclosure struct {
	Name       string
	ID         string `json:",omitempty"`
	Vendor     string `json:",omitempty"`
	Model      string `json:",omitempty"`
	Revision   string `json:",omitempty"`
	SGDevice   string `json:",omitempty"`
	Host       string `json:",omitempty"`
	Components int
	Populated  int
	Slots      []EnclosureSlot `json:",omitempty"`
	// SES is sg_ses output (enclosure status page: PSUs, fans, temperature
	// sensors, element faults) when requested and allowed.
	SES      *CommandResult `json:",omitempty"`
	Problems []string       `json:",omitempty"`
}

type SASRequest struct {
	// ProblemsOnly omits healthy phys and slots.
	ProblemsOnly bool `json:",omitempty"`
	// SES runs sg_ses on each enclosure for element status (needs sudo).
	SES bool `json:",omitempty"`
}

type SASResponse struct {
	Hosts      []SASHost
	Expanders  []SASExpander `json:",omitempty"`
	Enclosures []Enclosure   `json:",omitempty"`
	Phys       []SASPhy      `json:",omitempty"`
	PhyCount   int
	Problems   []string `json:",omitempty"`
}

var (
	phyNameRe   = regexp.MustCompile(`^phy-\d+:(\d+:)?\d+$`)
	sgNameRe    = regexp.MustCompile(`^sg\d+$`)
	endDeviceRe = regexp.MustCompile(`/(end_device-[0-9:]+)/`)
)

type phySnapshot struct {
	at       time.Time
	counters map[string]PhyCounters
}

type sasState struct {
	mu   sync.Mutex
	prev *phySnapshot
}

func attrInt(p string) int64 {
	n, _ := strconv.ParseInt(readAttr(p), 10, 64)
	return n
}

func classDir(class string) string { return filepath.Join(sysRoot, "class", class) }

func listClass(class string) []string {
	ents, err := os.ReadDir(classDir(class))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// parentOf names the nearest host or expander above a sysfs device path.
func parentOf(real string) string {
	parts := strings.Split(real, "/")
	for i := len(parts) - 2; i >= 0; i-- {
		p := parts[i]
		if strings.HasPrefix(p, "expander-") || (strings.HasPrefix(p, "host") && !strings.Contains(p, ":")) {
			return p
		}
	}
	return ""
}

type slotRef struct{ enclosure, slot string }

// enclosureSlotsByDisk maps disk names to their SES enclosure slot.
func enclosureSlotsByDisk() map[string]slotRef {
	out := map[string]slotRef{}
	for _, enc := range listClass("enclosure") {
		for _, sl := range enclosureSlots(enc) {
			if sl.Disk != "" {
				out[sl.Disk] = slotRef{enc, sl.Name}
			}
		}
	}
	return out
}

func enclosureSlots(enc string) []EnclosureSlot {
	dir := filepath.Join(classDir("enclosure"), enc)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []EnclosureSlot
	for _, e := range ents {
		cdir := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(cdir, "status")); err != nil {
			continue
		}
		sl := EnclosureSlot{
			Name:   e.Name(),
			Slot:   readAttr(filepath.Join(cdir, "slot")),
			Type:   readAttr(filepath.Join(cdir, "type")),
			Status: readAttr(filepath.Join(cdir, "status")),
			Fault:  readAttr(filepath.Join(cdir, "fault")) == "1",
			Locate: readAttr(filepath.Join(cdir, "locate")) == "1",
			Active: readAttr(filepath.Join(cdir, "active")) == "1",
		}
		if blk, err := os.ReadDir(filepath.Join(cdir, "device", "block")); err == nil && len(blk) > 0 {
			sl.Disk = blk[0].Name()
		}
		out = append(out, sl)
	}
	sort.Slice(out, func(a, b int) bool {
		na, ea := strconv.Atoi(out[a].Slot)
		nb, eb := strconv.Atoi(out[b].Slot)
		if ea == nil && eb == nil && na != nb {
			return na < nb
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// badSlotStatus reports SES element statuses that need attention.
func badSlotStatus(s string) bool {
	switch strings.ToLower(s) {
	case "", "ok", "not installed", "unknown", "unsupported", "not available", "no access allowed":
		return false
	}
	return true
}

func (st *sasState) report(ctx context.Context, req SASRequest) (*SASResponse, error) {
	resp := &SASResponse{}
	disks, _ := listDisks()
	diskByEndDev := map[string]string{}
	diskParent := map[string]string{}
	for _, d := range disks {
		if m := endDeviceRe.FindStringSubmatch(d.sysPath); m != nil {
			diskByEndDev[m[1]] = d.Name
		}
		diskParent[d.Name] = parentOf(d.sysPath)
	}
	countDisks := func(parent string) int {
		n := 0
		for _, p := range diskParent {
			if p == parent {
				n++
			}
		}
		return n
	}

	for _, h := range listClass("sas_host") {
		sh := filepath.Join(classDir("scsi_host"), h)
		resp.Hosts = append(resp.Hosts, SASHost{
			Name:     h,
			Driver:   readAttr(filepath.Join(sh, "proc_name")),
			Board:    readAttr(filepath.Join(sh, "board_name")),
			Firmware: readAttr(filepath.Join(sh, "version_fw")),
			BIOS:     readAttr(filepath.Join(sh, "version_bios")),
			Disks:    countDisks(h),
		})
	}
	hostIdx := map[string]int{}
	for i, h := range resp.Hosts {
		hostIdx[h.Name] = i
	}

	for _, x := range listClass("sas_expander") {
		dir := filepath.Join(classDir("sas_expander"), x)
		real, _ := filepath.EvalSymlinks(filepath.Join(dir, "device"))
		resp.Expanders = append(resp.Expanders, SASExpander{
			Name:     x,
			Parent:   parentOf(real),
			Vendor:   readAttr(filepath.Join(dir, "vendor_id")),
			Product:  readAttr(filepath.Join(dir, "product_id")),
			Revision: readAttr(filepath.Join(dir, "product_rev")),
			Level:    readAttr(filepath.Join(dir, "level")),
			Disks:    countDisks(x),
		})
	}
	expIdx := map[string]int{}
	for i, e := range resp.Expanders {
		expIdx[e.Name] = i
	}

	// what each port attaches to: port dir lists its phys and child device
	attached := map[string]string{}
	for _, p := range listClass("sas_port") {
		pdir, err := filepath.EvalSymlinks(filepath.Join(classDir("sas_port"), p, "device"))
		if err != nil {
			continue
		}
		ents, _ := os.ReadDir(pdir)
		var phys []string
		var child string
		for _, e := range ents {
			n := e.Name()
			switch {
			case phyNameRe.MatchString(n):
				phys = append(phys, n)
			case strings.HasPrefix(n, "end_device-"):
				child = n
				if d, ok := diskByEndDev[n]; ok {
					child = d
				}
			case strings.HasPrefix(n, "expander-"):
				child = n
			}
		}
		for _, ph := range phys {
			attached[ph] = child
		}
	}

	now := time.Now()
	st.mu.Lock()
	prev := st.prev
	snap := &phySnapshot{at: now, counters: map[string]PhyCounters{}}
	st.mu.Unlock()

	for _, p := range listClass("sas_phy") {
		dir := filepath.Join(classDir("sas_phy"), p)
		real, _ := filepath.EvalSymlinks(filepath.Join(dir, "device"))
		phy := SASPhy{
			Name:           p,
			Parent:         parentOf(real + "/x"),
			SASAddress:     readAttr(filepath.Join(dir, "sas_address")),
			Attached:       attached[p],
			NegotiatedRate: readAttr(filepath.Join(dir, "negotiated_linkrate")),
			MaxRate:        readAttr(filepath.Join(dir, "maximum_linkrate")),
			Counters: PhyCounters{
				InvalidDword:  attrInt(filepath.Join(dir, "invalid_dword_count")),
				Disparity:     attrInt(filepath.Join(dir, "running_disparity_error_count")),
				LossOfSync:    attrInt(filepath.Join(dir, "loss_of_dword_sync_count")),
				ResetProblems: attrInt(filepath.Join(dir, "phy_reset_problem_count")),
			},
		}
		snap.counters[p] = phy.Counters
		if prev != nil {
			if pc, ok := prev.counters[p]; ok {
				phy.Delta = &PhyCounters{
					InvalidDword:  phy.Counters.InvalidDword - pc.InvalidDword,
					Disparity:     phy.Counters.Disparity - pc.Disparity,
					LossOfSync:    phy.Counters.LossOfSync - pc.LossOfSync,
					ResetProblems: phy.Counters.ResetProblems - pc.ResetProblems,
				}
				phy.DeltaS = int64(now.Sub(prev.at).Seconds())
			}
		}
		c := phy.Counters
		if c.InvalidDword+c.Disparity+c.LossOfSync+c.ResetProblems > 0 {
			phy.Problems = append(phy.Problems, fmt.Sprintf("link errors: invalid dword %d, disparity %d, loss of sync %d, reset problems %d", c.InvalidDword, c.Disparity, c.LossOfSync, c.ResetProblems))
		}
		if phy.Delta != nil && phy.Delta.InvalidDword+phy.Delta.Disparity+phy.Delta.LossOfSync+phy.Delta.ResetProblems > 0 {
			phy.Problems = append(phy.Problems, fmt.Sprintf("link errors still increasing (+%d invalid dword, +%d disparity, +%d loss of sync, +%d reset problems in %ds)",
				phy.Delta.InvalidDword, phy.Delta.Disparity, phy.Delta.LossOfSync, phy.Delta.ResetProblems, phy.DeltaS))
		}
		if phy.Attached != "" && phy.NegotiatedRate != "" && phy.MaxRate != "" && phy.NegotiatedRate != phy.MaxRate &&
			!strings.Contains(phy.NegotiatedRate, "Unknown") && rateGbit(phy.NegotiatedRate) < rateGbit(phy.MaxRate) && rateGbit(phy.NegotiatedRate) > 0 {
			phy.Problems = append(phy.Problems, fmt.Sprintf("negotiated %s below max %s", phy.NegotiatedRate, phy.MaxRate))
		}
		if phy.Attached != "" && strings.Contains(strings.ToLower(phy.NegotiatedRate), "disabled") {
			phy.Problems = append(phy.Problems, "phy disabled with a device attached")
		}
		if i, ok := hostIdx[phy.Parent]; ok {
			resp.Hosts[i].Phys++
		}
		if i, ok := expIdx[phy.Parent]; ok {
			resp.Expanders[i].Phys++
		}
		resp.PhyCount++
		for _, pr := range phy.Problems {
			resp.Problems = append(resp.Problems, fmt.Sprintf("%s (%s -> %s): %s", phy.Name, phy.Parent, phy.Attached, pr))
		}
		if !req.ProblemsOnly || len(phy.Problems) > 0 {
			resp.Phys = append(resp.Phys, phy)
		}
	}
	st.mu.Lock()
	st.prev = snap
	st.mu.Unlock()

	for _, enc := range listClass("enclosure") {
		dir := filepath.Join(classDir("enclosure"), enc)
		dev := filepath.Join(dir, "device")
		e := Enclosure{
			Name:       enc,
			ID:         readAttr(filepath.Join(dir, "id")),
			Vendor:     readAttr(filepath.Join(dev, "vendor")),
			Model:      readAttr(filepath.Join(dev, "model")),
			Revision:   readAttr(filepath.Join(dev, "rev")),
			Components: int(attrInt(filepath.Join(dir, "components"))),
		}
		if real, err := filepath.EvalSymlinks(dev); err == nil {
			if m := hostRe.FindStringSubmatch(real + "/"); m != nil {
				e.Host = m[1]
			}
		}
		if sg, err := os.ReadDir(filepath.Join(dev, "scsi_generic")); err == nil && len(sg) > 0 && sgNameRe.MatchString(sg[0].Name()) {
			e.SGDevice = sg[0].Name()
		}
		for _, sl := range enclosureSlots(enc) {
			if sl.Disk != "" {
				e.Populated++
			}
			var pr []string
			if badSlotStatus(sl.Status) {
				pr = append(pr, "status "+sl.Status)
			}
			if sl.Fault {
				pr = append(pr, "fault indicator on")
			}
			for _, p := range pr {
				e.Problems = append(e.Problems, fmt.Sprintf("slot %s (%s): %s", sl.Name, sl.Disk, p))
			}
			if !req.ProblemsOnly || len(pr) > 0 {
				e.Slots = append(e.Slots, sl)
			}
		}
		if req.SES && e.SGDevice != "" {
			r := runCmd(ctx, true, "sg_ses", "--page=2", "/dev/"+e.SGDevice)
			e.SES = &r
		}
		for _, p := range e.Problems {
			resp.Problems = append(resp.Problems, enc+": "+p)
		}
		resp.Enclosures = append(resp.Enclosures, e)
	}
	return resp, nil
}

// rateGbit parses "12.0 Gbit" style link rates.
func rateGbit(s string) float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
