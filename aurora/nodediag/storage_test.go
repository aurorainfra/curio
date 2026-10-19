package nodediag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSmart(t *testing.T) {
	ata := `{"smartctl":{"exit_status":64},"smart_status":{"passed":true},"temperature":{"current":41},
	"power_on_time":{"hours":31000},
	"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":8}},{"id":197,"raw":{"value":0}},{"id":199,"raw":{"value":3}}]}}`
	var s SmartSummary
	parseSmart([]byte(ata), &s)
	require.Equal(t, "warn", s.Verdict)
	require.Equal(t, int64(8), *s.Reallocated)
	require.Equal(t, int64(0), *s.Pending)
	require.Equal(t, int64(31000), *s.PowerOnH)
	require.Contains(t, strings.Join(s.Reasons, ";"), "8 reallocated sectors")
	require.Contains(t, strings.Join(s.Reasons, ";"), "CRC")
	require.Contains(t, strings.Join(s.Reasons, ";"), "error log")

	sas := `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"temperature":{"current":38},
	"scsi_grown_defect_list":0,
	"scsi_error_counter_log":{"read":{"total_uncorrected_errors":0},"write":{"total_uncorrected_errors":0},"verify":{"total_uncorrected_errors":0}}}`
	s = SmartSummary{}
	parseSmart([]byte(sas), &s)
	require.Equal(t, "ok", s.Verdict, s.Reasons)

	sasBad := `{"smartctl":{"exit_status":8},"smart_status":{"passed":false},"scsi_grown_defect_list":120,
	"scsi_error_counter_log":{"read":{"total_uncorrected_errors":7}}}`
	s = SmartSummary{}
	parseSmart([]byte(sasBad), &s)
	require.Equal(t, "fail", s.Verdict)
	require.Equal(t, int64(120), *s.GrownDefects)
	require.Equal(t, int64(7), *s.ReadUncorrected)

	nvme := `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},
	"nvme_smart_health_information_log":{"critical_warning":0,"temperature":45,"available_spare":3,"available_spare_threshold":10,"percentage_used":91,"media_errors":0,"num_err_log_entries":12}}`
	s = SmartSummary{}
	parseSmart([]byte(nvme), &s)
	require.Equal(t, "fail", s.Verdict)
	require.Equal(t, int64(45), *s.TempC)
	require.Equal(t, int64(91), *s.PercentUsed)
	require.Contains(t, strings.Join(s.Reasons, ";"), "available spare 3% below threshold 10%")

	s = SmartSummary{}
	parseSmart([]byte("not json"), &s)
	require.NotEmpty(t, s.Error)
	s = SmartSummary{}
	parseSmart([]byte(`{"smartctl":{"exit_status":4}}`), &s)
	require.Equal(t, "unknown", s.Verdict)
}

// fakeSAS builds a sysfs tree: HBA host7 -> expander-7:0 -> sdb in enclosure
// slot 1, with link errors on the expander phy.
func fakeSAS(t *testing.T) string {
	sys := t.TempDir()
	mk := func(p string) string {
		full := filepath.Join(sys, p)
		require.NoError(t, os.MkdirAll(full, 0o755))
		return full
	}
	write := func(p, v string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(sys, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sys, p), []byte(v), 0o644))
	}
	link := func(target, name string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(sys, name)), 0o755))
		require.NoError(t, os.Symlink(filepath.Join(sys, target), filepath.Join(sys, name)))
	}

	host := "devices/pci0000:00/0000:01:00.0/host7"
	exp := host + "/port-7:0/expander-7:0"
	disk := exp + "/port-7:0:0/end_device-7:0:0/target7:0:0/7:0:0:0"
	encDev := exp + "/port-7:0:1/end_device-7:0:1/target7:0:1/7:0:1:0"

	mk(host + "/phy-7:0")
	mk(exp + "/phy-7:0:0")
	mk(disk + "/block/sdb/queue")
	write(disk+"/vendor", "SEAGATE")
	write(disk+"/model", "ST18000NM004J")
	write(disk+"/sas_address", "0x5000c500abcdef01")
	write(disk+"/block/sdb/size", "35156656128")
	write(disk+"/block/sdb/queue/rotational", "1")
	link(disk, disk+"/block/sdb/device")
	link(disk+"/block/sdb", "block/sdb")

	write("class/scsi_host/host7/proc_name", "mpt3sas")
	write("class/scsi_host/host7/version_fw", "16.17.01.00")
	mk("class/sas_host/host7")

	write("class/sas_expander/expander-7:0/vendor_id", "LSI")
	write("class/sas_expander/expander-7:0/product_id", "SAS3x48Front")
	link(exp, "class/sas_expander/expander-7:0/device")

	for name, dev := range map[string]string{"phy-7:0": host + "/phy-7:0", "phy-7:0:0": exp + "/phy-7:0:0"} {
		write("class/sas_phy/"+name+"/negotiated_linkrate", "12.0 Gbit")
		write("class/sas_phy/"+name+"/maximum_linkrate", "12.0 Gbit")
		write("class/sas_phy/"+name+"/invalid_dword_count", "0")
		write("class/sas_phy/"+name+"/running_disparity_error_count", "0")
		write("class/sas_phy/"+name+"/loss_of_dword_sync_count", "0")
		write("class/sas_phy/"+name+"/phy_reset_problem_count", "0")
		link(dev, "class/sas_phy/"+name+"/device")
	}
	write("class/sas_phy/phy-7:0:0/negotiated_linkrate", "6.0 Gbit")
	write("class/sas_phy/phy-7:0:0/invalid_dword_count", "5")

	// ports: host port -> expander, expander port -> disk end device
	link(host+"/phy-7:0", host+"/port-7:0/phy-7:0")
	link(host+"/port-7:0", "class/sas_port/port-7:0/device")
	link(exp+"/phy-7:0:0", exp+"/port-7:0:0/phy-7:0:0")
	link(exp+"/port-7:0:0", "class/sas_port/port-7:0:0/device")

	// enclosure with two slots, slot 1 holding sdb, slot 2 failed and empty
	mk(encDev)
	write(encDev+"/vendor", "SMC")
	write(encDev+"/model", "SC847P")
	mk(encDev + "/scsi_generic/sg9")
	link(encDev, "class/enclosure/7:0:1:0/device")
	write("class/enclosure/7:0:1:0/components", "2")
	write("class/enclosure/7:0:1:0/Slot01/status", "OK")
	write("class/enclosure/7:0:1:0/Slot01/slot", "1")
	link(disk, "class/enclosure/7:0:1:0/Slot01/device")
	write("class/enclosure/7:0:1:0/Slot02/status", "Critical")
	write("class/enclosure/7:0:1:0/Slot02/slot", "2")
	write("class/enclosure/7:0:1:0/Slot02/fault", "1")
	return sys
}

func TestSASTopology(t *testing.T) {
	sys := fakeSAS(t)
	old := sysRoot
	sysRoot = sys
	defer func() { sysRoot = old }()

	disks, err := listDisks()
	require.NoError(t, err)
	require.Len(t, disks, 1)
	d := disks[0]
	require.Equal(t, "sdb", d.Name)
	require.Equal(t, "sas", d.Transport)
	require.Equal(t, "7:0:0:0", d.HCTL)
	require.Equal(t, "host7", d.Host)
	require.Equal(t, int64(35156656128*512), d.SizeBytes)
	require.True(t, d.Rotational)

	slots := enclosureSlotsByDisk()
	require.Equal(t, slotRef{"7:0:1:0", "Slot01"}, slots["sdb"])

	st := &sasState{}
	r, err := st.report(context.Background(), SASRequest{})
	require.NoError(t, err)
	require.Len(t, r.Hosts, 1)
	require.Equal(t, "mpt3sas", r.Hosts[0].Driver)
	require.Equal(t, 1, r.Hosts[0].Phys)
	require.Len(t, r.Expanders, 1)
	require.Equal(t, "host7", r.Expanders[0].Parent)
	require.Equal(t, 1, r.Expanders[0].Disks)
	require.Equal(t, 2, r.PhyCount)

	var ep SASPhy
	for _, p := range r.Phys {
		if p.Name == "phy-7:0:0" {
			ep = p
		}
	}
	require.Equal(t, "expander-7:0", ep.Parent)
	require.Equal(t, "sdb", ep.Attached)
	require.Len(t, ep.Problems, 2, ep.Problems) // link errors, degraded rate
	require.Nil(t, ep.Delta)

	require.Len(t, r.Enclosures, 1)
	e := r.Enclosures[0]
	require.Equal(t, "sg9", e.SGDevice)
	require.Equal(t, 1, e.Populated)
	require.Len(t, e.Problems, 2) // Slot02 critical + fault

	// counters rising between calls are reported as deltas
	require.NoError(t, os.WriteFile(filepath.Join(sys, "class/sas_phy/phy-7:0:0/invalid_dword_count"), []byte("9"), 0o644))
	r, err = st.report(context.Background(), SASRequest{ProblemsOnly: true})
	require.NoError(t, err)
	require.Len(t, r.Phys, 1)
	require.Equal(t, int64(4), r.Phys[0].Delta.InvalidDword)
	require.Contains(t, strings.Join(r.Problems, "\n"), "still increasing")
	require.Len(t, r.Enclosures[0].Slots, 1) // only the bad slot
}

func TestRedactTOML(t *testing.T) {
	in := "[MK12]\n  CIDGravityTokens = [\"abc\", \"def\"]\n  PublishMsgPeriod = \"30m\"\n[[Tokens]]\n  Name = \"a\"\n  TokenSHA256 = \"00ff\"\n  #PageDutyIntegrationKey = \"x\""
	out := redactTOML(in)
	require.NotContains(t, out, "abc")
	require.NotContains(t, out, "00ff")
	require.NotContains(t, out, `"x"`)
	require.Contains(t, out, `PublishMsgPeriod = "30m"`)
	require.Contains(t, out, `Name = "a"`)
}
