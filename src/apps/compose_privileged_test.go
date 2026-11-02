package apps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// privilegedComposeFixture is testdata/privileged-compose-fixture.json: the
// shared test vectors of the privileged compose classifier. The backend holds
// a byte-identical copy and runs its own classifier against it; keep both
// copies identical.
type privilegedComposeFixture struct {
	Version         int      `json:"version"`
	ServiceKeyOrder []string `json:"serviceKeyOrder"`
	TopLevelOrder   []string `json:"topLevelOrder"`
	Cases           []struct {
		Name     string          `json:"name"`
		Compose  json.RawMessage `json:"compose"`
		Features json.RawMessage `json:"features"`
		Stripped json.RawMessage `json:"stripped"`
	} `json:"cases"`
}

func loadPrivilegedComposeFixture(t *testing.T) privilegedComposeFixture {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "privileged-compose-fixture.json"))
	require.NoError(t, err)

	var fixture privilegedComposeFixture
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Equal(t, 1, fixture.Version)
	require.NotEmpty(t, fixture.Cases)
	return fixture
}

// decodeJSON parses a JSON document the way compose definitions reach the
// agent (sync and push both deliver decoded JSON).
func decodeJSON(t *testing.T, raw []byte) interface{} {
	t.Helper()
	var value interface{}
	require.NoError(t, json.Unmarshal(raw, &value))
	return value
}

// asJSON re-encodes a Go value and parses it again, so values built by the
// classifier compare with the fixture as parsed JSON.
func asJSON(t *testing.T, value interface{}) interface{} {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return decodeJSON(t, encoded)
}

func TestPrivilegedComposeFixtureKeyOrder(t *testing.T) {
	fixture := loadPrivilegedComposeFixture(t)
	assert.Equal(t, fixture.ServiceKeyOrder, privilegedServiceKeys)

	// No fixture case has every top-level section; one compose with all of
	// them, written in reverse, reports them in topLevelOrder.
	compose := decodeJSON(t, []byte(`{
		"include": ["other.yml"],
		"configs": {"c": {"file": "/etc/c"}},
		"secrets": {"s": {"file": "/etc/s"}},
		"networks": {"n": {"external": true}},
		"volumes": {"v": {"external": true}}
	}`)).(map[string]interface{})
	section := map[string]string{
		"volume_external":  "volumes",
		"network_external": "networks",
		"secret_file":      "secrets",
		"config_file":      "configs",
		"include":          "include",
	}
	var order []string
	for _, feature := range classifyComposePrivileges(compose) {
		order = append(order, section[feature.Feature])
	}
	assert.Equal(t, fixture.TopLevelOrder, order)
}

func TestClassifyComposePrivilegesSharedFixture(t *testing.T) {
	for _, tc := range loadPrivilegedComposeFixture(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			compose := decodeJSON(t, tc.Compose).(map[string]interface{})

			features := classifyComposePrivileges(compose)

			assert.Equal(t, decodeJSON(t, tc.Features), asJSON(t, features))
			assert.Equal(t, decodeJSON(t, tc.Compose), asJSON(t, compose), "classifying must not change the definition")
		})
	}
}

func TestStripComposePrivilegesSharedFixture(t *testing.T) {
	for _, tc := range loadPrivilegedComposeFixture(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			compose := decodeJSON(t, tc.Compose).(map[string]interface{})

			removed := stripComposePrivileges(compose)

			assert.Equal(t, decodeJSON(t, tc.Stripped), asJSON(t, compose))
			assert.Equal(t, decodeJSON(t, tc.Features), asJSON(t, removed), "strip reports exactly what it removed")
			assert.Empty(t, classifyComposePrivileges(compose), "nothing privileged is left")
		})
	}
}

func TestClassifyComposePrivilegesUnusualShapes(t *testing.T) {
	compose := map[string]interface{}{
		"services": map[string]interface{}{
			"app": map[string]interface{}{
				"privileged": nil,
				"cap_add":    "ALL",
			},
		},
		"volumes": map[string]interface{}{"data": nil},
	}

	features := classifyComposePrivileges(compose)
	assert.Equal(t, []composePrivilegedFeature{
		{Service: "app", Feature: "cap_add", Value: "ALL"},
	}, features, "a scalar where a list belongs is one entry; a null privileged is unset")

	stripComposePrivileges(compose)
	assert.Equal(t, map[string]interface{}{"privileged": nil}, compose["services"].(map[string]interface{})["app"])
}

func TestClassifyComposePrivilegesUnusualShapesOfTheNewerKeys(t *testing.T) {
	compose := decodeJSON(t, []byte(`{
		"include": {"path": ["a.yml", "b.yml"]},
		"services": {
			"app": {
				"cgroup_parent": null,
				"extends": "base",
				"ipc": {"mode": "$<x>"}
			},
			"web": {"cgroup_parent": false, "extends": {"file": 7, "service": "x"}}
		},
		"networks": {
			"a": {"external": {}},
			"b": {"driver": null, "driver_opts": {}, "external": 0, "name": ""},
			"c": {"driver": "", "name": []},
			"d": null
		}
	}`)).(map[string]interface{})

	features := classifyComposePrivileges(compose)
	assert.Equal(t, []composePrivilegedFeature{
		{Service: "app", Feature: "ipc", Value: `{"mode":"$<x>"}`},
		{Service: "web", Feature: "cgroup_parent", Value: "false"},
		{Service: "", Feature: "network_external", Value: "a"},
		{Service: "", Feature: "network_external", Value: "c"},
		{Service: "", Feature: "include", Value: `["a.yml","b.yml"]`},
	}, features, "null and \"\" are unset; any map or list is a truthy external or name; non-strings are JSON-encoded")

	assert.Empty(t, classifyComposePrivileges(map[string]interface{}{"include": nil, "services": map[string]interface{}{}}))
	assert.Equal(t, []composePrivilegedFeature{{Feature: "include", Value: "null"}},
		classifyComposePrivileges(map[string]interface{}{"include": []interface{}{map[string]interface{}{}}}),
		"an include entry without a path reports JSON null")
}
