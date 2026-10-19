//go:build !skiff

package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func uncommented(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}

func TestNonBaseLayerSaveKeepsOnlyLayerKeys(t *testing.T) {
	// a layer overriding base back to a default value, plus a non-default one
	layer := "[Subsystems]\n  EnableWindowPost = false\n\n[Ingest]\n  MaxQueueSDR = 16\n"
	m, err := uiLayerJSON(layer)
	require.NoError(t, err)

	// round trip through JSON as the browser does
	b, err := json.Marshal(m)
	require.NoError(t, err)
	var submitted map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&submitted))

	out, err := uiPrepareLayerSave("market", submitted, layer)
	require.NoError(t, err)
	lines := uncommented(out)
	require.NotContains(t, out, "Addresses")
	require.Contains(t, lines, "EnableWindowPost = false", "override equal to the default must survive")
	require.Contains(t, lines, "MaxQueueSDR = 16")
	require.Contains(t, lines, "[Ingest]")
	require.Len(t, lines, 4, lines)
}

func TestNonBaseLayerSaveDropsEmptyAddresses(t *testing.T) {
	submitted := map[string]any{
		"Addresses": []any{map[string]any{"MinerAddresses": []any{}}},
		"Ingest":    map[string]any{"MaxQueueSDR": json.Number("4")},
	}
	out, err := uiPrepareLayerSave("market", submitted, "")
	require.NoError(t, err)
	require.NotContains(t, out, "Addresses")
	require.Contains(t, out, "MaxQueueSDR = 4")
}

func TestBaseLayerSaveIsFullConfig(t *testing.T) {
	submitted := map[string]any{
		"Addresses": []any{map[string]any{"MinerAddresses": []any{"f01234"}}},
	}
	out, err := uiPrepareLayerSave("base", submitted, "")
	require.NoError(t, err)
	require.Contains(t, uncommented(out), `MinerAddresses = ["f01234"]`)
	require.Contains(t, out, "# type:") // documented, commented full config
}
