package topics

const RequestAppState Topic = "request_app_state"
const WriteToFile Topic = "write_data"
const Handshake Topic = "device_handshake"
const GetImages Topic = "get_images"
const PruneImages Topic = "prune_images"
const RequestTerminalSession Topic = "request_terminal_session"
const StartTerminalSession Topic = "start_terminal_session"
const StopTerminalSession Topic = "stop_terminal_session"
const GetAppLogHistory Topic = "get_app_log_history"

// QueryAppLogs is the windowed successor to GetAppLogHistory. The older topic
// keeps its exact wire contract — the Studio log viewer consumes a flat
// string[] and would break on a richer result — so range support arrives as a
// separate procedure rather than as extra arguments on that one.
const QueryAppLogs Topic = "query_app_logs"

// QueryDeviceLogs reads the agent's own log: what the DEVICE did, across every
// app. Bounded, time-windowed and severity-filtered.
const QueryDeviceLogs Topic = "query_device_logs"

const ListContainers Topic = "list_containers"

const ListEthernetDevices Topic = "list_ethernet_devices"
const UpdateIPv4Configuration Topic = "update_ipv4_config"

const ListWiFiNetworks Topic = "list_wifi_networks"
const AddWiFiConfiguration Topic = "add_wifi_configuration"
const RemoveWiFiConfiguration Topic = "remove_wifi_configuration"
const ScanWifiNetworks Topic = "scan_wifi_networks"
const SelectWiFiNetwork Topic = "select_wifi_network"
const RestartWifi Topic = "restart_wifi"
const GetIPv4Addresses Topic = "get_ipv4_addresses"

const SystemReboot Topic = "system_reboot"
const SystemShutdown Topic = "system_shutdown"
const SystemRestartAgent Topic = "system_restart_agent"

const GetStorageData Topic = "get_storage_data"

// The device's firmware through fwupd (IronFlockOS ADR-0007, lane 3).
const GetFirmwareInventory Topic = "get_firmware_inventory"
const RefreshFirmwareUpdates Topic = "refresh_firmware_updates"
const InstallFirmwareUpdate Topic = "install_firmware_update"
const InstallFirmwareUpdateProgress Topic = "install_firmware_update_progress"
const GetNetworkMetaData Topic = "get_network_metadata"

const GetAgentMetaData Topic = "get_agent_metadata"

const UpdateAgent Topic = "update_agent"

const InitDeviceTerminal Topic = "init_device_terminal"

const GetOSRelease Topic = "get_os_release"
const DownloadOSUpdate Topic = "download_os_update"
const DownloadOSUpdateProgress Topic = "download_os_update_progress"
const InstallOSUpdate Topic = "install_os_update"
const InstallOSUpdateProgress Topic = "install_os_update_progress"
const PerformOSUpdate Topic = "perform_os_update"
const PerformOSUpdateProgress Topic = "perform_os_update_progress"
const GetTunnelState Topic = "get_tunnel_state"

// CheckHostPort answers whether a candidate reserved host port is free on the
// device (called by the backend before it saves a reservation).
const CheckHostPort Topic = "check_host_port"
