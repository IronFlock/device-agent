package api

import (
	"context"
	"errors"
	"reagent/common"
	"reagent/config"
	"reagent/diskguard"
	"reagent/errdefs"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/network"
	"reagent/privilege"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test wiring helpers
//
// These handlers only touch ex.Privilege and ex.Network, so we build a minimal
// External with a real privilege.Privilege (driven by a fakes.Messenger) and a
// mocks.Network. testConfig() is defined in request_app_state_test.go.
// =============================================================================

// newPrivilege returns a real Privilege backed by the given fake messenger and
// the package-local testConfig().
func newPrivilege(cfg *config.Config, m messenger.Messenger) *privilege.Privilege {
	p := privilege.NewPrivilege(m, cfg)
	return &p
}

// systemDetails returns a Details dict whose caller is "system"; privilege.Check
// short-circuits to granted without any messenger Call.
func systemDetails() common.Dict {
	return common.Dict{"caller_authid": "system"}
}

// grantPrivilege returns details for a numeric caller plus a fake messenger that
// answers the check_privilege RPC with `granted`.
func grantPrivilege(granted bool) (common.Dict, *fakes.Messenger) {
	m := fakes.NewMessenger()
	m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
		Arguments: []interface{}{granted},
	}, nil)
	return common.Dict{"caller_authid": "999"}, m
}

// =============================================================================
// wrapDetails - caller_authid normalization
// =============================================================================

func TestWrapDetails(t *testing.T) {
	// capture handler records the details it was invoked with.
	newCapture := func() (RegistrationHandler, *messenger.Result) {
		var seen messenger.Result
		h := func(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
			seen = response
			return &messenger.InvokeResult{}, nil
		}
		return h, &seen
	}

	t.Run("replaces system authid using requestor_account_key from kwargs", func(t *testing.T) {
		h, seen := newCapture()
		wrapped := wrapDetails(h)

		_, err := wrapped(context.Background(), messenger.Result{
			Details:     common.Dict{"caller_authid": "system"},
			ArgumentsKw: common.Dict{"requestor_account_key": "42"},
		})

		require.NoError(t, err)
		assert.Equal(t, uint64(42), seen.Details["caller_authid"])
	})

	t.Run("replaces system authid using requestor_account_key from args dict", func(t *testing.T) {
		h, seen := newCapture()
		wrapped := wrapDetails(h)

		_, err := wrapped(context.Background(), messenger.Result{
			Details:   common.Dict{"caller_authid": "system"},
			Arguments: []interface{}{common.Dict{"requestor_account_key": "7"}},
		})

		require.NoError(t, err)
		assert.Equal(t, uint64(7), seen.Details["caller_authid"])
	})

	t.Run("leaves system authid untouched when no requestor key present", func(t *testing.T) {
		h, seen := newCapture()
		wrapped := wrapDetails(h)

		_, err := wrapped(context.Background(), messenger.Result{
			Details: common.Dict{"caller_authid": "system"},
		})

		require.NoError(t, err)
		assert.Equal(t, "system", seen.Details["caller_authid"])
	})

	t.Run("leaves non-system authid untouched", func(t *testing.T) {
		h, seen := newCapture()
		wrapped := wrapDetails(h)

		_, err := wrapped(context.Background(), messenger.Result{
			Details:     common.Dict{"caller_authid": "123"},
			ArgumentsKw: common.Dict{"requestor_account_key": "42"},
		})

		require.NoError(t, err)
		assert.Equal(t, "123", seen.Details["caller_authid"])
	})

	t.Run("replaces system authid using requestor_account_key from a decoded args map", func(t *testing.T) {
		// Decoders hand the first argument over as a plain map, not common.Dict.
		h, seen := newCapture()
		wrapped := wrapDetails(h)

		_, err := wrapped(context.Background(), messenger.Result{
			Details:   common.Dict{"caller_authid": "system"},
			Arguments: []interface{}{map[string]interface{}{"requestor_account_key": uint64(4242)}},
		})

		require.NoError(t, err)
		assert.Equal(t, uint64(4242), seen.Details["caller_authid"])
	})

	t.Run("reads every wire form of an account", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			key  interface{}
			want uint64
		}{
			{"string", "42", 42},
			{"JSON integer", uint64(42), 42},
			{"msgpack integer", int64(42), 42},
			{"sized integer", int32(42), 42},
			{"float 42.0", float64(42), 42},
			{"float printed as an exponent", float64(1234567), 1234567},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, seen := newCapture()

				_, err := wrapDetails(h)(context.Background(), messenger.Result{
					Details:     common.Dict{"caller_authid": "system"},
					ArgumentsKw: common.Dict{"requestor_account_key": tc.key},
				})

				require.NoError(t, err)
				assert.Equal(t, tc.want, seen.Details["caller_authid"])
			})
		}
	})

	t.Run("a named key that is no account is no longer trusted as system", func(t *testing.T) {
		// The handler still runs (backend-only handlers check nobody), but as
		// a caller every privilege check refuses.
		for _, tc := range []struct {
			name string
			key  interface{}
		}{
			{"trailing garbage", "42x"},
			{"negative", int64(-1)},
			{"negative string", "-1"},
			{"zero", uint64(0)},
			{"zero string", "0"},
			{"fraction", float64(42.5)},
			{"empty string", ""},
			{"the word system", "system"},
			{"a list", []interface{}{uint64(42)}},
			{"a float past the exact integers", float64(1e20)},
			{"null", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, where := range []struct {
					name   string
					result messenger.Result
				}{
					{"in kwargs", messenger.Result{
						Details:     common.Dict{"caller_authid": "system"},
						ArgumentsKw: common.Dict{"requestor_account_key": tc.key},
					}},
					{"in a decoded args map", messenger.Result{
						Details:   common.Dict{"caller_authid": "system"},
						Arguments: []interface{}{map[string]interface{}{"requestor_account_key": tc.key}},
					}},
				} {
					h, seen := newCapture()

					_, err := wrapDetails(h)(context.Background(), where.result)

					require.NoError(t, err, where.name)
					assert.Equal(t, unnamedAccount, seen.Details["caller_authid"], where.name)
				}
			})
		}
	})

	t.Run("an explicit null in the kwargs names nobody, whatever the args map says", func(t *testing.T) {
		// JS `?? null` and Python None send a key that is there but empty. It
		// must not leave the call trusted as 'system', and the kwargs decide
		// once they carry the key.
		for _, first := range []interface{}{
			map[string]interface{}{"requestor_account_key": "9"},
			common.Dict{"requestor_account_key": "9"},
			map[string]interface{}{"other": "value"},
		} {
			h, seen := newCapture()

			_, err := wrapDetails(h)(context.Background(), messenger.Result{
				Details:     common.Dict{"caller_authid": "system"},
				ArgumentsKw: common.Dict{"requestor_account_key": nil},
				Arguments:   []interface{}{first},
			})

			require.NoError(t, err)
			assert.Equal(t, unnamedAccount, seen.Details["caller_authid"], "%#v", first)
		}
	})

	t.Run("an explicit null in an in-process args dict names nobody", func(t *testing.T) {
		h, seen := newCapture()

		_, err := wrapDetails(h)(context.Background(), messenger.Result{
			Details:   common.Dict{"caller_authid": "system"},
			Arguments: []interface{}{common.Dict{"requestor_account_key": nil}},
		})

		require.NoError(t, err)
		assert.Equal(t, unnamedAccount, seen.Details["caller_authid"])
	})

	t.Run("ignores args dict missing requestor key", func(t *testing.T) {
		h, seen := newCapture()
		wrapped := wrapDetails(h)

		_, err := wrapped(context.Background(), messenger.Result{
			Details:   common.Dict{"caller_authid": "system"},
			Arguments: []interface{}{common.Dict{"other": "value"}},
		})

		require.NoError(t, err)
		assert.Equal(t, "system", seen.Details["caller_authid"])
	})
}

// Privilege.Check answers 'system' and accounts only. Anything else, such as
// the caller wrapDetails puts in for a key that names no account, is refused
// on the device, without a lookup and without an error the caller would see.
func TestPrivilegeCheckRefusesCallersThatAreNoAccount(t *testing.T) {
	for _, caller := range []string{unnamedAccount, "0", "-1", "42x", "12-34"} {
		t.Run(caller, func(t *testing.T) {
			m := fakes.NewMessenger()

			granted, err := newPrivilege(testConfig(), m).Check("READ", common.Dict{"caller_authid": caller})

			require.NoError(t, err)
			assert.False(t, granted)
			assert.Zero(t, m.GetCallCount())
		})
	}

	t.Run("an account is looked up", func(t *testing.T) {
		details, m := grantPrivilege(true)

		granted, err := newPrivilege(testConfig(), m).Check("READ", details)

		require.NoError(t, err)
		assert.True(t, granted)
		assert.Equal(t, "READ", requestedPrivilege(t, m))
	})
}

// =============================================================================
// getStorageDataHandler - storage/system stats shaping + privilege gate
// =============================================================================

func TestGetStorageDataHandler(t *testing.T) {
	t.Run("returns stats dict when privileged", func(t *testing.T) {
		ex := &External{Config: testConfig(), Privilege: priv(t, true)}

		res, err := ex.getStorageDataHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, res.Arguments, 1)

		dict, ok := res.Arguments[0].(common.Dict)
		require.True(t, ok)

		// All documented stat keys must be present.
		for _, key := range []string{
			"cpu_count", "cpu_usage",
			"memory_total", "memory_used", "memory_available",
			"storage_total", "storage_used", "storage_free",
			"docker_apps_total", "docker_apps_used", "docker_apps_free",
			"docker_apps_mounted",
		} {
			_, present := dict[key]
			assert.Contains(t, dict, key, "missing key %q", key)
			_ = present
		}
	})

	t.Run("carries the disk guard's filesystems and disk_level", func(t *testing.T) {
		// A report-only guard on the real disk: reading only, nothing filled.
		diskguard.New(nil, diskguard.Config{AgentDir: t.TempDir()})
		ex := &External{Config: testConfig(), Privilege: priv(t, true)}

		res, err := ex.getStorageDataHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		dict, ok := res.Arguments[0].(common.Dict)
		require.True(t, ok)
		filesystems, ok := dict["filesystems"].([]common.Dict)
		require.True(t, ok, "missing filesystems list")
		require.NotEmpty(t, filesystems)
		for _, key := range []string{"mount", "roles", "total", "used", "free", "inodes_total", "inodes_free", "warn_free", "emergency_free", "level"} {
			assert.Contains(t, filesystems[0], key)
		}
		assert.Contains(t, []any{"ok", "warn", "emergency"}, dict["disk_level"])
	})

	t.Run("returns insufficient privileges error when denied", func(t *testing.T) {
		details, m := grantPrivilege(false)
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getStorageDataHandler(context.Background(), messenger.Result{
			Details: details,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})

	t.Run("propagates privilege check error", func(t *testing.T) {
		m := fakes.NewMessenger()
		m.SetCallError(string(topics.CheckPrivilege), errors.New("rpc boom"))
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getStorageDataHandler(context.Background(), messenger.Result{
			Details: common.Dict{"caller_authid": "999"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// listEthernetDevices - EthernetDevice -> Dict shaping
// =============================================================================

func TestListEthernetDevices(t *testing.T) {
	t.Run("shapes ethernet devices into dicts", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		devices := []network.EthernetDevice{
			{
				InterfaceName:   "eth0",
				MAC:             "aa:bb:cc:dd:ee:ff",
				Method:          "auto",
				IPv4AddressData: []network.IPv4AddressData{{Address: "10.0.0.2", Prefix: 24}},
				IPv6AddressData: []network.IPv6AddressData{{Address: "fe80::1", Prefix: 64}},
			},
			{
				InterfaceName: "eth1",
				MAC:           "11:22:33:44:55:66",
				Method:        "manual",
			},
		}
		net.EXPECT().ListEthernetDevices().Return(devices, nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.listEthernetDevices(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.Len(t, res.Arguments, 2)

		first, ok := res.Arguments[0].(common.Dict)
		require.True(t, ok)
		assert.Equal(t, "eth0", first["interfaceName"])
		assert.Equal(t, "aa:bb:cc:dd:ee:ff", first["mac"])
		assert.Equal(t, "auto", first["method"])
		assert.Equal(t, devices[0].IPv4AddressData, first["ipv4"])
		assert.Equal(t, devices[0].IPv6AddressData, first["ipv6"])

		second := res.Arguments[1].(common.Dict)
		assert.Equal(t, "eth1", second["interfaceName"])
		assert.Equal(t, "manual", second["method"])
	})

	t.Run("returns empty slice when there are no devices", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().ListEthernetDevices().Return([]network.EthernetDevice{}, nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.listEthernetDevices(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		assert.Empty(t, res.Arguments)
	})

	t.Run("propagates network error", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().ListEthernetDevices().Return(nil, errors.New("nm down")).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.listEthernetDevices(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("denies unprivileged caller without touching the network", func(t *testing.T) {
		// mocks.Network is strict: ListEthernetDevices must NOT be called here.
		net := mocks.NewNetwork(t)

		details, m := grantPrivilege(false)
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.listEthernetDevices(context.Background(), messenger.Result{
			Details: details,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// listWiFiNetworksHandler - WiFi -> Dict shaping + active-config kwargs
// =============================================================================

func TestListWiFiNetworksHandler(t *testing.T) {
	t.Run("shapes wifi networks and attaches active ip config", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		wifis := []network.WiFi{
			{
				MAC:          "aa:aa:aa:aa:aa:aa",
				SSID:         "HomeNet",
				Channel:      6,
				Signal:       70,
				SecurityType: "wpa-psk",
				Frequency:    2437,
				Known:        true,
				Current:      true,
			},
		}
		ipv4 := []network.IPv4AddressData{{Address: "192.168.1.5", Prefix: 24}}
		ipv6 := []network.IPv6AddressData{{Address: "fe80::2", Prefix: 64}}

		net.EXPECT().ListWifiNetworks().Return(wifis, nil).Once()
		net.EXPECT().GetActiveWirelessDeviceConfig().Return(ipv4, ipv6, nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.listWiFiNetworksHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.Len(t, res.Arguments, 1)

		w := res.Arguments[0].(common.Dict)
		assert.Equal(t, "aa:aa:aa:aa:aa:aa", w["mac"])
		assert.Equal(t, "HomeNet", w["ssid"])
		assert.Equal(t, uint32(6), w["channel"])
		assert.Equal(t, uint8(70), w["signal"])
		assert.Equal(t, "wpa-psk", w["security"])
		assert.Equal(t, uint32(2437), w["frequency"])
		assert.Equal(t, true, w["known"])
		assert.Equal(t, true, w["current"])

		require.NotNil(t, res.ArgumentsKw)
		assert.Equal(t, ipv4, res.ArgumentsKw["ipv4"])
		assert.Equal(t, ipv6, res.ArgumentsKw["ipv6"])
	})

	t.Run("propagates wifi list error", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().ListWifiNetworks().Return(nil, errors.New("scan failed")).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.listWiFiNetworksHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("denies unprivileged caller", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		details, m := grantPrivilege(false)
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.listWiFiNetworksHandler(context.Background(), messenger.Result{
			Details: details,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// addWiFiConfigurationHandler - WiFi credential payload parsing
// =============================================================================

func TestAddWiFiConfigurationHandler(t *testing.T) {
	t.Run("parses full payload and forwards credentials", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		expected := network.WiFiCredentials{
			Ssid:         "MyNet",
			Passwd:       "secret",
			Priority:     5,
			SecurityType: "wpa-psk",
		}
		net.EXPECT().AddWiFi("aa:bb:cc:dd:ee:ff", expected).Return(nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.addWiFiConfigurationHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"ssid":     "MyNet",
				"mac":      "aa:bb:cc:dd:ee:ff",
				"security": "wpa-psk",
				"password": "secret",
				"priority": uint64(5),
			}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("parses payload without optional mac and security", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		expected := network.WiFiCredentials{
			Ssid:     "OpenNet",
			Passwd:   "pw",
			Priority: 1,
		}
		// mac defaults to "" when absent.
		net.EXPECT().AddWiFi("", expected).Return(nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.addWiFiConfigurationHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"ssid":     "OpenNet",
				"password": "pw",
				"priority": uint64(1),
			}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("propagates AddWiFi error", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().AddWiFi("", network.WiFiCredentials{Ssid: "N", Passwd: "p", Priority: 2}).
			Return(errors.New("nm error")).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.addWiFiConfigurationHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"ssid":     "N",
				"password": "p",
				"priority": uint64(2),
			}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	parseErrCases := []struct {
		name    string
		payload map[string]interface{}
	}{
		{
			name: "bad mac type",
			payload: map[string]interface{}{
				"ssid": "N", "password": "p", "priority": uint64(1),
				"mac": 123,
			},
		},
		{
			name: "bad security type",
			payload: map[string]interface{}{
				"ssid": "N", "password": "p", "priority": uint64(1),
				"security": 123,
			},
		},
		{
			name: "bad ssid type",
			payload: map[string]interface{}{
				"ssid": 123, "password": "p", "priority": uint64(1),
			},
		},
		{
			name: "bad password type",
			payload: map[string]interface{}{
				"ssid": "N", "password": 123, "priority": uint64(1),
			},
		},
		{
			name: "bad priority type",
			payload: map[string]interface{}{
				"ssid": "N", "password": "p", "priority": "high",
			},
		},
	}

	for _, tc := range parseErrCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			// Network must not be touched on a parse error: a bare mock fails
			// the test if any method is called.
			net := mocks.NewNetwork(t)
			ex := &External{Network: net, Privilege: priv(t, true)}

			res, err := ex.addWiFiConfigurationHandler(context.Background(), messenger.Result{
				Details:   systemDetails(),
				Arguments: []interface{}{tc.payload},
			})

			require.Error(t, err)
			assert.Nil(t, res)
		})
	}

	t.Run("rejects empty args", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.addWiFiConfigurationHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects non-dict first arg", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.addWiFiConfigurationHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{"not-a-dict"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})
}

// =============================================================================
// removeWifiHandler - ssid parsing
// =============================================================================

func TestRemoveWifiHandler(t *testing.T) {
	t.Run("parses ssid and removes it", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().RemoveWifi("GoneNet").Return(nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.removeWifiHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"ssid": "GoneNet"}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("rejects empty args", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.removeWifiHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects non-dict arg", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.removeWifiHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{42},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects bad ssid type", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.removeWifiHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"ssid": 123}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("denies unprivileged caller", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		details, m := grantPrivilege(false)
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.removeWifiHandler(context.Background(), messenger.Result{
			Details:   details,
			Arguments: []interface{}{map[string]interface{}{"ssid": "x"}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// selectWiFiNetworkHandler - ssid+mac parsing
// =============================================================================

func TestSelectWiFiNetworkHandler(t *testing.T) {
	t.Run("parses ssid and mac then activates", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().ActivateWiFi("aa:bb:cc:dd:ee:ff", "PickMe").Return(nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.selectWiFiNetworkHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"ssid": "PickMe",
				"mac":  "aa:bb:cc:dd:ee:ff",
			}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("propagates ActivateWiFi error", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().ActivateWiFi("m", "s").Return(errors.New("bad password")).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.selectWiFiNetworkHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"ssid": "s",
				"mac":  "m",
			}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects missing mac", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.selectWiFiNetworkHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"ssid": "s"}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects bad ssid type", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.selectWiFiNetworkHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"ssid": 1, "mac": "m"}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects empty args", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.selectWiFiNetworkHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})
}

// =============================================================================
// updateIPConfigHandler - method/mac/interface/ipv4/prefix parsing
// =============================================================================

func TestUpdateIPConfigHandler(t *testing.T) {
	t.Run("auto method enables DHCP", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().EnableDHCP("aa:bb", "eth0").Return(nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"method":        "auto",
				"mac":           "aa:bb",
				"interfaceName": "eth0",
			}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("manual method sets static ipv4 address", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().SetIPv4Address("aa:bb", "eth0", "10.0.0.9", uint32(24)).Return(nil).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"method":        "manual",
				"mac":           "aa:bb",
				"interfaceName": "eth0",
				"ipv4":          "10.0.0.9",
				"prefix":        uint64(24),
			}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
	})

	t.Run("propagates SetIPv4Address error", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().SetIPv4Address("m", "i", "1.2.3.4", uint32(8)).
			Return(errors.New("set failed")).Once()

		ex := &External{Network: net, Privilege: priv(t, true)}

		_, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{map[string]interface{}{
				"method":        "manual",
				"mac":           "m",
				"interfaceName": "i",
				"ipv4":          "1.2.3.4",
				"prefix":        uint64(8),
			}},
		})

		// The static path returns a non-nil (empty) result alongside the error.
		require.Error(t, err)
	})

	t.Run("rejects nil args", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		res, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: nil,
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects an empty argument list", func(t *testing.T) {
		// Indexed, it panicked the client's invocation goroutine, which
		// nothing recovers, and took the agent down.
		net := mocks.NewNetwork(t)
		ex := &External{Network: net, Privilege: priv(t, true)}

		require.NotPanics(t, func() {
			res, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
				Details:   systemDetails(),
				Arguments: []interface{}{},
			})

			require.Error(t, err)
			assert.Nil(t, res)
		})
	})

	parseErrCases := []struct {
		name    string
		payload map[string]interface{}
	}{
		{
			name:    "bad method type",
			payload: map[string]interface{}{"method": 1, "mac": "m", "interfaceName": "i"},
		},
		{
			name:    "bad mac type",
			payload: map[string]interface{}{"method": "manual", "mac": 1, "interfaceName": "i"},
		},
		{
			name:    "bad interface type",
			payload: map[string]interface{}{"method": "manual", "mac": "m", "interfaceName": 1},
		},
		{
			name: "bad ipv4 type",
			payload: map[string]interface{}{
				"method": "manual", "mac": "m", "interfaceName": "i", "ipv4": 1, "prefix": uint64(24),
			},
		},
		{
			name: "bad prefix type",
			payload: map[string]interface{}{
				"method": "manual", "mac": "m", "interfaceName": "i", "ipv4": "1.2.3.4", "prefix": "24",
			},
		},
	}

	for _, tc := range parseErrCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			net := mocks.NewNetwork(t)
			ex := &External{Network: net, Privilege: priv(t, true)}

			res, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
				Details:   systemDetails(),
				Arguments: []interface{}{tc.payload},
			})

			require.Error(t, err)
			assert.Nil(t, res)
		})
	}

	t.Run("denies unprivileged caller", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		details, m := grantPrivilege(false)
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.updateIPConfigHandler(context.Background(), messenger.Result{
			Details: details,
			Arguments: []interface{}{map[string]interface{}{
				"method": "auto", "mac": "m", "interfaceName": "i",
			}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// priv builds a real Privilege wired to a fake messenger that answers the
// check_privilege RPC with `granted`. A "system" caller short-circuits to
// granted without an RPC, but tests pass systemDetails() for the happy path;
// this helper covers the cases where a numeric caller is used.
func priv(t *testing.T, granted bool) *privilege.Privilege {
	t.Helper()
	m := fakes.NewMessenger()
	m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
		Arguments: []interface{}{granted},
	}, nil)
	return newPrivilege(testConfig(), m)
}
