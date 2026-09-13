package tarutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTarFileSizes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p_aux"), make([]byte, 64), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "t_aux"), make([]byte, 10), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sc-02-data-layer-1.dat"), make([]byte, 5), 0644)) // not a fincache file

	var buf bytes.Buffer
	require.NoError(t, TarDirectory(FinCacheFileConstraints, dir, &buf, make([]byte, 1<<10)))

	sizes, err := TarFileSizes(&buf)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"p_aux": 64, "t_aux": 10}, sizes)
}
