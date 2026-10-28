package api

import (
	"context"
	"errors"
	"fmt"
	"reagent/common"
	"reagent/errdefs"
	"reagent/messenger"
	"reagent/terminal"
)

func (ex *External) initDeviceTerm(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	// The host terminal is a shell on the device itself, running as the agent:
	// the UI and the product docs put it under MAINTAIN.
	privileged, err := ex.Privilege.Check("MAINTAIN", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to open the device terminal"))
	}

	callerID := fmt.Sprint(response.Details["caller_authid"])

	pseudoTerminal, created, err := terminal.GetOrCreatePseudoTerminal(callerID)
	if err != nil {
		return nil, err
	}

	if created {
		res := pseudoTerminal.Setup(ex.Config, ex.Messenger)

		return &messenger.InvokeResult{Arguments: []interface{}{res}}, nil
	}

	res := common.Dict{
		"sessionID":   pseudoTerminal.SessionID,
		"writeTopic":  pseudoTerminal.WriteTopic,
		"dataTopic":   pseudoTerminal.DataTopic,
		"resizeTopic": pseudoTerminal.ResizeTopic,
	}

	return &messenger.InvokeResult{Arguments: []interface{}{res}}, nil
}

func (ex *External) startTerminalSessHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("DEVELOP", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to start a terminal session"))
	}

	if len(response.Arguments) == 0 {
		return nil, errors.New("failed to parse args, payload is missing")
	}

	payloadArg := response.Arguments[0]
	payload, ok := payloadArg.(map[string]interface{})

	if !ok {
		return nil, errors.New("failed to parse payload")
	}

	sessionIDKw := payload["sessionID"]
	sessionID, ok := sessionIDKw.(string)
	if !ok {
		return nil, errors.New("failed to parse sessionID")
	}

	// The payload's registrationID is ignored. It is the id of the UI's data
	// subscription, which the agent used to compare with the ids of the
	// router's unregister events, ids of another counter: it ended the wrong
	// users' sessions and never the right one. UIs still send it, and ones
	// that send none, or a number of another type, start all the same.

	// Only the session's owner, or 'system' relaying for it, may start it.
	err = ex.TerminalManager.StartTerminalSession(sessionID, fmt.Sprint(response.Details["caller_authid"]))
	if err != nil {
		return nil, err
	}

	return &messenger.InvokeResult{}, nil
}

func (ex *External) stopTerminalSession(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("DEVELOP", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to start a terminal session"))
	}

	if len(response.Arguments) == 0 {
		return nil, errors.New("failed to parse args, payload is missing")
	}

	payloadArg := response.Arguments[0]
	payload, ok := payloadArg.(map[string]interface{})

	if !ok {
		return nil, errors.New("failed to parse payload")
	}

	sessionIDKw := payload["sessionID"]
	sessionID, ok := sessionIDKw.(string)
	if !ok {
		return nil, errors.New("failed to parse sessionID")
	}

	// Only the session's owner, or 'system' relaying for it, may stop it.
	err = ex.TerminalManager.StopTerminalSession(sessionID, fmt.Sprint(response.Details["caller_authid"]))
	if err != nil {
		return nil, err
	}

	return &messenger.InvokeResult{}, nil
}

func (ex *External) requestTerminalSessHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("DEVELOP", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to start a terminal session"))
	}

	if len(response.Arguments) == 0 {
		return nil, errors.New("no args found")
	}

	payloadArg := response.Arguments[0]
	payload, ok := payloadArg.(map[string]interface{})

	if !ok {
		return nil, errors.New("failed to parse payload")
	}

	containerNameKw := payload["containerName"]
	containerName, ok := containerNameKw.(string)
	if !ok {
		return nil, errors.New("failed to parse containerName")
	}

	// A shell only in an app's own container: Docker would exec into any
	// container on the host, the platform's own on an appliance.
	if err := ex.AppManager.AppStore.CheckAppContainer(containerName); err != nil {
		return nil, err
	}

	termSess, err := ex.TerminalManager.RequestTerminalSession(containerName, fmt.Sprint(response.Details["caller_authid"]))
	if err != nil {
		return nil, err

	}

	// this will be used by the frontend to register/call to the terminal session
	responseData := common.Dict{
		"dataTopic":   termSess.DataTopic,
		"writeTopic":  termSess.WriteTopic,
		"resizeTopic": termSess.ResizeTopic,
		"sessionID":   termSess.SessionID,
	}

	return &messenger.InvokeResult{Arguments: []interface{}{responseData}}, nil
}
