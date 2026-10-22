package topics

type Topic string

const AgentProgress Topic = "agent_update_progress"
const BootConfig Topic = "reswarm.containers.get_boot_config"
const SetActualAppOnDeviceState Topic = "reswarm.containers.update_app_on_device"
const GetRequestedAppStates Topic = "reswarm.containers.get_requested_app_states"
const GetRegistryToken Topic = "reswarm.containers.get_registry_token" //nolint:gosec // G101: a WAMP topic name, not a credential

// Per-device HMAC key from which each app container's APP_AUTH_SECRET is
// derived (cross-app data access). Argument-free by design — the backend
// resolves the device from the session authid.
const GetAppCredKey Topic = "reswarm.devices.get_app_cred_key" //nolint:gosec // G101: a WAMP topic name, not a credential
const UpdateDeviceStatus Topic = "reswarm.devices.update_device_status"
const GetDeviceMetadata Topic = "reswarm.devices.read_device_metadata"

// const UpdateDevice Topic = "reswarm.devices.update_device"
const UpdateDeviceArchitecture Topic = "reswarm.devices.update_device_architecture"
const CheckPrivilege Topic = "reswarm.devices.check_privilege"
const SetDeviceTestament Topic = "reswarm.api.testament_device"

// Immediate garbage collection of the appliance-local appstore registry
// (registered by the registry auth service on the local router). Called by
// diskguard on the appliance host when disk runs low.
const RunRegistryGC Topic = "reswarm.registry.run_gc"

// const UpdateAppTunnel Topic = "reswarm.devices.update_app_tunnel"
const TunnelStateUpdate = "tunnel_state_update"
const ExposePort Topic = "re.tunnel.expose_port"
const ClosePort Topic = "re.tunnel.close_port"
