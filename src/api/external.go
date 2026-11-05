package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reagent/apps"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/logging"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/network"
	"reagent/persistence"
	"reagent/privilege"
	"reagent/system"
	"reagent/terminal"
	"reagent/tunnel"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// External is the API that is meant to be used by the externally exposed WAMP topics.
// It contains all the functionality available in the reagent.
type External struct {
	Container       container.Container
	Messenger       messenger.Messenger
	LogMessenger    messenger.Messenger
	Database        persistence.Database
	TunnelManager   tunnel.TunnelManager
	Network         network.Network
	Privilege       *privilege.Privilege
	Filesystem      *filesystem.Filesystem
	System          *system.System
	AppManager      *apps.AppManager
	TerminalManager *terminal.TerminalManager
	LogManager      *logging.LogManager
	Config          *config.Config
}

// RegistrationHandler is the handler that gets executed whenever a registered topic gets called.
type RegistrationHandler = func(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error)

// ! dynamically created registrations (terminal / logger) can be found in their respective packages
func (ex *External) getTopicHandlerMap() map[topics.Topic]RegistrationHandler {
	return map[topics.Topic]RegistrationHandler{
		topics.WriteToFile:            ex.writeToFileHandler,
		topics.GetImages:              ex.getImagesHandler,
		topics.RequestTerminalSession: ex.requestTerminalSessHandler,
		topics.StartTerminalSession:   ex.startTerminalSessHandler,
		topics.StopTerminalSession:    ex.stopTerminalSession,

		topics.ListWiFiNetworks:        ex.listWiFiNetworksHandler,
		topics.AddWiFiConfiguration:    ex.addWiFiConfigurationHandler,
		topics.ScanWifiNetworks:        ex.wifiScanHandler,
		topics.RemoveWiFiConfiguration: ex.removeWifiHandler,
		topics.SelectWiFiNetwork:       ex.selectWiFiNetworkHandler,
		topics.ListEthernetDevices:     ex.listEthernetDevices,
		topics.UpdateIPv4Configuration: ex.updateIPConfigHandler,
		topics.SystemReboot:            ex.systemRebootHandler,
		topics.SystemShutdown:          ex.systemShutdownHandler,
		topics.SystemRestartAgent:      ex.systemRestartAgentHandler,
		topics.RestartWifi:             ex.wifiRebootHandler,
		topics.UpdateAgent:             ex.updateReagent,
		topics.PruneImages:             ex.pruneImageHandler,
		topics.GetAgentMetaData:        ex.getAgentMetadataHandler,
		topics.ListContainers:          ex.listContainersHandler,
		topics.GetNetworkMetaData:      ex.getNetworkDataHandler,
		topics.GetAppLogHistory:        ex.getAppLogHistoryHandler,
		topics.QueryAppLogs:            ex.queryAppLogsHandler,
		topics.QueryDeviceLogs:         ex.queryDeviceLogsHandler,

		topics.GetOSRelease:     ex.getOSReleaseHandler,
		topics.DownloadOSUpdate: ex.downloadOSUpdateHandler,
		topics.InstallOSUpdate:  ex.installOSUpdateHandler,
		topics.PerformOSUpdate:  ex.performOSUpdateHandler,

		topics.InitDeviceTerminal:   ex.initDeviceTerm,
		topics.GetIPv4Addresses:     ex.getCurrentIPAddresses,
		topics.GetStorageData:       ex.getStorageDataHandler,
		topics.GetFirmwareInventory: ex.getFirmwareInventoryHandler,

		// Backend-only: only the platform backend calls these, and it has
		// decided before it calls (request_app_state after the app's privilege
		// check, check_host_port while saving a port reservation,
		// device_handshake to refresh the device status; get_tunnel_state has
		// no caller left, the backend asks re.tunnel). The agent refuses
		// everyone else itself, see backendOnly.
		topics.RequestAppState: backendOnly(ex.requestAppStateHandler),
		topics.Handshake:       backendOnly(ex.deviceHandshakeHandler),
		topics.CheckHostPort:   backendOnly(ex.checkHostPortHandler),
		topics.GetTunnelState:  backendOnly(ex.getTunnelState),
	}
}

// backendRole is the role the router gives the platform backend's sessions,
// on the cloud and on an appliance.
const backendRole = "system"

// backendOnly refuses every caller but the platform backend. The router and
// ironflock-auth keep these procedures from browsers only under the
// wamp-authz policy: a released router lets an anonymous or logged-in session
// call anything under re.mgmt., and routers roll out apart from agents.
//
// It checks the caller's role, not its authid: wrapDetails rewrites the authid
// of a backend call that names a requestor_account_key, and request_app_state
// carries the app's key, which can be 0 or null. The role is the router's
// alone to set, it is disclosed with the authid on every path (a cluster
// forwards both), and an undisclosed caller is refused.
func backendOnly(handler RegistrationHandler) RegistrationHandler {
	return func(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
		if response.Details["caller_authrole"] != backendRole {
			return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges"))
		}

		return handler(ctx, response)
	}
}

// unnamedAccount is the caller of a backend call whose requestor_account_key
// names no account. It is not 'system', so the call is not trusted, and it is
// not an account, so every privilege check refuses it.
const unnamedAccount = "unnamed-account"

// wrapDetails decides who a call is checked as. A 'system' caller is the
// platform backend. When it acts for a person it names that person's account
// in requestor_account_key, in the kwargs or in the first argument's dict, and
// the call is checked as that account. A key that is present but is not an
// account (a positive whole number), null included, must not leave the call
// trusted as 'system': the caller becomes unnamedAccount instead. The kwargs
// decide when they carry the key at all. The backend-only handlers go by the
// caller's role (see backendOnly), so request_app_state still runs; it
// carries the app's requestor_account_key, which can be 0 or null.
func wrapDetails(handler RegistrationHandler) RegistrationHandler {
	return func(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
		if response.Details["caller_authid"] != "system" {
			return handler(ctx, response)
		}

		named, present := response.ArgumentsKw["requestor_account_key"]
		if !present && len(response.Arguments) > 0 {
			// Decoders produce plain maps; common.Dict is what in-process
			// callers pass.
			switch first := response.Arguments[0].(type) {
			case map[string]interface{}:
				named, present = first["requestor_account_key"]
			case common.Dict:
				named, present = first["requestor_account_key"]
			}
		}

		if present {
			if account, ok := accountKey(named); ok {
				response.Details["caller_authid"] = account
			} else {
				response.Details["caller_authid"] = unnamedAccount
			}
		}

		return handler(ctx, response)
	}
}

// accountKey reads an account id off the wire. The serializers disagree on
// the number type (JSON gives uint64, and float64 for 42.0 or 1e6; msgpack and
// CBOR give sized integers), and callers send it as a string too.
func accountKey(value interface{}) (uint64, bool) {
	switch number := value.(type) {
	case float64:
		if number < 1 || number > 1<<53 || number != math.Trunc(number) {
			return 0, false
		}
		return uint64(number), true
	case float32:
		return accountKey(float64(number))
	}

	key, err := strconv.ParseUint(fmt.Sprint(value), 10, 64)
	return key, err == nil && key > 0
}

// RegisterAll registers all the static topics exposed by the reagent
func (ex *External) RegisterAll() error {
	serialNumber := ex.Config.ReswarmConfig.SerialNumber
	topicHandlerMap := ex.getTopicHandlerMap()
	for topic, handler := range topicHandlerMap {
		// will register all topics, e.g.: re.mgmt.request_app_state
		fullTopic := common.BuildExternalApiTopic(serialNumber, string(topic))
		err := ex.Messenger.Register(topics.Topic(fullTopic), wrapDetails(handler), nil)
		if err != nil {
			// on reconnect we will reregister, which could cause a already exists exception
			if strings.Contains(err.Error(), "wamp.error.procedure_already_exists") {
				log.Warn().Msgf("Tried to register already existing topic: %s", fullTopic)
			} else {
				return err
			}
		}
		log.Info().Msgf("Registered topic %s on the device", fullTopic)
	}
	return nil
}
