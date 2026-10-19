package config

import (
	"strings"

	"github.com/BurntSushi/toml"
)

// DropEmptyAddresses removes an Addresses section from a non-base config
// layer when none of its entries lists a miner address.
//
// Layers are applied in order and TOML arrays replace, so an Addresses
// section in a later layer replaces the base layer's addresses. Layer saves
// that re-serialize a full default config emit an "[[Addresses]]" entry with
// every field at its default; applied on top of base it silently drops every
// miner from the node. Such an entry carries no useful configuration, so it
// is ignored. Sections with at least one miner address are kept as written.
//
// It returns the layer text without the section and whether anything was
// removed.
func DropEmptyAddresses(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	present := false
	for _, l := range lines {
		if isAddressesHeader(l) || isRootAddressesKey(l) {
			present = true
			break
		}
	}
	if !present {
		return text, false
	}

	// [addresses] (pre-multi-address format) decodes like [[addresses]]
	normalized := strings.Join(lines, "\n")
	for _, l := range lines {
		if strings.EqualFold(strings.TrimSpace(l), "[addresses]") {
			normalized = strings.ReplaceAll(normalized, l, "[[addresses]]")
		}
	}
	var probe struct {
		Addresses []struct {
			MinerAddresses []string
		}
	}
	if _, err := toml.Decode(normalized, &probe); err != nil {
		return text, false // leave invalid layers to the regular decode error
	}
	for _, a := range probe.Addresses {
		for _, m := range a.MinerAddresses {
			if strings.TrimSpace(m) != "" {
				return text, false
			}
		}
	}

	var out []string
	inRoot, skip := true, false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			inRoot = false
			skip = isAddressesHeader(l)
			if skip {
				continue
			}
		}
		if skip || (inRoot && isRootAddressesKey(l)) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), true
}

// isAddressesHeader matches [[Addresses]], [Addresses] and their subtables.
func isAddressesHeader(l string) bool {
	t := strings.TrimSpace(l)
	if !strings.HasPrefix(t, "[") {
		return false
	}
	if i := strings.Index(t, "#"); i > 0 {
		t = strings.TrimSpace(t[:i])
	}
	name := strings.ToLower(strings.Trim(t, "[] \t"))
	return name == "addresses" || strings.HasPrefix(name, "addresses.")
}

// isRootAddressesKey matches an inline "Addresses = [...]" assignment.
func isRootAddressesKey(l string) bool {
	t := strings.ToLower(strings.TrimSpace(l))
	if !strings.HasPrefix(t, "addresses") {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(t[len("addresses"):]), "=")
}
