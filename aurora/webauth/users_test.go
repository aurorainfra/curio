package webauth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/filecoin-project/curio/deps/config"
)

func fastHash(t *testing.T, pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

func TestSetLayerUsersKeepsRestOfLayer(t *testing.T) {
	// a real base layer: full commented config with a miner
	base := config.DefaultCurioConfig()
	base.Addresses.Set([]config.CurioAddresses{{MinerAddresses: []string{"f01234"}, PreCommitControl: []string{}, CommitControl: []string{}, DealPublishControl: []string{}, TerminateControl: []string{}, BalanceManager: config.DefaultBalanceManager()}})
	base.Ingest.MaxQueueSDR.Set(16)
	text, err := config.ConfigUpdate(base, config.DefaultCurioConfig(), config.Commented(true), config.DefaultKeepUncommented(), config.NoEnv())
	require.NoError(t, err)

	users := []config.AuroraWebUser{{Name: "ops", PasswordBcrypt: fastHash(t, "pw-ops-123456")}, {Name: "ro@x", PasswordBcrypt: fastHash(t, "pw-ro-1234567"), Disabled: true}}
	out, err := SetLayerUsers(string(text), users)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(out, strings.TrimRight(string(text), "\n")), "existing text must be kept verbatim")

	got, set, err := LayerUsers(out)
	require.NoError(t, err)
	require.True(t, set)
	require.Equal(t, users, got)

	cfg := config.DefaultCurioConfig()
	_, err = config.LoadConfigWithUpgrades(out, cfg)
	require.NoError(t, err)
	require.Equal(t, []string{"f01234"}, cfg.Addresses.Get()[0].MinerAddresses)
	require.Equal(t, 16, cfg.Ingest.MaxQueueSDR.Get())

	// replace again: old blocks go, the rest stays
	users = users[:1]
	out2, err := SetLayerUsers(out, users)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(out2, "[[Aurora.WebUI.Users]]"))
	got, _, err = LayerUsers(out2)
	require.NoError(t, err)
	require.Equal(t, users, got)

	// remove all
	out3, err := SetLayerUsers(out2, nil)
	require.NoError(t, err)
	require.NotContains(t, out3, "[[Aurora.WebUI.Users]]")
	_, set, err = LayerUsers(out3)
	require.NoError(t, err)
	require.False(t, set)
}

func TestSetLayerUsersReplacesOtherForms(t *testing.T) {
	h := fastHash(t, "pw-ops-123456")
	for name, layer := range map[string]string{
		"blocks in the middle": "[Ingest]\n  MaxQueueSDR = 3\n\n[[Aurora.WebUI.Users]] # old\n  Name = \"old\"\n  PasswordBcrypt = \"" + h + "\"\n\n[Seal]\n  BatchSealPipelines = 2\n",
		"inline array":         "[Aurora.WebUI]\n  SessionTTL = \"1h0m0s\"\n  Users = [{Name = \"old\", PasswordBcrypt = \"" + h + "\"}]\n[Ingest]\n  MaxQueueSDR = 3\n",
		"multi-line inline":    "[Aurora.WebUI]\n  Users = [\n    {Name = \"old\", PasswordBcrypt = \"" + h + "\"},\n  ]\n  SessionTTL = \"1h0m0s\"\n[Ingest]\n  MaxQueueSDR = 3\n",
		"dotted key":           "Aurora.WebUI.Users = [{Name = \"old\", PasswordBcrypt = \"" + h + "\"}]\n[Ingest]\n  MaxQueueSDR = 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			users := []config.AuroraWebUser{{Name: "new", PasswordBcrypt: h}}
			out, err := SetLayerUsers(layer, users)
			require.NoError(t, err)
			require.NotContains(t, out, `"old"`)
			got, set, err := LayerUsers(out)
			require.NoError(t, err)
			require.True(t, set)
			require.Equal(t, users, got)

			cfg := config.DefaultCurioConfig()
			_, err = config.LoadConfigWithUpgrades(out, cfg)
			require.NoError(t, err)
			require.Equal(t, 3, cfg.Ingest.MaxQueueSDR.Get())
		})
	}
}

func TestSetLayerUsersValidates(t *testing.T) {
	h := fastHash(t, "pw-ops-123456")
	_, err := SetLayerUsers("", []config.AuroraWebUser{{Name: "a b", PasswordBcrypt: h}})
	require.Error(t, err)
	_, err = SetLayerUsers("", []config.AuroraWebUser{{Name: "a", PasswordBcrypt: "plain"}})
	require.Error(t, err)
	_, err = SetLayerUsers("", []config.AuroraWebUser{{Name: "a", PasswordBcrypt: h}, {Name: "a", PasswordBcrypt: h}})
	require.Error(t, err)
	_, err = HashPassword("short")
	require.Error(t, err)

	out, err := SetLayerUsers("", []config.AuroraWebUser{{Name: "a", PasswordBcrypt: h}})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(out, "\n[[Aurora.WebUI.Users]]") || strings.HasPrefix(out, "[[Aurora.WebUI.Users]]"))
}
