package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// composePrivilegedFeature is one setting of a compose definition that reaches
// past the container sandbox: host devices, extra capabilities, host
// namespaces, host paths and networks, other compose files. A release that
// uses any of them has to declare
// privileged access, and the project has to grant it; without the grant the
// agent strips them before the app starts.
//
// The backend classifies the same way when it publishes a release. Both
// implementations are pinned by the shared test vectors in
// testdata/privileged-compose-fixture.json (the backend holds an identical
// copy): when a rule changes here, it changes there too.
type composePrivilegedFeature struct {
	Service string `json:"service"`
	Feature string `json:"feature"`
	Value   string `json:"value"`
}

// privilegedServiceKeys are the service keys that can carry privileged
// features, in the order the classifier reports them.
var privilegedServiceKeys = []string{
	"privileged",
	"cap_add",
	"devices",
	"device_cgroup_rules",
	"security_opt",
	"network_mode",
	"pid",
	"ipc",
	"uts",
	"userns_mode",
	"cgroup",
	"cgroup_parent",
	"volumes",
	"volumes_from",
	"extends",
}

// classifyComposePrivileges lists the privileged features of a compose
// definition: per service in ascending name order (keys in
// privilegedServiceKeys order, list entries in list order), then the
// top-level volumes, networks, secrets and configs (each in ascending name
// order) and include. The definition is not changed.
func classifyComposePrivileges(dockerCompose map[string]interface{}) []composePrivilegedFeature {
	return scanComposePrivileges(dockerCompose, false)
}

// stripComposePrivileges removes every privileged feature from the compose
// definition in place and returns what it removed. Everything else stays as
// authored.
func stripComposePrivileges(dockerCompose map[string]interface{}) []composePrivilegedFeature {
	return scanComposePrivileges(dockerCompose, true)
}

func scanComposePrivileges(dockerCompose map[string]interface{}, strip bool) []composePrivilegedFeature {
	features := []composePrivilegedFeature{}

	services, _ := dockerCompose["services"].(map[string]interface{})
	for _, serviceName := range sortedComposeKeys(services) {
		service, ok := services[serviceName].(map[string]interface{})
		if !ok {
			continue
		}

		for _, key := range privilegedServiceKeys {
			value, present := service[key]
			if !present {
				continue
			}

			found, kept := classifyServiceKey(key, value)
			for _, featureValue := range found {
				features = append(features, composePrivilegedFeature{Service: serviceName, Feature: key, Value: featureValue})
			}

			if !strip || len(found) == 0 {
				continue
			}

			// A list keeps its harmless entries; a key that is left empty, or
			// is a single setting, goes as a whole.
			if len(kept) > 0 {
				service[key] = kept
			} else {
				delete(service, key)
			}
		}
	}

	// A named volume can still bind a host path through its driver options,
	// or reach a volume of another app by its fixed name.
	volumes, _ := dockerCompose["volumes"].(map[string]interface{})
	for _, volumeName := range sortedComposeKeys(volumes) {
		volume, ok := volumes[volumeName].(map[string]interface{})
		if !ok {
			continue
		}

		if isNonEmptyComposeValue(volume["driver_opts"]) {
			features = append(features, composePrivilegedFeature{Feature: "volume_driver_opts", Value: volumeName})
			if strip {
				delete(volume, "driver")
				delete(volume, "driver_opts")
			}
		}

		if isTruthyComposeValue(volume["external"]) || isTruthyComposeValue(volume["name"]) {
			features = append(features, composePrivilegedFeature{Feature: "volume_external", Value: volumeName})
			if strip {
				delete(volume, "external")
				delete(volume, "name")
			}
		}
	}

	// A network with its own driver (macvlan, ipvlan, host) or driver options
	// reaches the host's interfaces; an external or fixed-name network can be
	// another app's, or the host network itself. The services' memberships
	// stay: without the settings the network is the app's own bridge.
	networks, _ := dockerCompose["networks"].(map[string]interface{})
	for _, networkName := range sortedComposeKeys(networks) {
		network, ok := networks[networkName].(map[string]interface{})
		if !ok {
			continue
		}

		driver, hasDriver := network["driver"]
		if (hasDriver && driver != nil && driver != "" && driver != "bridge") || isNonEmptyComposeValue(network["driver_opts"]) {
			features = append(features, composePrivilegedFeature{Feature: "network_driver", Value: networkName})
			if strip {
				delete(network, "driver")
				delete(network, "driver_opts")
			}
		}

		if isTruthyComposeValue(network["external"]) || isTruthyComposeValue(network["name"]) {
			features = append(features, composePrivilegedFeature{Feature: "network_external", Value: networkName})
			if strip {
				delete(network, "external")
				delete(network, "name")
			}
		}
	}

	// Secrets and configs read from a file outside the app's directory are
	// host files.
	for _, section := range []struct{ key, feature string }{
		{key: "secrets", feature: "secret_file"},
		{key: "configs", feature: "config_file"},
	} {
		entries, _ := dockerCompose[section.key].(map[string]interface{})
		for _, entryName := range sortedComposeKeys(entries) {
			entry, ok := entries[entryName].(map[string]interface{})
			if !ok {
				continue
			}

			file, ok := entry["file"].(string)
			if !ok || !isOutsideComposeDir(file) {
				continue
			}

			features = append(features, composePrivilegedFeature{Feature: section.feature, Value: entryName})
			if strip {
				delete(entries, entryName)
				removeComposeServiceReferences(services, section.key, entryName)
			}
		}
	}

	// An included compose file can hold any setting, and is not part of the
	// release.
	if include, present := dockerCompose["include"]; present {
		includes := composeListEntries(include)
		for _, entry := range includes {
			value := composeScalarString(entry)
			if included, ok := entry.(map[string]interface{}); ok {
				value = composeScalarString(included["path"])
			}
			features = append(features, composePrivilegedFeature{Feature: "include", Value: value})
		}
		if strip && len(includes) > 0 {
			delete(dockerCompose, "include")
		}
	}

	return features
}

// classifyServiceKey returns the privileged feature values of one service key
// and, for the keys that are lists of independent entries (security_opt,
// volumes, volumes_from), the entries that may stay. Any other key with a
// feature goes as a whole.
func classifyServiceKey(key string, value interface{}) (found []string, kept []interface{}) {
	switch key {
	case "privileged":
		// null leaves the setting unset, like leaving it out.
		if value == nil || value == false || value == "false" {
			return nil, nil
		}
		if value == true {
			return []string{"true"}, nil
		}
		return []string{composeScalarString(value)}, nil

	case "cap_add", "device_cgroup_rules":
		for _, entry := range composeListEntries(value) {
			found = append(found, composeScalarString(entry))
		}
		return found, nil

	case "devices":
		for _, entry := range composeListEntries(value) {
			if device, ok := entry.(map[string]interface{}); ok {
				if source, ok := device["source"].(string); ok {
					found = append(found, source)
					continue
				}
			}
			found = append(found, composeScalarString(entry))
		}
		return found, nil

	case "network_mode", "pid", "ipc", "uts", "userns_mode", "cgroup":
		mode := composeScalarString(value)
		if mode == "host" || strings.HasPrefix(mode, "container:") || strings.Contains(mode, "$") {
			return []string{mode}, nil
		}
		return nil, nil

	case "cgroup_parent":
		// Any parent cgroup is a place in the host's hierarchy.
		if value == nil || value == "" {
			return nil, nil
		}
		return []string{composeScalarString(value)}, nil

	case "extends":
		// Extending a service of the same file is fine; another file can hold
		// any setting, and is not part of the release.
		if extends, ok := value.(map[string]interface{}); ok {
			if file, ok := extends["file"].(string); ok {
				return []string{file}, nil
			}
		}
		return nil, nil

	case "security_opt", "volumes", "volumes_from":
		for _, entry := range composeListEntries(value) {
			featureValue, privileged := classifyServiceListEntry(key, entry)
			if privileged {
				found = append(found, featureValue)
			} else {
				kept = append(kept, entry)
			}
		}
		return found, kept
	}

	return nil, nil
}

func classifyServiceListEntry(key string, entry interface{}) (string, bool) {
	switch key {
	case "security_opt":
		option := composeScalarString(entry)
		return option, !strings.HasPrefix(option, "no-new-privileges")

	case "volumes_from":
		from, ok := entry.(string)
		return from, ok && strings.HasPrefix(from, "container:")

	case "volumes":
		switch volume := entry.(type) {
		case string:
			return volume, isPrivilegedShortVolume(volume)
		case map[string]interface{}: // long syntax
			source, _ := volume["source"].(string)
			switch volume["type"] {
			case "bind":
				readOnly, _ := volume["read_only"].(bool)
				return source, isPrivilegedBindSource(source, readOnly)
			case "npipe":
				return source, true
			case "volume":
				return source, strings.Contains(source, "$")
			}
			return source, false
		}
	}

	return "", false
}

// isPrivilegedShortVolume classifies a short-syntax volume entry
// ("source:target[:mode]", the mode being everything after the second colon).
// A Windows drive-letter source keeps its colon; a one-part entry is an
// anonymous volume.
func isPrivilegedShortVolume(volume string) bool {
	drive := ""
	rest := volume
	if windowsDrivePath.MatchString(volume) {
		drive, rest = volume[:2], volume[2:]
	}

	parts := strings.SplitN(rest, ":", 3)
	if len(parts) == 1 {
		// An anonymous volume, unless a parameter fills it in.
		return strings.Contains(volume, "$")
	}

	readOnly := false
	if len(parts) == 3 {
		for _, option := range strings.Split(parts[2], ",") {
			if option == "ro" {
				readOnly = true
			}
		}
	}

	return isPrivilegedBindSource(drive+parts[0], readOnly)
}

// isPrivilegedBindSource reports whether a bind source reaches outside the
// app's own directory. Interpolated sources are decided at runtime and
// count as privileged; so do absolute, home and Windows (drive, UNC, named
// pipe) paths, except the read-only host time zone files. Relative sources are fine unless they climb out; any
// other source names a volume.
func isPrivilegedBindSource(source string, readOnly bool) bool {
	switch {
	case strings.Contains(source, "$"):
		return true
	case strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~") || strings.HasPrefix(source, `\`) || windowsDrivePath.MatchString(source):
		return !readOnly || (source != "/etc/localtime" && source != "/etc/timezone")
	case strings.HasPrefix(source, "."):
		return hasParentPathSegment(source)
	}
	return false
}

// isOutsideComposeDir reports whether a secret's or config's file lies
// outside the app's directory.
func isOutsideComposeDir(file string) bool {
	return strings.HasPrefix(file, "/") ||
		strings.HasPrefix(file, "~") ||
		strings.HasPrefix(file, `\`) ||
		windowsDrivePath.MatchString(file) ||
		strings.Contains(file, "$") ||
		hasParentPathSegment(file)
}

func hasParentPathSegment(path string) bool {
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return true
		}
	}
	return false
}

// removeComposeServiceReferences drops every service's reference to a removed
// top-level secret or config, and the service's list when nothing is left.
func removeComposeServiceReferences(services map[string]interface{}, key string, name string) {
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok {
			continue
		}

		references, ok := service[key].([]interface{})
		if !ok {
			continue
		}

		kept := make([]interface{}, 0, len(references))
		for _, reference := range references {
			switch ref := reference.(type) {
			case string:
				if ref == name {
					continue
				}
			case map[string]interface{}:
				if source, _ := ref["source"].(string); source == name {
					continue
				}
			}
			kept = append(kept, reference)
		}

		if len(kept) == len(references) {
			continue
		}
		if len(kept) == 0 {
			delete(service, key)
		} else {
			service[key] = kept
		}
	}
}

// composeListEntries returns a list value's entries; a lone scalar where the
// compose schema expects a list counts as one entry.
func composeListEntries(value interface{}) []interface{} {
	switch entries := value.(type) {
	case nil:
		return nil
	case []interface{}:
		return entries
	default:
		return []interface{}{value}
	}
}

// composeScalarString renders a compose value the way it is reported: strings
// as written, anything else JSON-encoded (without HTML escaping, like the
// backend's JSON.stringify).
func composeScalarString(value interface{}) string {
	if s, ok := value.(string); ok {
		return s
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}

// isNonEmptyComposeValue reports whether a setting such as driver_opts has
// content: null, false, 0, "" and an empty map or list have none.
func isNonEmptyComposeValue(value interface{}) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case map[string]interface{}:
		return len(v) > 0
	case []interface{}:
		return len(v) > 0
	case string:
		return v != ""
	}
	return true
}

// isTruthyComposeValue is the truthiness of external and name: null, false, 0
// and "" are false, anything else (any map or list included) is true.
func isTruthyComposeValue(value interface{}) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	}
	return true
}

func sortedComposeKeys(entries map[string]interface{}) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
