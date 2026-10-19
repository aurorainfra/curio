package config

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const baseWithMiner = `
[[Addresses]]
  MinerAddresses = ["f01234"]
  PreCommitControl = ["f3abc"]
`

func applyBaseAnd(t *testing.T, layer string) *CurioConfig {
	t.Helper()
	cfg := DefaultCurioConfig()
	err := ApplyLayers(context.Background(), cfg, []ConfigText{
		{Title: "base", Config: baseWithMiner},
		{Title: "market", Config: layer},
	}, FixTOML)
	require.NoError(t, err)
	return cfg
}

func TestEmptyAddressesInLayerKeepBaseMiners(t *testing.T) {
	// what the UI wrote for non-base layers: a full commented default config,
	// where the [[Addresses]] header itself is never commented
	layerCfg := DefaultCurioConfig()
	layerCfg.Ingest.MaxQueueSDR.Set(16)
	damaged, err := ConfigUpdate(layerCfg, DefaultCurioConfig(), Commented(true), DefaultKeepUncommented(), NoEnv())
	require.NoError(t, err)
	require.Contains(t, string(damaged), "\n[[Addresses]]")

	for name, layer := range map[string]string{
		"full commented default": string(damaged),
		"empty entry":            "[[Addresses]]\n  PreCommitControl = []\n\n[Ingest]\n  MaxQueueSDR = 16\n",
		"inline empty array":     "Addresses = []\n\n[Ingest]\n  MaxQueueSDR = 16\n",
		"old single table":       "[addresses]\n  CommitControl = []\n[Ingest]\n  MaxQueueSDR = 16\n",
		"subtables":              "[[Addresses]]\n  [Addresses.BalanceManager]\n    [Addresses.BalanceManager.MK12Collateral]\n      CollateralLowThreshold = \"5 FIL\"\n[Ingest]\n  MaxQueueSDR = 16\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := applyBaseAnd(t, layer)
			addrs := cfg.Addresses.Get()
			require.Len(t, addrs, 1)
			require.Equal(t, []string{"f01234"}, addrs[0].MinerAddresses)
			require.Equal(t, 16, cfg.Ingest.MaxQueueSDR.Get())
		})
	}
}

func TestLayerAddressesWithMinersStillOverride(t *testing.T) {
	cfg := applyBaseAnd(t, "[[Addresses]]\n  MinerAddresses = [\"f05678\"]\n")
	addrs := cfg.Addresses.Get()
	require.Len(t, addrs, 1)
	require.Equal(t, []string{"f05678"}, addrs[0].MinerAddresses)
}

func TestDropEmptyAddressesText(t *testing.T) {
	in := "# comment\n[[Addresses]] # x\n  MinerAddresses = []\n  [Addresses.BalanceManager]\n    X = 1\n[Ingest]\n  MaxQueueSDR = 3\n[Market]\n  Addresses = 1\n"
	out, ok := DropEmptyAddresses(in)
	require.True(t, ok)
	require.Equal(t, "# comment\n[Ingest]\n  MaxQueueSDR = 3\n[Market]\n  Addresses = 1\n", out)

	out, ok = DropEmptyAddresses("Addresses = []\n[Ingest]\n  MaxQueueSDR = 3\n")
	require.True(t, ok)
	require.Equal(t, "[Ingest]\n  MaxQueueSDR = 3\n", out)

	keep := "[[Addresses]]\n  MinerAddresses = [\"f01\"]\n[[Addresses]]\n  MinerAddresses = []\n"
	out, ok = DropEmptyAddresses(keep)
	require.False(t, ok)
	require.Equal(t, keep, out)

	out, ok = DropEmptyAddresses("[Ingest]\n  MaxQueueSDR = 3\n")
	require.False(t, ok)
	require.True(t, strings.Contains(out, "MaxQueueSDR"))
}
