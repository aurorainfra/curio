package config

import (
	"bytes"

	"github.com/BurntSushi/toml"

	"github.com/filecoin-project/curio/deps"
	depsconfig "github.com/filecoin-project/curio/deps/config"
)

func configToJSONMap(v any) (map[string]any, error) {
	cb, err := depsconfig.ConfigUpdate(v, depsconfig.DefaultCurioConfig(), depsconfig.Commented(false), depsconfig.DefaultKeepUncommented(), depsconfig.NoEnv())
	if err != nil {
		return nil, err
	}
	return tomlToJSONMap(string(cb))
}

func tomlToJSONMap(layerToml string) (map[string]any, error) {
	configStruct := map[string]any{}
	if layerToml != "" {
		sanitized, err := depsconfig.StripEmptyDynamicTables(layerToml, depsconfig.DefaultCurioConfig())
		if err != nil {
			return nil, err
		}
		if _, err := toml.Decode(sanitized, &configStruct); err != nil {
			return nil, err
		}
	}
	return configStruct, nil
}

// prepareCurioLayerSave turns a UI submission into the text stored for a
// layer. The base layer is stored as a full, commented config. Other layers
// are overlays applied on top of base, so they are stored with exactly the
// keys the layer sets: re-serializing them as a full config would comment
// out values equal to the defaults (dropping overrides of base back to the
// default) and emit an empty [[Addresses]] entry that replaces base's miners.
func prepareCurioLayerSave(layer string, configStruct map[string]any) (string, error) {
	var tomlData bytes.Buffer
	if err := toml.NewEncoder(&tomlData).Encode(configStruct); err != nil {
		return "", err
	}

	curioCfg := depsconfig.DefaultCurioConfig()
	if _, err := deps.LoadConfigWithUpgrades(tomlData.String(), curioCfg); err != nil {
		return "", err
	}

	if layer == "base" {
		return formatLayerTOML(curioCfg)
	}

	layerToml, err := depsconfig.StripEmptyDynamicTables(tomlData.String(), depsconfig.DefaultCurioConfig())
	if err != nil {
		return "", err
	}
	layerToml, _ = depsconfig.DropEmptyAddresses(layerToml)
	return layerToml, nil
}

func formatLayerTOML(curioCfg *depsconfig.CurioConfig) (string, error) {
	cb, err := depsconfig.ConfigUpdate(curioCfg, depsconfig.DefaultCurioConfig(), depsconfig.Commented(true), depsconfig.DefaultKeepUncommented(), depsconfig.NoEnv())
	if err != nil {
		return "", err
	}
	return string(cb), nil
}

// mustEncodeTOML serialises v to TOML or panics.
func mustEncodeTOML(v any) string {
	data, err := depsconfig.TransparentMarshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
