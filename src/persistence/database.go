package persistence

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"reagent/common"
	"reagent/config"
	"reagent/messenger"
	"reagent/safe"
	"reagent/system"
	"runtime"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	_ "modernc.org/sqlite"
)

type AppStateDatabase struct {
	db          *sql.DB
	config      *config.Config
	updateQueue chan func()
}

const (
	driver      = "sqlite"
	cacheShared = "cache=shared"
	busyTimeout = "_busy_timeout=2500"
	journalMode = "_journal_mode=WAL"
	//syncOff     = "_synchronous=OFF"
	maxOpenConn = 1
)

func NewSQLiteDb(config *config.Config) (*AppStateDatabase, error) {
	databaseFileName := config.CommandLineArguments.DatabaseFileName

	firstLetter := databaseFileName[0:1]

	if firstLetter != "/" && firstLetter != "." && runtime.GOOS != "windows" {
		databaseFileName += "./"
	}

	connectionString := fmt.Sprintf("%s?%s&%s&%s", databaseFileName, cacheShared, busyTimeout, journalMode)
	log.Debug().Msgf("Setup database with %s as connection string", connectionString)
	db, err := sql.Open(driver, connectionString)
	if err != nil {
		return nil, err
	}

	// SQLite cannot handle concurrent reads/writes, so we limit sqlite to one connection. https://github.com/mattn/go-sqlite3/issues/274
	// will not effect performance noticably, even for large amounts of operations: https://stackoverflow.com/questions/35804884/sqlite-concurrent-writing-performance/35805826
	db.SetMaxOpenConns(maxOpenConn)

	return &AppStateDatabase{db: db, config: config, updateQueue: make(chan func())}, nil
}

func (sqlite *AppStateDatabase) Close() error {
	return sqlite.db.Close()
}

//go:embed update-scripts/*
var scriptFiles embed.FS

const scriptsDir = "update-scripts"

func (sqlite *AppStateDatabase) QueueTask(task func()) {
	safe.Go(func() {
		sqlite.updateQueue <- task
	})
}

func (sqlite *AppStateDatabase) Init() error {
	scriptFiles, err := fs.ReadDir(scriptFiles, scriptsDir)
	if err != nil {
		return err
	}

	for _, file := range scriptFiles {
		fileName := file.Name()
		log.Debug().Msgf("Executing Database script: %s", fileName)

		filePath := scriptsDir + "/" + fileName
		if strings.Contains(fileName, "single-") {
			err := sqlite.execute(filePath) // ignore error for single run scripts
			// Only log warning if it's not a "duplicate column" error (expected for already-executed scripts)
			if err != nil && !strings.Contains(err.Error(), "duplicate column") {
				log.Warn().Err(err).Msgf("Failed to execute %s database script", fileName)
			}
		} else {
			err := sqlite.execute(filePath)
			if err != nil {
				return err
			}
		}

	}

	safe.Go(func() {
		for task := range sqlite.updateQueue {
			task()
		}
	})

	return nil
}

func (ast *AppStateDatabase) UpsertAppState(app *common.App, newState common.AppState) (common.Timestamp, error) {
	app.StateLock.Lock()

	selectStatement, err := ast.db.Prepare(QuerySelectCurrentAppStateByKeyAndStage)
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	rows, err := selectStatement.Query(app.AppKey, app.Stage)
	if err != nil {
		return "", err
	}
	hasResult := rows.Next() // only get first result since there should only be one

	if !hasResult {
		err := rows.Close()
		if err != nil {
			app.StateLock.Unlock()
			return "", err
		}

		app.StateLock.Unlock()
		return ast.insertAppState(app)
	}

	var curName string
	var curState string
	var curVersion string
	var curReleaseKey uint64
	err = rows.Scan(&curName, &curState, &curVersion, &curReleaseKey)
	if err != nil {
		app.StateLock.Unlock()
		return "", rows.Close()
	}

	// The name counts too: the backend renamed the app behind this key (a
	// reused key after a backend reset), and the orphan sweep derives the
	// expected container name from this row.
	if curName == app.AppName && curState == string(newState) && curVersion == app.Version && curReleaseKey == app.ReleaseKey {
		err := rows.Close()
		if err != nil {
			app.StateLock.Unlock()
			return "", err
		}

		// Silently do nothing if state is already the same
		app.StateLock.Unlock()
		return "", nil
	}

	err = rows.Close()
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	// First add new entry in history
	insertStatement, err := ast.db.Prepare(QueryInsertAppStateHistoryEntry) // Prepare statement.
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	historyTimestamp := time.Now().Format(time.RFC3339)
	_, err = insertStatement.Exec(app.AppName, app.AppKey, app.Version, app.ReleaseKey, app.Stage, curState, historyTimestamp)
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	err = insertStatement.Close()
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	// Update current app state
	updateStatement, err := ast.db.Prepare(QueryUpdateAppStateByAppKeyAndStage) // Prepare statement.
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}
	_, err = updateStatement.Exec(app.AppName, newState, app.Version, app.ReleaseKey, app.AppKey, app.Stage)
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	err = updateStatement.Close()
	if err != nil {
		app.StateLock.Unlock()
		return "", err
	}

	// Update RequestedAppState
	requestedState, err := ast.GetRequestedState(app.AppKey, app.Stage)
	if err == nil {
		// if true: when it reconnects, it can try to let the database know it is now a different version as the remote database
		// note: this is only neccessary because we do not force users to update to the latest version
		// The database will then check if this version is actually the latest version, if not it will request another update
		requestedState.CurrentState = newState
		requestedState.RequestedState = app.RequestedState
		requestedState.PresentVersion = app.Version
		requestedState.ReleaseKey = app.ReleaseKey

		err = ast.UpsertRequestedStateChange(requestedState)
		if err != nil {
			app.StateLock.Unlock()
			return "", err
		}
	} else {
		log.Debug().Msgf("Requested app state for %d (%s) was not found\n", app.AppKey, app.Stage)
	}

	timestamp := common.Timestamp(historyTimestamp)

	app.StateLock.Unlock()

	return timestamp, nil
}

func (ast *AppStateDatabase) GetAppState(appKey uint64, stage common.Stage) (*common.App, error) {
	preppedStatement, err := ast.db.Prepare(QuerySelectAppStateByAppKeyAndStage)
	if err != nil {
		return &common.App{}, err
	}

	rows, err := preppedStatement.Query(appKey, stage)
	if err != nil {
		return &common.App{}, err
	}

	hasRow := rows.Next()
	if !hasRow {
		if err != nil {
			_ = rows.Close()
			return &common.App{}, err
		}

		_ = rows.Close()
		return nil, nil
	}

	app := &common.App{}
	err = rows.Scan(&app.AppName, &app.AppKey, &app.Version, &app.ReleaseKey, &app.Stage, &app.CurrentState, &app.LastUpdated)
	if err != nil {
		_ = rows.Close()
		return &common.App{}, err
	}

	err = rows.Close()
	if err != nil {
		return &common.App{}, err
	}

	requestedState, err := ast.GetRequestedState(app.AppKey, app.Stage)
	if err == nil {
		// neccessary to update remote app state (need to know e.g. who to publish updates to)
		app.RequestorAccountKey = requestedState.RequestorAccountKey
		app.RequestedState = requestedState.RequestedState
	}

	return app, nil
}

func (ast *AppStateDatabase) GetAppStates() ([]*common.App, error) {
	rows, err := ast.db.Query(QuerySelectAllAppStates)
	if err != nil {
		return nil, err
	}

	apps := []*common.App{}
	for rows.Next() {
		app := &common.App{}
		err = rows.Scan(&app.AppName, &app.AppKey, &app.Version, &app.ReleaseKey, &app.Stage, &app.CurrentState, &app.LastUpdated)
		if err != nil {
			return nil, err
		}

		apps = append(apps, app)
	}

	err = rows.Close()
	if err != nil {
		return nil, err
	}

	requestedStates, err := ast.GetRequestedStates()
	if err != nil {
		return nil, err
	}

	for _, requestedState := range requestedStates {
		for appIndex := range apps {
			app := apps[appIndex]

			if app.AppKey == requestedState.AppKey && app.Stage == requestedState.Stage {
				// neccessary to update remote app state (need to know e.g. who to publish updates to)
				app.RequestorAccountKey = requestedState.RequestorAccountKey
				app.RequestedState = requestedState.RequestedState
			}

		}
	}

	return apps, nil
}

// insertAppState inserts a new AppState entry into the database and returns the timestamp
func (ast *AppStateDatabase) insertAppState(app *common.App) (common.Timestamp, error) {
	insertStatement, err := ast.db.Prepare(QueryInsertAppStateEntry) // Prepare statement.
	if err != nil {
		return "", err
	}

	defer func() { _ = insertStatement.Close() }()

	timestamp := time.Now().Format(time.RFC3339)

	app.StateLock.Lock()
	appName := app.AppName
	appKey := app.AppKey
	version := app.Version
	releaseKey := app.ReleaseKey
	stage := app.Stage
	currentState := app.CurrentState
	app.StateLock.Unlock()

	_, err = insertStatement.Exec(appName, appKey, version, releaseKey, stage, currentState, timestamp)
	if err != nil {
		return "", err
	}

	return common.Timestamp(timestamp), nil
}

func (ast *AppStateDatabase) UpdateDeviceStatus(status messenger.DeviceStatus) error {
	return ast.updateDeviceState(status, "")
}

func (ast *AppStateDatabase) UpdateNetworkInterface(intf system.NetworkInterface) error {
	return ast.updateDeviceState("", intf)
}

func (ast *AppStateDatabase) GetRequestedStates() ([]common.TransitionPayload, error) {
	rows, err := ast.db.Query(QuerySelectAllRequestedStates)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	payloads := []common.TransitionPayload{}
	for rows.Next() {
		var appName string
		var appKey uint64
		// var deviceToAppKey uint64
		var requestorAccountKey uint64
		var deviceOwnerAccountKey uint64
		var releaseKey uint64
		var requestUpdate bool
		var newReleaseKey uint64
		var stage common.Stage
		var version string
		var presentVersion string
		var newestVersion string
		var environmentVariablesString string
		var environmentTemplateString *string
		var portsString *string
		var dockerComposeString *string
		var newDockerComposeString *string
		var currentState common.AppState
		var requestedState common.AppState

		err = rows.Scan(&appName, &appKey, &stage, &version, &presentVersion, &newestVersion, &currentState, &requestedState, &requestorAccountKey, &deviceOwnerAccountKey, &releaseKey, &newReleaseKey, &requestUpdate, &environmentVariablesString, &environmentTemplateString, &portsString, &dockerComposeString, &newDockerComposeString)
		if err != nil {
			return nil, err
		}

		payload := common.BuildTransitionPayload(appKey, appName, requestorAccountKey, stage, currentState, requestedState, releaseKey, newReleaseKey, ast.config)
		payload.DeviceOwnerAccountKey = deviceOwnerAccountKey
		payload.Version = version
		payload.NewestVersion = newestVersion
		payload.PresentVersion = presentVersion
		payload.RequestUpdate = requestUpdate

		environmentVariables := make(map[string]interface{})
		if environmentVariablesString != "" {
			err := json.Unmarshal([]byte(environmentVariablesString), &environmentVariables)
			if err != nil {
				return nil, err
			}

			payload.EnvironmentVariables = environmentVariables
		}

		environmentTemplate := make(map[string]interface{})
		if environmentTemplateString != nil && *environmentTemplateString != "" {
			err := json.Unmarshal([]byte(*environmentTemplateString), &environmentTemplate)
			if err != nil {
				return nil, err
			}

			payload.EnvironmentTemplate = environmentTemplate
		}

		var ports []interface{}
		if portsString != nil && *portsString != "" {
			err := json.Unmarshal([]byte(*portsString), &ports)
			if err != nil {
				return nil, err
			}

			payload.Ports = ports
		}

		dockerCompose := make(map[string]interface{})
		if dockerComposeString != nil && *dockerComposeString != "" {
			err := json.Unmarshal([]byte(*dockerComposeString), &dockerCompose)
			if err != nil {
				return nil, err
			}

			payload.DockerCompose = dockerCompose
		}

		newDockerCompose := make(map[string]interface{})
		if newDockerComposeString != nil && *newDockerComposeString != "" {
			err := json.Unmarshal([]byte(*newDockerComposeString), &newDockerCompose)
			if err != nil {
				return nil, err
			}

			payload.NewDockerCompose = newDockerCompose
		}

		payloads = append(payloads, payload)
	}

	return payloads, nil
}

func (ast *AppStateDatabase) GetRequestedState(aKey uint64, aStage common.Stage) (common.TransitionPayload, error) {
	preppedStatement, err := ast.db.Prepare(QuerySelectRequestedStateByAppKeyAndStage)
	if err != nil {
		return common.TransitionPayload{}, err
	}

	rows, err := preppedStatement.Query(aKey, aStage)
	if err != nil {
		return common.TransitionPayload{}, err
	}

	hasResult := rows.Next() // only get first result
	if !hasResult {
		err := rows.Close()
		if err != nil {
			return common.TransitionPayload{}, err
		}

		return common.TransitionPayload{}, fmt.Errorf("no requested state found for app_key: %d with stage: %s", aKey, aStage)
	}

	var appName string
	var appKey uint64
	var requestorAccountKey uint64
	var deviceOwnerAccountKey uint64
	var requestUpdate bool
	var stage common.Stage
	var version string
	var presentVersion string
	var releaseKey uint64
	var newReleaseKey uint64
	var newestVersion string
	var currentState common.AppState
	var requestedState common.AppState
	var environmentVariablesString string
	var environmentTemplateString *string
	var dockerComposeString *string
	var newDockerComposeString *string
	var portsString *string

	err = rows.Scan(&appName, &appKey, &stage, &version, &presentVersion, &newestVersion, &currentState, &requestedState, &requestorAccountKey, &deviceOwnerAccountKey, &releaseKey, &newReleaseKey, &requestUpdate, &environmentVariablesString, &environmentTemplateString, &portsString, &dockerComposeString, &newDockerComposeString)
	if err != nil {
		return common.TransitionPayload{}, err
	}

	err = rows.Close()
	if err != nil {
		return common.TransitionPayload{}, err
	}

	payload := common.BuildTransitionPayload(appKey, appName, requestorAccountKey, stage, currentState, requestedState, releaseKey, newReleaseKey, ast.config)
	payload.DeviceOwnerAccountKey = deviceOwnerAccountKey
	payload.Version = version
	payload.NewestVersion = newestVersion
	payload.PresentVersion = presentVersion
	payload.RequestUpdate = requestUpdate

	environmentVariables := make(map[string]interface{})
	if environmentVariablesString != "" {
		err := json.Unmarshal([]byte(environmentVariablesString), &environmentVariables)
		if err != nil {
			return common.TransitionPayload{}, err
		}

		payload.EnvironmentVariables = environmentVariables
	}

	environmentTemplate := make(map[string]interface{})
	if environmentTemplateString != nil && *environmentTemplateString != "" {
		err := json.Unmarshal([]byte(*environmentTemplateString), &environmentTemplate)
		if err != nil {
			return common.TransitionPayload{}, err
		}

		payload.EnvironmentTemplate = environmentTemplate
	}

	ports := make([]interface{}, 0)
	if portsString != nil && *portsString != "" {
		err := json.Unmarshal([]byte(*portsString), &ports)
		if err != nil {
			return common.TransitionPayload{}, err
		}

		payload.Ports = ports
	}

	dockerCompose := make(map[string]interface{})
	if dockerComposeString != nil && *dockerComposeString != "" {
		err := json.Unmarshal([]byte(*dockerComposeString), &dockerCompose)
		if err != nil {
			return common.TransitionPayload{}, err
		}

		payload.DockerCompose = dockerCompose
	}

	newDockerCompose := make(map[string]interface{})
	if newDockerComposeString != nil && *newDockerComposeString != "" {
		err := json.Unmarshal([]byte(*newDockerComposeString), &newDockerCompose)
		if err != nil {
			return common.TransitionPayload{}, err
		}

		payload.NewDockerCompose = newDockerCompose
	}

	if err != nil {
		return common.TransitionPayload{}, err
	}

	return payload, nil
}

func (ast *AppStateDatabase) BulkUpsertRequestedStateChanges(payloads []common.TransitionPayload) error {
	tx, err := ast.db.Begin()
	if err != nil {
		return err
	}

	for _, payload := range payloads {
		upsertStatement, err := tx.Prepare(QueryUpsertRequestedStateEntry) // Prepare statement.
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		environmentsJSONBytes, err := json.Marshal(payload.EnvironmentVariables)
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		environmentsJSONString := string(environmentsJSONBytes)

		environmentTemplateJSONBytes, err := json.Marshal(payload.EnvironmentTemplate)
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		environmentTemplateJSONString := string(environmentTemplateJSONBytes)

		portsJSONBytes, err := json.Marshal(payload.Ports)
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		portsJSONString := string(portsJSONBytes)

		dockerComposeJSONBytes, err := json.Marshal(payload.DockerCompose)
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		dockerComposeJSONString := string(dockerComposeJSONBytes)

		newDockerComposeJSONBytes, err := json.Marshal(payload.NewDockerCompose)
		if err != nil {
			_ = tx.Rollback()
			return err
		}

		newDockerComposeJSONString := string(newDockerComposeJSONBytes)

		_, err = upsertStatement.Exec(payload.AppName, payload.AppKey, payload.Stage, payload.Version, payload.PresentVersion, payload.NewestVersion,
			payload.CurrentState, payload.RequestedState, payload.RequestorAccountKey, payload.DeviceOwnerAccountKey, payload.ReleaseKey, payload.NewReleaseKey, payload.RequestUpdate, environmentsJSONString, environmentTemplateJSONString, portsJSONString, dockerComposeJSONString, newDockerComposeJSONString,
			time.Now().Format(time.RFC3339),
		)

		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func (ast *AppStateDatabase) UpsertRequestedStateChange(payload common.TransitionPayload) error {
	upsertStatement, err := ast.db.Prepare(QueryUpsertRequestedStateEntry) // Prepare statement.
	if err != nil {
		return err
	}

	defer func() { _ = upsertStatement.Close() }()

	// if the payload we received from the backend/database does not include a current state
	// // TODO: test if this is still relevant --> I don't think so since we always send full state now
	// if payload.CurrentState == "" {
	// 	app, err := ast.GetAppState(payload.AppKey, payload.Stage)
	// 	if err != nil {
	// 		return err
	// 	}

	// 	payload.CurrentState = app.CurrentState
	// }

	environmentsJSONBytes, err := json.Marshal(payload.EnvironmentVariables)
	if err != nil {
		return err
	}

	environmentsJSONString := string(environmentsJSONBytes)

	environmentTemplateJSONBytes, err := json.Marshal(payload.EnvironmentTemplate)
	if err != nil {
		return err
	}

	environmentTemplateJSONString := string(environmentTemplateJSONBytes)

	portsJSONBytes, err := json.Marshal(payload.Ports)
	if err != nil {
		return err
	}

	portsJSONString := string(portsJSONBytes)

	dockerComposeJSONBytes, err := json.Marshal(payload.DockerCompose)
	if err != nil {
		return err
	}

	dockerComposeJSONString := string(dockerComposeJSONBytes)

	newDockerComposeJSONBytes, err := json.Marshal(payload.NewDockerCompose)
	if err != nil {
		return err
	}

	newDockerComposeJSONString := string(newDockerComposeJSONBytes)

	_, err = upsertStatement.Exec(payload.AppName, payload.AppKey, payload.Stage, payload.Version, payload.PresentVersion, payload.NewestVersion,
		payload.CurrentState, payload.RequestedState, payload.RequestorAccountKey, payload.DeviceOwnerAccountKey, payload.ReleaseKey, payload.NewReleaseKey, payload.RequestUpdate, environmentsJSONString, environmentTemplateJSONString, portsJSONString, dockerComposeJSONString, newDockerComposeJSONString,
		time.Now().Format(time.RFC3339),
	)

	if err != nil {
		return err
	}

	return nil
}

func (ast *AppStateDatabase) ClearAllLogHistory(appName string, appKey uint64, stage common.Stage) error {
	logsBytes, err := json.Marshal([]string{})
	if err != nil {
		return err
	}

	updateStatement, err := ast.db.Prepare(QueryUpdateLogHistoryEntries)
	if err != nil {
		return err
	}

	defer func() { _ = updateStatement.Close() }()

	_, err = updateStatement.Exec(string(logsBytes), appName, appKey, stage)
	if err != nil {
		return err
	}

	return nil
}

func (ast *AppStateDatabase) UpsertLogHistory(appName string, appKey uint64, stage common.Stage, logs []string) error {
	tx, err := ast.db.Begin()
	if err != nil {
		return err
	}

	logsBytes, err := json.Marshal(logs)
	if err != nil {
		return err
	}

	logsString := string(logsBytes)

	upsertStatement, err := tx.Prepare(QueryUpsertLogHistoryEntry)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	_, err = upsertStatement.Exec(appName, appKey, stage, "APP", logsString)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	// since we don't have a logtype anymore, have to update all logtypes...
	updateStatement, err := tx.Prepare(QueryUpdateLogHistoryEntries)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	_, err = updateStatement.Exec(logsString, appName, appKey, stage)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

func (ast *AppStateDatabase) GetAppLogHistory(appName string, appKey uint64, stage common.Stage) ([]string, error) {
	preppedStatement, err := ast.db.Prepare(QuerySelectLogHistoryByAppKeyStageAndType)
	if err != nil {
		return nil, err
	}

	rows, err := preppedStatement.Query(appKey, stage)
	if err != nil {
		return nil, err
	}

	hasResult := rows.Next() // only get first result
	if !hasResult {
		err := rows.Close()
		if err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("no logs found for %d (%s) ", appKey, stage)
	}

	var logsString string

	err = rows.Scan(&logsString)
	if err != nil {
		return nil, err
	}

	err = rows.Close()
	if err != nil {
		return nil, err
	}

	var logsArray []string
	err = json.Unmarshal([]byte(logsString), &logsArray)
	if err != nil {
		return nil, err
	}

	return logsArray, nil
}

func (ast *AppStateDatabase) updateDeviceState(newStatus messenger.DeviceStatus, newInt system.NetworkInterface) error {
	selectStatement, err := ast.db.Prepare(QuerySelectAllDeviceState)
	if err != nil {
		return err
	}

	defer func() { _ = selectStatement.Close() }()

	rows, err := selectStatement.Query()
	if err != nil {
		return err
	}
	hasResult := rows.Next() // only get first result

	if !hasResult {
		err := rows.Close()
		if err != nil {
			return err
		}

		return fmt.Errorf("no device state to update")
	}

	var curInterfaceType string
	var curDeviceStatus string
	err = rows.Scan(&curInterfaceType, &curDeviceStatus)
	if err != nil {
		return err
	}

	if curInterfaceType == string(newInt) {
		err := rows.Close()
		if err != nil {
			return err
		}

		return fmt.Errorf("the current interface is already %s", curInterfaceType)
	}

	if curDeviceStatus == string(newStatus) {
		err := rows.Close()
		if err != nil {
			return err
		}

		return fmt.Errorf("the device status is already %s", curDeviceStatus)
	}

	err = rows.Close()
	if err != nil {
		return err
	}

	// Add new entry in history
	insertStatement, err := ast.db.Prepare(QueryInsertDeviceStateHistoryEntry) // Prepare statement.
	if err != nil {
		return err
	}
	_, err = insertStatement.Exec(curInterfaceType, curDeviceStatus, time.Now().Format(time.RFC3339))
	if err != nil {
		return err
	}

	err = insertStatement.Close()
	if err != nil {
		return err
	}

	// Fallback to the current when value is left out
	var newestStatus string
	var newestInterface string

	if newStatus == "" {
		newestStatus = curDeviceStatus
	} else {
		newestStatus = string(newStatus)
	}

	if newInt == "" {
		newestInterface = curInterfaceType
	} else {
		newestInterface = string(newInt)
	}

	// Update current state
	updateStatement, err := ast.db.Prepare(QueryUpdateDeviceState) // Prepare statement.
	if err != nil {
		return err
	}
	_, err = updateStatement.Exec(newestStatus, newestInterface)
	if err != nil {
		return err
	}

	err = updateStatement.Close()
	if err != nil {
		return err
	}

	return nil
}

func (ast *AppStateDatabase) DeleteAppState(appKey uint64, stage common.Stage) error {
	deleteStatement, err := ast.db.Prepare(QueryDeleteAppStateByAppKeyAndStage)
	if err != nil {
		return err
	}
	defer func() { _ = deleteStatement.Close() }()

	_, err = deleteStatement.Exec(appKey, stage)
	if err != nil {
		return err
	}

	log.Debug().Msgf("Deleted app state for app_key=%d, stage=%s from database", appKey, stage)
	return nil
}

func (ast *AppStateDatabase) DeleteRequestedState(appKey uint64, stage common.Stage) error {
	deleteStatement, err := ast.db.Prepare(QueryDeleteRequestedStateByAppKeyAndStage)
	if err != nil {
		return err
	}
	defer func() { _ = deleteStatement.Close() }()

	_, err = deleteStatement.Exec(appKey, stage)
	if err != nil {
		return err
	}

	log.Debug().Msgf("Deleted requested state for app_key=%d, stage=%s from database", appKey, stage)
	return nil
}

func (ast *AppStateDatabase) execute(fileName string) error {
	file, err := fs.ReadFile(scriptFiles, fileName)

	if err != nil {
		return err
	}

	requests := strings.Split(string(file), ";\n")

	for _, request := range requests {
		_, err := ast.db.Exec(request)
		if err != nil {
			return err
		}
	}

	return nil
}
