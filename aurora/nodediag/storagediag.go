package nodediag

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/filecoin-project/curio/lib/storiface"
)

const (
	cmdTimeout     = 20 * time.Second
	maxCmdOutput   = 256 << 10
	maxDmesgLines  = 200
	maxDmesgBytes  = 8 << 20
	maxMetaFile    = 64 << 10
	maxSubdirNames = 2_000_000
)

// root of /proc and /sys, overridden in tests
var (
	procRoot = "/proc"
	sysRoot  = "/sys"
)

// diskName matches the block device names commands may be run on.
var diskName = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|md\d+|dm-\d+)$`)

type StorageDiagRequest struct {
	// PathID is the storage path ID; the path must be attached to this node.
	PathID string
}

type MountInfo struct {
	MountPoint string
	Source     string
	FSType     string
	Options    string
	MajorMinor string
}

type BlockDevice struct {
	Name       string
	Model      string `json:",omitempty"`
	SizeBytes  int64
	Rotational bool
	// Role is "mount" for the device the filesystem is mounted from,
	// "member" for devices behind it (md/dm/LVM members) and "disk" for the
	// whole disks diagnostics were run on.
	Role string
}

type CommandResult struct {
	Command  string
	ExitCode int    `json:",omitempty"`
	Output   string `json:",omitempty"`
	// Unavailable explains why the command was not run or failed to start
	// (missing binary, no sudo rule).
	Unavailable string `json:",omitempty"`
	DurationMS  int64
}

type StatFS struct {
	TotalBytes, FreeBytes, AvailBytes int64
	TotalInodes, FreeInodes           uint64
	ReadOnly                          bool
}

type StorageDiagResponse struct {
	PathID    string
	LocalPath string
	CanSeal   bool
	CanStore  bool
	// SectorStoreJSON is the contents of sectorstore.json.
	SectorStoreJSON string `json:",omitempty"`
	StatFS          *StatFS
	// Entries counts directory entries per sector file type directory.
	Entries  map[string]int
	Mount    *MountInfo
	Devices  []BlockDevice
	MDStat   string `json:",omitempty"`
	Commands []CommandResult
	Errors   []string `json:",omitempty"`
}

func (h *handler) storageDiag(ctx context.Context, req StorageDiagRequest) (*StorageDiagResponse, error) {
	if h.d == nil || h.d.LocalStore == nil {
		return nil, errors.New("no local storage on this node")
	}
	locals, err := h.d.LocalStore.Local(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing local storage: %w", err)
	}
	var sp *storiface.StoragePath
	for i := range locals {
		if string(locals[i].ID) == req.PathID {
			sp = &locals[i]
			break
		}
	}
	if sp == nil {
		return nil, badRequest(fmt.Sprintf("storage path %q is not attached to this node", req.PathID))
	}

	resp := &StorageDiagResponse{
		PathID:    req.PathID,
		LocalPath: sp.LocalPath,
		CanSeal:   sp.CanSeal,
		CanStore:  sp.CanStore,
		Entries:   map[string]int{},
	}
	errf := func(format string, args ...any) {
		resp.Errors = append(resp.Errors, fmt.Sprintf(format, args...))
	}

	if b, err := readCapped(filepath.Join(sp.LocalPath, "sectorstore.json"), maxMetaFile); err != nil {
		errf("reading sectorstore.json: %s", err)
	} else {
		resp.SectorStoreJSON = string(b)
	}

	var sfs unix.Statfs_t
	if err := unix.Statfs(sp.LocalPath, &sfs); err != nil {
		errf("statfs: %s", err)
	} else {
		bs := int64(sfs.Bsize)
		resp.StatFS = &StatFS{
			TotalBytes:  int64(sfs.Blocks) * bs,
			FreeBytes:   int64(sfs.Bfree) * bs,
			AvailBytes:  int64(sfs.Bavail) * bs,
			TotalInodes: sfs.Files,
			FreeInodes:  sfs.Ffree,
			ReadOnly:    sfs.Flags&unix.ST_RDONLY != 0,
		}
	}

	for _, ft := range storiface.PathTypes {
		n, err := countEntries(ctx, filepath.Join(sp.LocalPath, ft.String()))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errf("listing %s: %s", ft.String(), err)
			}
			continue
		}
		resp.Entries[ft.String()] = n
	}

	real, err := filepath.EvalSymlinks(sp.LocalPath)
	if err != nil {
		errf("resolving path: %s", err)
		real = sp.LocalPath
	}
	mi, err := findMount(filepath.Join(procRoot, "self/mountinfo"), real)
	if err != nil {
		errf("finding mount: %s", err)
	}
	resp.Mount = mi

	var disks []string
	if mi != nil {
		resp.Devices, disks = resolveDevices(mi.MajorMinor)
		if len(resp.Devices) == 0 {
			errf("no block device found for %s (%s)", mi.Source, mi.MajorMinor)
		}
	}
	for _, d := range resp.Devices {
		if strings.HasPrefix(d.Name, "md") {
			if b, err := readCapped(filepath.Join(procRoot, "mdstat"), maxMetaFile); err == nil {
				resp.MDStat = string(b)
			}
			break
		}
	}

	if mi != nil {
		resp.Commands = append(resp.Commands,
			runCmd(ctx, false, "df", "-B1", "--output=source,fstype,size,used,avail,pcent,itotal,iused,iavail,ipcent,target", mi.MountPoint),
			runCmd(ctx, false, "findmnt", "-J", "--target", mi.MountPoint),
		)
	}
	for _, disk := range disks {
		dev := "/dev/" + disk
		resp.Commands = append(resp.Commands,
			runCmd(ctx, false, "lsblk", "-J", "-O", dev),
			runCmd(ctx, true, "smartctl", "-a", "-j", dev),
		)
		if strings.HasPrefix(disk, "nvme") {
			resp.Commands = append(resp.Commands, runCmd(ctx, true, "nvme", "smart-log", "-o", "json", dev))
		}
	}
	resp.Commands = append(resp.Commands, dmesgFor(ctx, resp.Devices))

	return resp, nil
}

func readCapped(p string, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, n))
}

func countEntries(ctx context.Context, dir string) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n := 0
	for n < maxSubdirNames {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		names, err := f.Readdirnames(4096)
		n += len(names)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// findMount returns the mount containing path, from a mountinfo file.
func findMount(mountinfo, path string) (*MountInfo, error) {
	f, err := os.Open(mountinfo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var best *MountInfo
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		// 36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw,errors=continue
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		pf := strings.Fields(pre)
		qf := strings.Fields(post)
		if len(pf) < 6 || len(qf) < 2 {
			continue
		}
		mp := unescapeMount(pf[4])
		if !pathWithin(path, mp) {
			continue
		}
		if best == nil || len(mp) >= len(best.MountPoint) {
			best = &MountInfo{MountPoint: mp, MajorMinor: pf[2], Options: pf[5], FSType: qf[0], Source: unescapeMount(qf[1])}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if best == nil {
		return nil, fmt.Errorf("no mount contains %s", path)
	}
	return best, nil
}

func pathWithin(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// unescapeMount decodes the octal escapes (\040 etc.) used in mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// resolveDevices walks sysfs from the device a filesystem is mounted from
// down to the whole disks behind it: partitions to their disk, md/dm devices
// to their member devices. It returns every device seen and the names of the
// whole disks.
func resolveDevices(majorMinor string) ([]BlockDevice, []string) {
	link, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "dev/block", majorMinor))
	if err != nil {
		return nil, nil
	}

	var devs []BlockDevice
	var disks []string
	seen := map[string]bool{}

	var walk func(sysPath, role string, depth int)
	walk = func(sysPath, role string, depth int) {
		name := filepath.Base(sysPath)
		if seen[name] || depth > 8 {
			return
		}
		seen[name] = true
		devs = append(devs, describeDevice(sysPath, name, role))

		// a partition: its parent directory is the disk
		if _, err := os.Stat(filepath.Join(sysPath, "partition")); err == nil {
			walk(filepath.Dir(sysPath), "disk", depth+1)
			return
		}
		// md / dm: walk the member devices
		slaves, _ := os.ReadDir(filepath.Join(sysPath, "slaves"))
		if len(slaves) > 0 {
			for _, s := range slaves {
				p, err := filepath.EvalSymlinks(filepath.Join(sysPath, "slaves", s.Name()))
				if err != nil {
					continue
				}
				walk(p, "member", depth+1)
			}
			return
		}
		if diskName.MatchString(name) && !strings.HasPrefix(name, "md") && !strings.HasPrefix(name, "dm-") {
			devs[len(devs)-1].Role = "disk"
			disks = append(disks, name)
		}
	}
	walk(link, "mount", 0)
	sort.Strings(disks)
	return devs, disks
}

func describeDevice(sysPath, name, role string) BlockDevice {
	d := BlockDevice{Name: name, Role: role}
	if b, err := readCapped(filepath.Join(sysPath, "size"), 64); err == nil {
		sectors, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		d.SizeBytes = sectors * 512
	}
	disk := sysPath
	if _, err := os.Stat(filepath.Join(sysPath, "partition")); err == nil {
		disk = filepath.Dir(sysPath)
	}
	if b, err := readCapped(filepath.Join(disk, "queue/rotational"), 8); err == nil {
		d.Rotational = strings.TrimSpace(string(b)) == "1"
	}
	if b, err := readCapped(filepath.Join(disk, "device/model"), 256); err == nil {
		d.Model = strings.TrimSpace(string(b))
	}
	return d
}

var cmdSearchPath = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin", "/usr/local/sbin", "/usr/local/bin"}

func lookCmd(name string) (string, error) {
	for _, dir := range cmdSearchPath {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not installed", name)
}

// runCmd runs a fixed diagnostic command. Arguments are never derived from
// caller input other than through values validated against this node's own
// configuration (mount points of attached storage paths, device names
// resolved from sysfs and matched against diskName). Privileged commands are
// run through "sudo -n" unless curio runs as root; without a matching sudo
// rule they report Unavailable.
func runCmd(ctx context.Context, privileged bool, name string, args ...string) CommandResult {
	return runCmdOut(ctx, privileged, &cappedBuffer{max: maxCmdOutput}, name, args...)
}

type cmdOutput interface {
	io.Writer
	String() string
}

func runCmdOut(ctx context.Context, privileged bool, out cmdOutput, name string, args ...string) CommandResult {
	bin, err := lookCmd(name)
	res := CommandResult{Command: strings.Join(append([]string{name}, args...), " ")}
	if err != nil {
		res.Unavailable = err.Error()
		return res
	}
	argv := append([]string{bin}, args...)
	if privileged && os.Geteuid() != 0 {
		sudo, err := lookCmd("sudo")
		if err != nil {
			res.Unavailable = "needs root and " + err.Error()
			return res
		}
		argv = append([]string{sudo, "-n"}, argv...)
	}

	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdout = out
	cmd.Stderr = out
	start := time.Now()
	err = cmd.Run()
	res.DurationMS = time.Since(start).Milliseconds()
	res.Output = out.String()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			if privileged && os.Geteuid() != 0 && strings.HasPrefix(res.Output, "sudo:") {
				res.Unavailable = "no passwordless sudo rule for this command (see aurora/mcp/README.md): " + strings.TrimSpace(res.Output)
				res.Output = ""
			}
		} else {
			res.Unavailable = err.Error()
		}
	}
	return res
}

// dmesgFor returns recent kernel warnings and errors mentioning the devices,
// plus generic I/O and filesystem errors.
func dmesgFor(ctx context.Context, devs []BlockDevice) CommandResult {
	res := runCmdOut(ctx, true, &tailBuffer{max: maxDmesgBytes}, "dmesg", "-T", "--level=err,warn")
	if res.Unavailable != "" || res.ExitCode != 0 {
		return res
	}
	var keys []string
	for _, d := range devs {
		keys = append(keys, d.Name)
	}
	keys = append(keys, "I/O error", "EXT4-fs error", "XFS (", "Buffer I/O", "blk_update_request", "critical medium error", "nvme")

	var kept []string
	sc := bufio.NewScanner(strings.NewReader(res.Output))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		for _, k := range keys {
			if strings.Contains(l, k) {
				kept = append(kept, l)
				break
			}
		}
	}
	if len(kept) > maxDmesgLines {
		kept = kept[len(kept)-maxDmesgLines:]
	}
	res.Command += " (filtered to storage-related lines)"
	res.Output = strings.Join(kept, "\n")
	return res
}

type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	if c.buf.Len() >= c.max {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.truncated {
		return c.buf.String() + "\n…[output truncated]"
	}
	return c.buf.String()
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
