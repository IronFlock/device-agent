package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"reagent/apps"
	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/store"
	"reagent/terminal"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"
	"reagent/tunnel"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Privilege gates on handlers that used to run for any caller
//
// init_device_terminal opens a shell on the device host (MAINTAIN, the UI's
// host-terminal gate), get_ipv4_addresses and get_network_metadata read the
// device's addresses (READ, like list_wifi_networks) and scan_wifi_networks
// drives the radio (NETWORK, like the other wifi writes). A denial must return
// before the side effect: no shell in the terminal registry, no Scan on the
// strict network mock.
// =============================================================================

// requestedPrivilege returns the privilege the handler asked
// reswarm.devices.check_privilege about.
func requestedPrivilege(t *testing.T, m *fakes.Messenger) string {
	t.Helper()
	calls := m.GetCallCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, topics.CheckPrivilege, calls[0].Topic)
	payload, ok := calls[0].Args[0].(common.Dict)
	require.True(t, ok)
	return fmt.Sprint(payload["privilege"])
}

// exitStrayTerminal ends the host shell a test, or a regressed gate, started
// for callerID and waits until it has left the terminal registry, so no bash
// outlives the test and the next test starts from an empty registry.
func exitStrayTerminal(t *testing.T, callerID string) func() {
	return func() {
		pT := terminal.GetPseudoTerminal(callerID)
		if pT == nil {
			return
		}
		select {
		case pT.Input <- "exit\n":
		case <-time.After(time.Second):
		}
		assert.Eventually(t, func() bool { return terminal.GetPseudoTerminal(callerID) == nil },
			20*time.Second, 20*time.Millisecond, "the host shell for %s did not exit", callerID)
	}
}

// needsHostShell skips where the test cannot start the host shell the handler
// opens: Windows opens a ConPTY PowerShell, and a minimal image may lack bash.
func needsHostShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the Windows host shell is a ConPTY PowerShell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash to open the host shell with")
	}
}

func TestInitDeviceTermPrivilegeGate(t *testing.T) {
	// The fake messenger lets a regressed gate get through Setup, so the test
	// fails on its assertions instead of a nil-session panic.
	newEx := func(m *fakes.Messenger) *External {
		return &External{
			Config:    testConfig(),
			Messenger: fakes.NewMessenger(),
			Privilege: newPrivilege(testConfig(), m),
		}
	}

	t.Run("denies a caller without MAINTAIN before starting a shell", func(t *testing.T) {
		details, m := grantPrivilege(false)
		t.Cleanup(exitStrayTerminal(t, "999"))

		res, err := newEx(m).initDeviceTerm(context.Background(), messenger.Result{Details: details})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Nil(t, terminal.GetPseudoTerminal("999"))
		assert.Equal(t, "MAINTAIN", requestedPrivilege(t, m))
	})

	t.Run("a failed privilege lookup is not a denial and starts nothing", func(t *testing.T) {
		m := fakes.NewMessenger()
		m.SetCallError(string(topics.CheckPrivilege), errors.New("rpc boom"))
		t.Cleanup(exitStrayTerminal(t, "998"))

		res, err := newEx(m).initDeviceTerm(context.Background(), messenger.Result{
			Details: common.Dict{"caller_authid": "998"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, errdefs.IsInsufficientPrivileges(err))
		assert.Nil(t, terminal.GetPseudoTerminal("998"))
	})

	t.Run("a backend call naming an account is checked as that account", func(t *testing.T) {
		// wrapDetails is the registration path: a 'system' caller that names
		// requestor_account_key is no longer trusted as 'system'.
		m := fakes.NewMessenger()
		m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
			Arguments: []interface{}{false},
		}, nil)
		// A regressed swap would open the shell as 'system'; exit that one too.
		t.Cleanup(exitStrayTerminal(t, "4242"))
		t.Cleanup(exitStrayTerminal(t, "system"))

		res, err := wrapDetails(newEx(m).initDeviceTerm)(context.Background(), messenger.Result{
			Details:     common.Dict{"caller_authid": "system"},
			ArgumentsKw: common.Dict{"requestor_account_key": 4242},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Nil(t, terminal.GetPseudoTerminal("4242"))
		assert.Nil(t, terminal.GetPseudoTerminal("system"))
		require.Len(t, m.GetCallCalls(), 1)
		assert.Equal(t, 4242, m.GetCallCalls()[0].Args[0].(common.Dict)["requestor_account_key"])
	})

	t.Run("a backend call naming an account in its first argument is checked as that account", func(t *testing.T) {
		// Decoders hand the first argument over as a plain map.
		m := fakes.NewMessenger()
		m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
			Arguments: []interface{}{false},
		}, nil)
		t.Cleanup(exitStrayTerminal(t, "4242"))
		t.Cleanup(exitStrayTerminal(t, "system"))

		res, err := wrapDetails(newEx(m).initDeviceTerm)(context.Background(), messenger.Result{
			Details:   common.Dict{"caller_authid": "system"},
			Arguments: []interface{}{map[string]interface{}{"requestor_account_key": uint64(4242)}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Nil(t, terminal.GetPseudoTerminal("system"))
		require.Len(t, m.GetCallCalls(), 1)
		assert.Equal(t, 4242, m.GetCallCalls()[0].Args[0].(common.Dict)["requestor_account_key"])
	})

	t.Run("a backend call naming no account is refused, not trusted as 'system'", func(t *testing.T) {
		// null too: JS `?? null` and Python None name nobody.
		for _, key := range []interface{}{"42x", int64(-1), uint64(0), nil} {
			for _, where := range []struct {
				name   string
				result messenger.Result
			}{
				{"in kwargs", messenger.Result{
					Details:     common.Dict{"caller_authid": "system"},
					ArgumentsKw: common.Dict{"requestor_account_key": key},
				}},
				{"in a decoded args map", messenger.Result{
					Details:   common.Dict{"caller_authid": "system"},
					Arguments: []interface{}{map[string]interface{}{"requestor_account_key": key}},
				}},
			} {
				t.Run(fmt.Sprintf("%v %s", key, where.name), func(t *testing.T) {
					m := fakes.NewMessenger()
					t.Cleanup(exitStrayTerminal(t, "system"))
					t.Cleanup(exitStrayTerminal(t, unnamedAccount))

					res, err := wrapDetails(newEx(m).initDeviceTerm)(context.Background(), where.result)

					require.Error(t, err)
					assert.Nil(t, res)
					assert.True(t, errdefs.IsInsufficientPrivileges(err))
					assert.Nil(t, terminal.GetPseudoTerminal("system"))
					assert.Nil(t, terminal.GetPseudoTerminal(unnamedAccount))
					// Refused on the device: there is no account to ask about.
					assert.Zero(t, m.GetCallCount())
				})
			}
		}
	})
}

func TestInitDeviceTermOpensTheHostShell(t *testing.T) {
	// These start a real bash on a pty: the refusals above prove nothing if an
	// allowed caller no longer gets a working terminal. Each test closes its
	// shell and waits for it to go before it ends.
	newEx := func(m *fakes.Messenger) (*External, *fakes.Messenger) {
		cfg := testConfig()
		cfg.ReswarmConfig.SerialNumber = "SER-1"
		session := fakes.NewMessenger()
		return &External{Config: cfg, Messenger: session, Privilege: newPrivilege(cfg, m)}, session
	}

	// sessionOf checks the dict the UI gets back, the session id and the three
	// topics opened for it under this device's serial, and returns it.
	sessionOf := func(t *testing.T, res *messenger.InvokeResult) common.Dict {
		t.Helper()
		require.NotNil(t, res)
		require.Len(t, res.Arguments, 1)
		got, ok := res.Arguments[0].(common.Dict)
		require.True(t, ok)
		sessionID, _ := got["sessionID"].(string)
		require.NotEmpty(t, sessionID)
		assert.Equal(t, common.Dict{
			"sessionID":   sessionID,
			"writeTopic":  "re.mgmt.SER-1.term_write." + sessionID,
			"dataTopic":   "re.mgmt.SER-1.data." + sessionID,
			"resizeTopic": "re.mgmt.SER-1.term_resize." + sessionID,
		}, got)
		return got
	}

	// outputGoesTo waits for the shell's prompt and checks that its output is
	// kept to owner (and 'system', which the appliance bridge is), with the
	// owner handed to the bridge for the cloud side.
	outputGoesTo := func(t *testing.T, session *fakes.Messenger, dataTopic string, owner string) {
		t.Helper()
		var prompt []fakes.PublishCall
		require.Eventually(t, func() bool {
			prompt = nil
			for _, call := range session.GetPublishCalls() {
				if string(call.Topic) == dataTopic {
					prompt = append(prompt, call)
				}
			}
			return len(prompt) > 0
		}, 10*time.Second, 20*time.Millisecond, "the shell printed nothing")
		for _, call := range prompt {
			assert.Equal(t, common.Dict{"acknowledge": true, "eligible_authid": []string{owner, "system"}}, call.Options)
			assert.Equal(t, common.Dict{"__bridge_eligible_authid__": []string{owner}}, call.Kwargs)
		}
	}

	t.Run("opens a shell for a caller with MAINTAIN and hands the same one back", func(t *testing.T) {
		needsHostShell(t)
		require.Nil(t, terminal.GetPseudoTerminal("4711"), "a shell left over from another test")
		m := fakes.NewMessenger()
		m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
			Arguments: []interface{}{true},
		}, nil)
		ex, session := newEx(m)
		details := common.Dict{"caller_authid": "4711"}
		t.Cleanup(exitStrayTerminal(t, "4711"))

		res, err := ex.initDeviceTerm(context.Background(), messenger.Result{Details: details})

		require.NoError(t, err)
		opened := sessionOf(t, res)
		assert.Equal(t, "MAINTAIN", requestedPrivilege(t, m))
		pT := terminal.GetPseudoTerminal("4711")
		require.NotNil(t, pT)
		assert.Equal(t, opened["sessionID"], pT.SessionID)
		// The agent listens where it told the UI to type and resize.
		require.Len(t, session.SubscribeCalls, 1)
		assert.Equal(t, topics.Topic(pT.WriteTopic), session.SubscribeCalls[0].Topic)
		require.Len(t, session.RegisterCalls, 1)
		assert.Equal(t, topics.Topic(pT.ResizeTopic), session.RegisterCalls[0].Topic)
		outputGoesTo(t, session, pT.DataTopic, "4711")

		// A second tab of the same user gets that shell, not a new one, and
		// is checked again.
		res, err = ex.initDeviceTerm(context.Background(), messenger.Result{Details: details})

		require.NoError(t, err)
		assert.Equal(t, opened, sessionOf(t, res))
		assert.Same(t, pT, terminal.GetPseudoTerminal("4711"))
		assert.Len(t, m.GetCallCalls(), 2)
		assert.Len(t, session.SubscribeCalls, 1)
	})

	t.Run("opens a shell for a backend 'system' caller without asking", func(t *testing.T) {
		// A backend call that names no account stays 'system' through
		// wrapDetails, and Privilege.Check trusts it.
		needsHostShell(t)
		require.Nil(t, terminal.GetPseudoTerminal("system"), "a shell left over from another test")
		m := fakes.NewMessenger()
		ex, _ := newEx(m)
		t.Cleanup(exitStrayTerminal(t, "system"))

		res, err := wrapDetails(ex.initDeviceTerm)(context.Background(), messenger.Result{Details: systemDetails()})

		require.NoError(t, err)
		opened := sessionOf(t, res)
		assert.Zero(t, m.GetCallCount())
		pT := terminal.GetPseudoTerminal("system")
		require.NotNil(t, pT)
		assert.Equal(t, opened["sessionID"], pT.SessionID)
	})

	t.Run("opens a shell for the account a backend call names, and keeps it to that account", func(t *testing.T) {
		// The appliance bridge path: it calls as 'system' naming the local
		// account of the cloud user it forwards.
		needsHostShell(t)
		require.Nil(t, terminal.GetPseudoTerminal("4243"), "a shell left over from another test")
		m := fakes.NewMessenger()
		m.SetCallResponse(string(topics.CheckPrivilege), messenger.Result{
			Arguments: []interface{}{true},
		}, nil)
		ex, session := newEx(m)
		t.Cleanup(exitStrayTerminal(t, "4243"))
		t.Cleanup(exitStrayTerminal(t, "system"))

		res, err := wrapDetails(ex.initDeviceTerm)(context.Background(), messenger.Result{
			Details:     common.Dict{"caller_authid": "system"},
			ArgumentsKw: common.Dict{"requestor_account_key": uint64(4243)},
		})

		require.NoError(t, err)
		opened := sessionOf(t, res)
		assert.Equal(t, "MAINTAIN", requestedPrivilege(t, m))
		assert.Nil(t, terminal.GetPseudoTerminal("system"))
		pT := terminal.GetPseudoTerminal("4243")
		require.NotNil(t, pT)
		assert.Equal(t, opened["sessionID"], pT.SessionID)
		outputGoesTo(t, session, pT.DataTopic, "4243")
	})
}

func TestRequestTerminalSessHandlerRecordsTheOwner(t *testing.T) {
	// A container shell's output and keystrokes are kept to the caller that
	// asked for it (see terminal/owner.go), so that caller is its owner.
	for _, tc := range []struct {
		name   string
		result messenger.Result
		owner  string
	}{
		{"a person", messenger.Result{Details: common.Dict{"caller_authid": "999"}}, "999"},
		{"the account a backend call names", messenger.Result{
			Details:     common.Dict{"caller_authid": "system"},
			ArgumentsKw: common.Dict{"requestor_account_key": uint64(4244)},
		}, "4244"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shells, shellsPeer := net.Pipe()
			attached, attachedPeer := net.Pipe()
			t.Cleanup(func() {
				for _, c := range []net.Conn{shells, shellsPeer, attached, attachedPeer} {
					_ = c.Close()
				}
			})
			docker := mocks.NewContainer(t)
			docker.EXPECT().ExecCommand(mock.Anything, "dev_1_shellapp", []string{"cat", "/etc/shells"}).Return(container.HijackedResponse{
				Conn:   shells,
				Reader: bufio.NewReader(strings.NewReader("/bin/sh\n/bin/bash\n")),
			}, nil).Once()
			docker.EXPECT().ExecAttach(mock.Anything, "dev_1_shellapp", "/bin/bash").Return(container.HijackedResponse{
				Conn:   attached,
				Reader: bufio.NewReader(attached),
			}, nil).Once()

			tm := terminal.NewTerminalManager(fakes.NewMessenger(), docker)
			_, m := grantPrivilege(true)
			ex := &External{
				Privilege:       newPrivilege(testConfig(), m),
				TerminalManager: &tm,
				AppManager:      appManagerWith(t, 1, "shellapp", common.DEV),
			}
			tc.result.Arguments = []interface{}{map[string]interface{}{"containerName": "dev_1_shellapp"}}

			res, err := wrapDetails(ex.requestTerminalSessHandler)(context.Background(), tc.result)

			require.NoError(t, err)
			require.Len(t, res.Arguments, 1)
			sessionID := res.Arguments[0].(common.Dict)["sessionID"].(string)
			require.Contains(t, tm.ActiveSessions, sessionID)
			assert.Equal(t, tc.owner, tm.ActiveSessions[sessionID].Owner)
			assert.Equal(t, "DEVELOP", requestedPrivilege(t, m))
		})
	}
}

// appManagerWith is an AppManager whose store holds one installed app.
func appManagerWith(t *testing.T, appKey uint64, appName string, stage common.Stage) *apps.AppManager {
	t.Helper()
	db := newLogDB(t)
	seedApp(t, db, appKey, appName, stage)
	as := store.NewAppStore(db, fakes.NewMessenger())
	return &apps.AppManager{AppStore: &as}
}

// DEVELOP on a device grants a shell in its apps' containers, never in any
// other container on the host — on an appliance, the platform's own database.
// The container mock is strict: a refused name that still reached Docker
// fails the test.
func TestRequestTerminalSessHandlerStaysWithinApps(t *testing.T) {
	for _, name := range []string{
		"ironflock-postgres-1",         // a platform container on an appliance
		"prod_9_otherapp",              // shaped like an app, but not installed
		"dev_1_otherapp",               // an installed app's key, another name
		"prod_1_shellapp",              // the installed app, but another stage
		"/dev_1_shellapp",              // not the exact name the agent gives it
		"dev_1_shellapp_compose-web-1", // a container it did not start as the app
	} {
		t.Run(name, func(t *testing.T) {
			tm := terminal.NewTerminalManager(fakes.NewMessenger(), mocks.NewContainer(t))
			ex := &External{
				Privilege:       priv(t, true),
				TerminalManager: &tm,
				AppManager:      appManagerWith(t, 1, "shellapp", common.DEV),
			}

			res, err := ex.requestTerminalSessHandler(context.Background(), messenger.Result{
				Details:   systemDetails(),
				Arguments: []interface{}{map[string]interface{}{"containerName": name}},
			})

			require.Error(t, err)
			assert.Nil(t, res)
			assert.True(t, errdefs.IsContainerNotFound(err))
			assert.Empty(t, tm.ActiveSessions)
		})
	}
}

func TestGetCurrentIPAddressesPrivilegeGate(t *testing.T) {
	t.Run("denies a caller without READ", func(t *testing.T) {
		details, m := grantPrivilege(false)
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getCurrentIPAddresses(context.Background(), messenger.Result{Details: details})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Equal(t, "READ", requestedPrivilege(t, m))
	})

	t.Run("serves a caller with READ", func(t *testing.T) {
		details, m := grantPrivilege(true)
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getCurrentIPAddresses(context.Background(), messenger.Result{Details: details})

		require.NoError(t, err)
		require.Len(t, res.Arguments, 1)
		assert.Equal(t, "READ", requestedPrivilege(t, m))
	})

	t.Run("serves a backend 'system' caller without asking", func(t *testing.T) {
		m := fakes.NewMessenger()
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getCurrentIPAddresses(context.Background(), messenger.Result{Details: systemDetails()})

		require.NoError(t, err)
		require.Len(t, res.Arguments, 1)
		assert.Zero(t, m.GetCallCount())
	})
}

func TestWifiScanHandlerPrivilegeGate(t *testing.T) {
	t.Run("denies a caller without NETWORK without scanning", func(t *testing.T) {
		// mocks.Network is strict: Scan must NOT be called here.
		net := mocks.NewNetwork(t)
		details, m := grantPrivilege(false)
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.wifiScanHandler(context.Background(), messenger.Result{Details: details})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Equal(t, "NETWORK", requestedPrivilege(t, m))
	})

	t.Run("scans for a caller with NETWORK", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().Scan().Return(nil).Once()
		details, m := grantPrivilege(true)
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.wifiScanHandler(context.Background(), messenger.Result{Details: details})

		require.NoError(t, err)
		assert.NotNil(t, res)
		assert.Equal(t, "NETWORK", requestedPrivilege(t, m))
	})

	t.Run("scans for a backend 'system' caller without asking", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().Scan().Return(nil).Once()
		m := fakes.NewMessenger()
		ex := &External{Network: net, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.wifiScanHandler(context.Background(), messenger.Result{Details: systemDetails()})

		require.NoError(t, err)
		assert.NotNil(t, res)
		assert.Zero(t, m.GetCallCount())
	})

	t.Run("propagates a scan error", func(t *testing.T) {
		net := mocks.NewNetwork(t)
		net.EXPECT().Scan().Return(errors.New("radio busy")).Once()
		ex := &External{Network: net, Privilege: priv(t, true)}

		_, err := ex.wifiScanHandler(context.Background(), messenger.Result{Details: systemDetails()})

		require.Error(t, err)
		assert.False(t, errdefs.IsInsufficientPrivileges(err))
	})
}

func TestGetNetworkDataHandlerPrivilegeGate(t *testing.T) {
	t.Run("denies a caller without READ", func(t *testing.T) {
		details, m := grantPrivilege(false)
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getNetworkDataHandler(context.Background(), messenger.Result{Details: details})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Equal(t, "READ", requestedPrivilege(t, m))
	})

	t.Run("a failed privilege lookup is not a denial and reads nothing", func(t *testing.T) {
		m := fakes.NewMessenger()
		m.SetCallError(string(topics.CheckPrivilege), errors.New("rpc boom"))
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getNetworkDataHandler(context.Background(), messenger.Result{
			Details: common.Dict{"caller_authid": "999"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, errdefs.IsInsufficientPrivileges(err))
	})

	t.Run("serves a caller with READ", func(t *testing.T) {
		details, m := grantPrivilege(true)
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getNetworkDataHandler(context.Background(), messenger.Result{Details: details})

		require.NoError(t, err)
		require.NotNil(t, res)
		assert.NotNil(t, res.Arguments)
		assert.Equal(t, "READ", requestedPrivilege(t, m))
	})

	t.Run("serves a backend 'system' caller without asking", func(t *testing.T) {
		m := fakes.NewMessenger()
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getNetworkDataHandler(context.Background(), messenger.Result{Details: systemDetails()})

		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Zero(t, m.GetCallCount())
	})
}

// =============================================================================
// execute_cmd is gone
//
// It ran any command on the device host for any caller that reached it, with
// no check, and nothing on the platform called it. The agent no longer
// registers it, so no router rule or bridge path can make it serve a call.
// =============================================================================

func TestRegisterAllOmitsExecuteCmd(t *testing.T) {
	cfg := testConfig()
	cfg.ReswarmConfig.SerialNumber = "SER-1"
	m := fakes.NewMessenger()
	ex := &External{Config: cfg, Messenger: m}

	require.NoError(t, ex.RegisterAll())

	registered := map[topics.Topic]bool{}
	for _, c := range m.RegisterCalls {
		registered[c.Topic] = true
	}
	assert.True(t, registered["re.mgmt.SER-1.init_device_terminal"])
	assert.False(t, registered["re.mgmt.SER-1.execute_cmd"])
}

// =============================================================================
// Backend-only procedures refuse everyone but the backend
//
// request_app_state deploys whatever compose, environment and registry
// credentials it is given; device_handshake, check_host_port and
// get_tunnel_state answer anyone. None checks a privilege: only the platform
// backend calls them. A released router lets any session, anonymous included,
// call anything under re.mgmt., so the agent refuses every caller whose role
// is not the backend's. The backend's own calls keep working, with or without
// an account named (request_app_state carries the app's key: 0, null or one).
// =============================================================================

// procedureMessenger keeps the handlers RegisterAll registers, so a test calls
// a procedure the way the router would.
type procedureMessenger struct {
	*fakes.Messenger

	procedures map[topics.Topic]func(context.Context, messenger.Result) (*messenger.InvokeResult, error)
}

func (m *procedureMessenger) Register(topic topics.Topic, cb func(context.Context, messenger.Result) (*messenger.InvokeResult, error), options common.Dict) error {
	m.procedures[topic] = cb
	return m.Messenger.Register(topic, cb, options)
}

func TestBackendOnlyProceduresRefuseEveryoneButTheBackend(t *testing.T) {
	backend := common.Dict{"caller_authid": "system", "caller_authrole": "system"}

	callers := []struct {
		name    string
		details common.Dict
		kwargs  common.Dict
		allowed bool
	}{
		{"an anonymous session", common.Dict{"caller_authid": "8f3a1c", "caller_authrole": "anonymous"}, nil, false},
		{"a logged-in user", common.Dict{"caller_authid": "42", "caller_authrole": "acct_manager"}, nil, false},
		{"a logged-in user naming an account", common.Dict{"caller_authid": "42", "caller_authrole": "acct_manager"},
			common.Dict{"requestor_account_key": uint64(42)}, false},
		{"a device", common.Dict{"caller_authid": "7-1234", "caller_authrole": "swarm_device"}, nil, false},
		{"an appliance uplink", common.Dict{"caller_authid": "77", "caller_authrole": "instance"}, nil, false},
		{"a caller the router does not disclose", common.Dict{}, nil, false},
		{"the backend's authid without its role", common.Dict{"caller_authid": "system"}, nil, false},

		{"the backend", backend, nil, true},
		{"the backend naming an account", backend, common.Dict{"requestor_account_key": uint64(4242)}, true},
		{"the backend naming account 0, an app's key", backend, common.Dict{"requestor_account_key": uint64(0)}, true},
		{"the backend naming null", backend, common.Dict{"requestor_account_key": nil}, true},
	}

	procedures := []struct {
		topic  topics.Topic
		args   []interface{}
		kwargs common.Dict
		// ran checks the answer of a call the procedure served.
		ran func(t *testing.T, res *messenger.InvokeResult, err error)
		// expect sets up what serving it asks of the tunnel manager.
		expect func(tm *mocks.TunnelManager)
	}{
		{
			// app_key 0 stops the handler at parsing, before it deploys.
			topic:  topics.RequestAppState,
			kwargs: common.Dict{"app_key": uint64(0)},
			ran: func(t *testing.T, res *messenger.InvokeResult, err error) {
				assert.ErrorIs(t, err, errdefs.ErrMissingFromPayload)
			},
		},
		{
			topic: topics.Handshake,
			ran: func(t *testing.T, res *messenger.InvokeResult, err error) {
				require.NoError(t, err)
				assert.Equal(t, "SER-1", res.ArgumentsKw["id"])
			},
		},
		{
			topic: topics.CheckHostPort,
			args:  []interface{}{map[string]interface{}{"app_key": uint64(5), "port": uint64(1883), "host_port": uint64(0)}},
			ran: func(t *testing.T, res *messenger.InvokeResult, err error) {
				require.NoError(t, err)
				assert.Equal(t, []interface{}{common.Dict{"available": true, "reason": ""}}, res.Arguments)
			},
		},
		{
			topic: topics.GetTunnelState,
			ran: func(t *testing.T, res *messenger.InvokeResult, err error) {
				require.NoError(t, err)
				assert.Equal(t, []interface{}{[]tunnel.TunnelState{}}, res.Arguments)
			},
			expect: func(tm *mocks.TunnelManager) {
				tm.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
			},
		},
	}

	for _, procedure := range procedures {
		for _, caller := range callers {
			t.Run(string(procedure.topic)+"/"+caller.name, func(t *testing.T) {
				cfg := testConfig()
				cfg.ReswarmConfig.SerialNumber = "SER-1"
				// Strict: a refused call that reached the tunnel manager fails it.
				tunnels := mocks.NewTunnelManager(t)
				m := &procedureMessenger{
					Messenger:  fakes.NewMessenger(),
					procedures: map[topics.Topic]func(context.Context, messenger.Result) (*messenger.InvokeResult, error){},
				}
				ex := &External{Config: cfg, Messenger: m, TunnelManager: tunnels}
				require.NoError(t, ex.RegisterAll())

				handler := m.procedures[topics.Topic("re.mgmt.SER-1."+string(procedure.topic))]
				require.NotNil(t, handler)

				kwargs := common.Dict{}
				for k, v := range procedure.kwargs {
					kwargs[k] = v
				}
				for k, v := range caller.kwargs {
					kwargs[k] = v
				}
				details := common.Dict{}
				for k, v := range caller.details {
					details[k] = v
				}
				if caller.allowed && procedure.expect != nil {
					procedure.expect(tunnels)
				}

				res, err := handler(context.Background(), messenger.Result{
					Details:     details,
					Arguments:   procedure.args,
					ArgumentsKw: kwargs,
				})

				if caller.allowed {
					procedure.ran(t, res, err)
					return
				}
				require.Error(t, err)
				assert.True(t, errdefs.IsInsufficientPrivileges(err), "%v", err)
				assert.Nil(t, res)
			})
		}
	}
}

// =============================================================================
// Only the owner starts or stops a container terminal session
//
// A container shell belongs to the caller that asked for it (see
// terminal/owner.go). Anyone else with DEVELOP who learns its session id must
// not start it (which wires its keystrokes and output up again) or stop it.
// The caller is the one wrapDetails leaves: a backend call is checked as the
// account it names, and the appliance bridge names the cloud user's account.
// =============================================================================

func TestContainerTerminalSessionStartsAndStopsOnlyForItsOwner(t *testing.T) {
	// newSession is a container shell of account 5151 whose output stays open
	// until the test ends.
	newSession := func(t *testing.T) (*External, *terminal.TerminalManager, *terminal.TerminalSession, *fakes.Messenger) {
		conn, peer := net.Pipe()
		output, outputWriter := io.Pipe()
		t.Cleanup(func() {
			_ = outputWriter.Close()
			_ = conn.Close()
			_ = peer.Close()
		})

		sessions := fakes.NewMessenger()
		tm := terminal.NewTerminalManager(sessions, nil)
		termSess := terminal.NewSession("app_1_prod", "SER-1", "5151", &container.HijackedResponse{
			Conn:   conn,
			Reader: bufio.NewReader(output),
		})
		tm.ActiveSessions[termSess.SessionID] = termSess

		// Every account holds DEVELOP: the owner check is what refuses.
		_, privileges := grantPrivilege(true)
		ex := &External{Privilege: newPrivilege(testConfig(), privileges), TerminalManager: &tm}

		return ex, &tm, termSess, sessions
	}
	started := func(sessions *fakes.Messenger, termSess *terminal.TerminalSession) bool {
		for _, call := range sessions.SubscribeCalls {
			if string(call.Topic) == termSess.WriteTopic {
				return true
			}
		}
		return false
	}
	call := func(handler RegistrationHandler, caller common.Dict, termSess *terminal.TerminalSession) (*messenger.InvokeResult, error) {
		details := common.Dict{"caller_authid": caller["caller_authid"]}
		kwargs := common.Dict{}
		if named, ok := caller["requestor_account_key"]; ok {
			kwargs["requestor_account_key"] = named
		}
		return wrapDetails(handler)(context.Background(), messenger.Result{
			Details:     details,
			ArgumentsKw: kwargs,
			Arguments: []interface{}{map[string]interface{}{
				"sessionID":      termSess.SessionID,
				"registrationID": uint64(1),
			}},
		})
	}

	others := []struct {
		name   string
		caller common.Dict
	}{
		{"another account", common.Dict{"caller_authid": "777"}},
		{"an account the owner's is a prefix of", common.Dict{"caller_authid": "51510"}},
		{"a backend call naming another account", common.Dict{"caller_authid": "system", "requestor_account_key": uint64(777)}},
	}
	for _, other := range others {
		t.Run(other.name+" may not start or stop it", func(t *testing.T) {
			ex, tm, termSess, sessions := newSession(t)

			res, err := call(ex.startTerminalSessHandler, other.caller, termSess)
			require.Error(t, err)
			assert.True(t, errdefs.IsInsufficientPrivileges(err), "%v", err)
			assert.Nil(t, res)
			assert.False(t, started(sessions, termSess), "the keystrokes were wired up for another account")

			res, err = call(ex.stopTerminalSession, other.caller, termSess)
			require.Error(t, err)
			assert.True(t, errdefs.IsInsufficientPrivileges(err), "%v", err)
			assert.Nil(t, res)
			assert.Contains(t, tm.ActiveSessions, termSess.SessionID, "another account ended the session")
		})
	}

	for _, owner := range []struct {
		name   string
		caller common.Dict
	}{
		{"the owner", common.Dict{"caller_authid": "5151"}},
		{"a backend call naming the owner", common.Dict{"caller_authid": "system", "requestor_account_key": uint64(5151)}},
		{"the backend relaying for it", common.Dict{"caller_authid": "system"}},
	} {
		t.Run(owner.name+" may start and stop it", func(t *testing.T) {
			ex, tm, termSess, sessions := newSession(t)

			_, err := call(ex.startTerminalSessHandler, owner.caller, termSess)
			require.NoError(t, err)
			assert.True(t, started(sessions, termSess))

			_, err = call(ex.stopTerminalSession, owner.caller, termSess)
			require.NoError(t, err)
			assert.NotContains(t, tm.ActiveSessions, termSess.SessionID)
		})
	}
}

// start_terminal_session ignores the registrationID in its payload. UIs send
// the id of their data subscription, which the agent used to compare with the
// ids of the router's unregister events, ids of another counter. A UI that
// sends none, or a number of another type, starts its session all the same.
func TestStartTerminalSessionIgnoresTheRegistrationID(t *testing.T) {
	for name, payload := range map[string]map[string]interface{}{
		"a uint64, as UIs send it": {"registrationID": uint64(4711)},
		"a float64":                {"registrationID": float64(4711)},
		"a string":                 {"registrationID": "4711"},
		"null":                     {"registrationID": nil},
		"none":                     {},
	} {
		t.Run(name, func(t *testing.T) {
			conn, peer := net.Pipe()
			output, outputWriter := io.Pipe()
			t.Cleanup(func() {
				_ = outputWriter.Close()
				_ = conn.Close()
				_ = peer.Close()
			})

			sessions := fakes.NewMessenger()
			tm := terminal.NewTerminalManager(sessions, nil)
			termSess := terminal.NewSession("app_1_prod", "SER-1", "5151", &container.HijackedResponse{
				Conn:   conn,
				Reader: bufio.NewReader(output),
			})
			tm.ActiveSessions[termSess.SessionID] = termSess

			_, privileges := grantPrivilege(true)
			ex := &External{Privilege: newPrivilege(testConfig(), privileges), TerminalManager: &tm}

			payload["sessionID"] = termSess.SessionID
			_, err := wrapDetails(ex.startTerminalSessHandler)(context.Background(), messenger.Result{
				Details:   common.Dict{"caller_authid": "5151"},
				Arguments: []interface{}{payload},
			})
			require.NoError(t, err)

			var subscribed []string
			for _, call := range sessions.SubscribeCalls {
				subscribed = append(subscribed, string(call.Topic))
			}
			assert.Equal(t, []string{termSess.WriteTopic}, subscribed, "the keystrokes were not wired up")
		})
	}
}

// A session's resize procedure is registered by the terminal package, past
// wrapDetails, so it reads the account a 'system' call names itself. It must
// come to the same account as wrapDetails for every caller and every way a key
// arrives: the same call may not resize a shell it could not start.
func TestTerminalResizeNamesTheAccountAsWrapDetailsDoes(t *testing.T) {
	m := &procedureMessenger{
		Messenger:  fakes.NewMessenger(),
		procedures: map[topics.Topic]func(context.Context, messenger.Result) (*messenger.InvokeResult, error){},
	}
	docker := mocks.NewContainer(t)
	docker.EXPECT().ResizeExecContainer(mock.Anything, "exec-1", mock.Anything).Return(nil).Maybe()
	tm := terminal.NewTerminalManager(m, docker)

	// Besides an ordinary account, accounts a float64 key spells differently:
	// 1e6, and the two ends of the range a float64 holds exactly.
	owners := []string{"5151", "1000000", "9007199254740991", "9007199254740992"}
	resizes := map[string]func(context.Context, messenger.Result) (*messenger.InvokeResult, error){}
	for _, owner := range owners {
		conn, peer := net.Pipe()
		output, outputWriter := io.Pipe()
		t.Cleanup(func() {
			_ = outputWriter.Close()
			_ = conn.Close()
			_ = peer.Close()
		})

		termSess := terminal.NewSession("app_1_prod", "SER-1", owner, &container.HijackedResponse{
			Conn:   conn,
			Reader: bufio.NewReader(output),
			ExecID: "exec-1",
		})
		tm.ActiveSessions[termSess.SessionID] = termSess
		require.NoError(t, tm.StartTerminalSession(termSess.SessionID, owner))
		resizes[owner] = m.procedures[topics.Topic(termSess.ResizeTopic)]
		require.NotNil(t, resizes[owner])
	}

	keys := []interface{}{
		uint64(5151), uint32(5151), int64(5151), int(5151), float64(5151), float32(5151), "5151", "05151",
		uint64(777), "777", "51510", float64(5151.5), float64(1<<53 + 2), "5151.0", "+5151", " 5151",
		uint64(0), "0", int64(-5151), "-5151", "", "abc", nil, true, []interface{}{uint64(5151)},
		float64(1e6), float32(1e6), uint64(1000000), "1000000", "1e6", "1e+06",
		float64(1<<53 - 1), float64(1 << 53), uint64(1<<53 - 1), uint64(1 << 53), "9007199254740992", "9007199254740993",
	}

	for _, owner := range owners {
		// Anyone but 'system' is checked as themselves, whatever they name.
		for _, caller := range []string{"system", owner, "777"} {
			for _, key := range keys {
				for _, where := range []string{"kwargs", "first argument", "first argument as a common.Dict"} {
					// Each call gets maps of its own: wrapDetails rewrites the details.
					call := func() messenger.Result {
						first := map[string]interface{}{"height": uint64(40), "width": uint64(120)}
						kwargs := common.Dict{}
						var firstArgument interface{} = first
						switch where {
						case "kwargs":
							kwargs["requestor_account_key"] = key
						case "first argument":
							first["requestor_account_key"] = key
						default:
							// What an in-process caller passes. The resize
							// cannot read its size, but refuses it first.
							first["requestor_account_key"] = key
							firstArgument = common.Dict(first)
						}
						return messenger.Result{
							Details:     common.Dict{"caller_authid": caller},
							ArgumentsKw: kwargs,
							Arguments:   []interface{}{firstArgument},
						}
					}

					var checkedAs interface{}
					_, err := wrapDetails(func(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
						checkedAs = response.Details["caller_authid"]
						return &messenger.InvokeResult{}, nil
					})(context.Background(), call())
					require.NoError(t, err)
					ownersCall := fmt.Sprint(checkedAs) == owner

					_, err = resizes[owner](context.Background(), call())
					assert.Equal(t, ownersCall, !errdefs.IsInsufficientPrivileges(err),
						"%s calling on %s's shell with %#v in the %s: wrapDetails checks the call as %v, the resize said %v",
						caller, owner, key, where, checkedAs, err)
				}
			}
		}
	}
}

// An argument-less call is refused. Indexing it would panic on the WAMP
// client's invocation goroutine, which nothing recovers.
func TestTerminalSessionHandlersRefuseArgumentlessCalls(t *testing.T) {
	tm := terminal.NewTerminalManager(fakes.NewMessenger(), nil)
	ex := &External{Privilege: newPrivilege(testConfig(), fakes.NewMessenger()), TerminalManager: &tm}

	for name, handler := range map[string]RegistrationHandler{
		"request_terminal_session": ex.requestTerminalSessHandler,
		"start_terminal_session":   ex.startTerminalSessHandler,
		"stop_terminal_session":    ex.stopTerminalSession,
	} {
		for _, args := range [][]interface{}{nil, {}} {
			t.Run(fmt.Sprintf("%s %#v", name, args), func(t *testing.T) {
				require.NotPanics(t, func() {
					res, err := handler(context.Background(), messenger.Result{Details: systemDetails(), Arguments: args})
					assert.Error(t, err)
					assert.Nil(t, res)
				})
			})
		}
	}
}
