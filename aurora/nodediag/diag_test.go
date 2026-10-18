package nodediag

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/filecoin-project/go-jsonrpc/auth"
	"github.com/stretchr/testify/require"
)

const testDump = `goroutine 1 [running]:
main.main()
	/src/main.go:10 +0x25

goroutine 18 [chan receive, 12 minutes]:
github.com/filecoin-project/curio/harmony/harmonytask.(*TaskEngine).poller(0xc000123000, {0x1, 0x2})
	/src/harmonytask/harmonytask.go:300 +0x1a5
created by github.com/filecoin-project/curio/harmony/harmonytask.New in goroutine 1
	/src/harmonytask/harmonytask.go:200 +0x99

goroutine 19 [chan receive, 3 minutes]:
github.com/filecoin-project/curio/harmony/harmonytask.(*TaskEngine).poller(0xc000999000, {0x7, 0x8})
	/src/harmonytask/harmonytask.go:300 +0x1a5
created by github.com/filecoin-project/curio/harmony/harmonytask.New in goroutine 7
	/src/harmonytask/harmonytask.go:200 +0x99

goroutine 20 [sync.Mutex.Lock, 45 minutes]:
sync.runtime_SemacquireMutex(0xc0000a0000, 0x0, 0x1)
	/go/src/runtime/sema.go:77 +0x25
sync.(*Mutex).lockSlow(0xc0000a0000)
	/go/src/sync/mutex.go:171 +0x165
github.com/filecoin-project/curio/lib/paths.(*Local).AcquireSector(0xc0001, {0x2})
	/src/lib/paths/local.go:991 +0x10
`

func TestGoroutineFilter(t *testing.T) {
	r := filterGoroutines([]byte(testDump), GoroutinesRequest{})
	require.Equal(t, 4, r.Total)
	require.Equal(t, 4, r.Matched)
	require.Len(t, r.Groups, 3, "goroutines 18 and 19 share a stack")
	require.Equal(t, 2, r.Groups[0].Count)
	require.Equal(t, 12, r.Groups[0].MaxWaitMinutes)
	require.ElementsMatch(t, []int64{18, 19}, r.Groups[0].IDs)
	require.Contains(t, r.Groups[0].Stack, "poller(...)")
	require.Equal(t, 2, r.ByState["chan receive"])

	r = filterGoroutines([]byte(testDump), GoroutinesRequest{MinWaitMinutes: 30})
	require.Equal(t, 1, r.Matched)
	require.Equal(t, "github.com/filecoin-project/curio/lib/paths.(*Local).AcquireSector", r.TopFrames[0].Frame)

	r = filterGoroutines([]byte(testDump), GoroutinesRequest{Filter: "harmonytask", State: "chan"})
	require.Equal(t, 2, r.Matched)

	// the real dump of this process parses
	live, err := goroutines(context.Background(), GoroutinesRequest{})
	require.NoError(t, err)
	require.Positive(t, live.Total)
	require.Equal(t, live.Total, live.Matched)
}

func TestFindMountAndDevices(t *testing.T) {
	dir := t.TempDir()
	mi := filepath.Join(dir, "mountinfo")
	require.NoError(t, os.WriteFile(mi, []byte(`22 1 259:2 / / rw,relatime - ext4 /dev/nvme0n1p2 rw
40 22 9:0 / /data rw,noatime - xfs /dev/md0 rw
41 40 253:1 / /data/store\040one rw,noatime - xfs /dev/mapper/vg-store rw
`), 0o644))

	m, err := findMount(mi, "/data/store one/sealed")
	require.NoError(t, err)
	require.Equal(t, "/data/store one", m.MountPoint)
	require.Equal(t, "253:1", m.MajorMinor)
	require.Equal(t, "/dev/mapper/vg-store", m.Source)

	m, err = findMount(mi, "/data/other")
	require.NoError(t, err)
	require.Equal(t, "/data", m.MountPoint)

	m, err = findMount(mi, "/database")
	require.NoError(t, err)
	require.Equal(t, "/", m.MountPoint)

	// fake sysfs: dm-1 on md0, md0 on sda1+sdb1; nvme0n1p2 partition of nvme0n1
	sys := t.TempDir()
	mk := func(p string) string {
		full := filepath.Join(sys, p)
		require.NoError(t, os.MkdirAll(full, 0o755))
		return full
	}
	write := func(p, v string) { require.NoError(t, os.WriteFile(filepath.Join(sys, p), []byte(v), 0o644)) }
	link := func(target, name string) {
		require.NoError(t, os.Symlink(filepath.Join(sys, target), filepath.Join(sys, name)))
	}

	mk("devices/block/sda/sda1")
	write("devices/block/sda/sda1/partition", "1")
	mk("devices/block/sda/queue")
	write("devices/block/sda/queue/rotational", "1")
	mk("devices/block/sda/device")
	write("devices/block/sda/device/model", "HDD-1")
	mk("devices/block/sdb/sdb1")
	write("devices/block/sdb/sdb1/partition", "1")
	mk("devices/virtual/block/md0/slaves")
	link("devices/block/sda/sda1", "devices/virtual/block/md0/slaves/sda1")
	link("devices/block/sdb/sdb1", "devices/virtual/block/md0/slaves/sdb1")
	mk("devices/virtual/block/dm-1/slaves")
	link("devices/virtual/block/md0", "devices/virtual/block/dm-1/slaves/md0")
	mk("devices/pci/nvme/nvme0n1/nvme0n1p2")
	write("devices/pci/nvme/nvme0n1/nvme0n1p2/partition", "2")
	mk("dev/block")
	link("devices/virtual/block/dm-1", "dev/block/253:1")
	link("devices/pci/nvme/nvme0n1/nvme0n1p2", "dev/block/259:2")

	old := sysRoot
	sysRoot = sys
	defer func() { sysRoot = old }()

	devs, disks := resolveDevices("253:1")
	require.Equal(t, []string{"sda", "sdb"}, disks)
	var names []string
	for _, d := range devs {
		names = append(names, d.Name+":"+d.Role)
	}
	require.Equal(t, []string{"dm-1:mount", "md0:member", "sda1:member", "sda:disk", "sdb1:member", "sdb:disk"}, names)
	require.True(t, devs[3].Rotational)
	require.Equal(t, "HDD-1", devs[3].Model)

	_, disks = resolveDevices("259:2")
	require.Equal(t, []string{"nvme0n1"}, disks)

	devs, disks = resolveDevices("1:1")
	require.Empty(t, devs)
	require.Empty(t, disks)
}

func TestDiskNameValidation(t *testing.T) {
	for _, ok := range []string{"sda", "sdab", "nvme0n1", "nvme12n3", "md127", "dm-4", "vdb"} {
		require.True(t, diskName.MatchString(ok), ok)
	}
	for _, bad := range []string{"sda;rm", "../sda", "sda1 -x", "nvme0n1p1", "loop0", ""} {
		require.False(t, diskName.MatchString(bad), bad)
	}
}

func TestHandlerPermission(t *testing.T) {
	h := Handler(nil)
	call := func(perms []auth.Permission) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, PathPrefix+"/"+OpGoroutines, bytes.NewReader([]byte(`{"Filter":"zzz-no-match"}`)))
		if perms != nil {
			req = req.WithContext(auth.WithPerm(req.Context(), perms))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	require.Equal(t, http.StatusForbidden, call(nil).Code)
	require.Equal(t, http.StatusForbidden, call([]auth.Permission{"admin", "read", "write", "sign"}).Code)

	w := call([]auth.Permission{PermDiag})
	require.Equal(t, http.StatusOK, w.Code)
	var r GoroutinesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
	require.Positive(t, r.Total)
	require.Zero(t, r.Matched)

	req := httptest.NewRequest(http.MethodPost, PathPrefix+"/nope", bytes.NewReader([]byte(`{}`)))
	req = req.WithContext(auth.WithPerm(req.Context(), []auth.Permission{PermDiag}))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	req = httptest.NewRequest(http.MethodGet, PathPrefix+"/"+OpGoroutines, nil)
	req = req.WithContext(auth.WithPerm(req.Context(), []auth.Permission{PermDiag}))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
}
