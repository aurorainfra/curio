//go:build !skiff

package config

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/lotus/chain/types"
)

// Every "#/$defs/X" reference in the UI schema must resolve against the root;
// the editor renders fields with unresolved references as untyped values.
func TestUISchemaRefsResolve(t *testing.T) {
	b, err := json.Marshal(buildUISchema())
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	defs, _ := doc["$defs"].(map[string]any)
	require.NotEmpty(t, defs)

	refs := regexp.MustCompile(`"\$ref":"#/\$defs/([^"]+)"`).FindAllStringSubmatch(string(b), -1)
	require.NotEmpty(t, refs)
	for _, r := range refs {
		require.Contains(t, defs, r[1], "unresolved $ref %s", r[1])
	}
	require.Equal(t, 1, strings.Count(string(b), `"$defs"`), "nested $defs left in the schema")
	require.Equal(t, 1, strings.Count(string(b), `"$schema"`), "nested $schema left in the schema")

	// the struct-typed Dynamic lists render as objects with their fields
	for _, name := range []string{"CurioAddresses", "BalanceManagerConfig", "MK12CollateralConfig", "AuroraWebUser", "AuroraMCPToken"} {
		d, ok := defs[name].(map[string]any)
		require.True(t, ok, name)
		require.Equal(t, "object", d["type"], name)
		require.NotEmpty(t, d["properties"], name)
	}
	// field docs reach the hoisted definitions
	addr := defs["CurioAddresses"].(map[string]any)["properties"].(map[string]any)
	require.Contains(t, addr["MinerAddresses"], "options")
}

func TestFILPatternMatchesParser(t *testing.T) {
	re := regexp.MustCompile(filPattern)
	for _, v := range []string{"1 fil", "0.03 fil", "0.31", "1 attofil", "5 FIL", "0.5fil", "100 afil", "1 AttoFIL", ".5 fil"} {
		_, err := types.ParseFIL(v)
		require.NoError(t, err, v)
		require.True(t, re.MatchString(v), v)
	}
	for _, v := range []string{"", "fil", "1 nanofil", "abc", "1 fil 2"} {
		require.False(t, re.MatchString(v), v)
	}
}
