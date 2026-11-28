package persistence

const QuerySelectCurrentAppStateByKeyAndStage = `SELECT app_name, state, version, release_key FROM AppStates WHERE app_key = ? AND stage = ?`

const QuerySelectAllDeviceState = `SELECT interface_type, device_status FROM DeviceStates`
const QuerySelectAllAppStates = `SELECT app_name, app_key, version, release_key, stage, state, timestamp FROM AppStates`

const QuerySelectAllRequestedStates = `SELECT app_name, app_key, stage, version, present_version, newest_version, current_state,
manually_requested_state, requestor_account_key, device_owner_account_key, release_key, new_release_key, request_update, environment_variables, environment_template, ports, docker_compose, new_docker_compose, privileged, new_privileged FROM RequestedAppStates`
const QuerySelectRequestedStateByAppKeyAndStage = `SELECT app_name, app_key, stage, version, present_version, newest_version, current_state,
manually_requested_state, requestor_account_key, device_owner_account_key, release_key, new_release_key, request_update, environment_variables, environment_template, ports, docker_compose, new_docker_compose, privileged, new_privileged FROM RequestedAppStates WHERE app_key = ? AND stage = ?`
const QuerySelectAppStateByAppKeyAndStage = `SELECT app_name, app_key, version, release_key, stage, state, timestamp FROM AppStates WHERE app_key = ? AND stage = ?`
const QuerySelectLogHistoryByAppKeyStageAndType = `SELECT log FROM LogHistory WHERE app_key = ? AND stage = ?`

const QueryUpdateRequestedAppStateCurrentStateByAppKeyAndStage = `UPDATE RequestedAppStates SET current_state = ?, present_version = ?, release_key = ?, manually_requested_state = ? WHERE app_key = ? AND stage = ?`
const QueryUpdateAppStateByAppKeyAndStage = `UPDATE AppStates SET app_name = ?, state = ?, version = ?, release_key = ? WHERE app_key = ? AND stage = ?`
const QueryUpdateDeviceState = `UPDATE DeviceStates SET device_status = ?, interface_type = ?`

const QueryInsertAppStateEntry = `INSERT INTO AppStates(app_name, app_key, version, release_key, stage, state, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?)`
const QueryInsertAppStateHistoryEntry = `INSERT INTO AppStateHistory(app_name, app_key, version, release_key, stage, state, timestamp) VALUES (?, ?, ?, ?, ?, ?, ?)`
const QueryInsertDeviceStateHistoryEntry = `INSERT INTO DeviceStateHistory(interface_type, device_status, timestamp) VALUES (?, ?, ?)`

const QueryUpsertLogHistoryEntry = `INSERT INTO LogHistory(app_name, app_key, stage, log_type, log) VALUES (?, ?, ?, ?, ?) ON conflict(app_name, app_key, stage, log_type) do update set log = excluded.log`
const QueryUpdateLogHistoryEntries = `UPDATE LogHistory SET log = ? WHERE app_name = ? AND app_key = ? AND stage = ?`

const QueryUpsertRequestedStateEntry = `INSERT INTO RequestedAppStates(app_name, app_key, stage, version, present_version, newest_version, current_state, manually_requested_state, requestor_account_key, device_owner_account_key, release_key, new_release_key, request_update, environment_variables, environment_template, ports, docker_compose, new_docker_compose, privileged, new_privileged, timestamp)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) on conflict(app_name, app_key, stage) do update set
present_version = excluded.present_version,
newest_version = excluded.newest_version,
release_key = excluded.release_key,
environment_variables = excluded.environment_variables,
environment_template = excluded.environment_template,
ports = excluded.ports,
docker_compose = excluded.docker_compose,
new_docker_compose = excluded.new_docker_compose,
privileged = excluded.privileged,
new_privileged = excluded.new_privileged,
new_release_key = excluded.new_release_key,
manually_requested_state=excluded.manually_requested_state,
current_state=excluded.current_state,
requestor_account_key=excluded.requestor_account_key,
device_owner_account_key = CASE WHEN excluded.device_owner_account_key > 0 THEN excluded.device_owner_account_key ELSE device_owner_account_key END,
request_update=excluded.request_update`

const QueryDeleteAppStateByAppKeyAndStage = `DELETE FROM AppStates WHERE app_key = ? AND stage = ?`
const QueryDeleteRequestedStateByAppKeyAndStage = `DELETE FROM RequestedAppStates WHERE app_key = ? AND stage = ?`

// The image names of a requested state (present_image_name, new_image_name),
// read and written on their own, after the row: where the columns are missing
// (their single- script failed, e.g. on a full disk) the row is read and
// written as before and the names are left out.
const QuerySelectRequestedStateImageNames = `SELECT app_key, stage, present_image_name, new_image_name FROM RequestedAppStates`
const QueryUpdateRequestedStateImageNames = `UPDATE RequestedAppStates SET present_image_name = ?, new_image_name = ? WHERE app_name = ? AND app_key = ? AND stage = ?`

// The image the device really pulled or ran an app's version under
// (installed_image, installed_image_version): written by the agent alone,
// never by a sync. Read and written on their own as the names above.
const QuerySelectRequestedStateInstalledImages = `SELECT app_key, stage, installed_image, installed_image_version FROM RequestedAppStates`
const QueryUpdateRequestedStateInstalledImage = `UPDATE RequestedAppStates SET installed_image = ?, installed_image_version = ? WHERE app_key = ? AND stage = ?`
