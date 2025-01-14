package flowpilot

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/gofrs/uuid"
	"time"
)

type context interface {
	// Get returns the context value with the given name.
	Get(string) interface{}
	GetFlowName() FlowName
	// IsFlow returns true if the name matches the current flow name.
	IsFlow(name FlowName) bool
}

// flowContext represents the basic context for a flow.
type flowContext interface {
	// Set sets a context value for the given key.
	Set(string, interface{})
	// GetFlowID returns the unique ID of the current defaultFlow.
	GetFlowID() uuid.UUID
	// Payload returns the JSONManager for accessing payload data.
	Payload() payload
	// Stash returns the JSONManager for accessing stash data.
	Stash() stash
	// GetInitialState returns the initial state of the flow.
	GetInitialState() StateName
	// GetCurrentState returns the current state of the flow.
	GetCurrentState() StateName
	IsStateScheduled(StateName) bool
	StateVisited(name StateName) bool
	GetScheduledStates() []StateName
	// GetPreviousState returns the previous state of the flow.
	GetPreviousState() StateName
	// IsPreviousState returns true if the previous state equals the given name.
	IsPreviousState(name StateName) bool
	// GetErrorState returns the designated error state of the flow.
	GetErrorState() StateName
}

// actionInitializationContext represents the basic context for a flow action's initialization.
type actionInitializationContext interface {
	// AddInputs adds input parameters to the inputSchema.
	AddInputs(inputs ...Input)
	StateIsRevertible() bool

	flowContext
	actionSuspender
}

// actionExecutionContext represents the context for an action execution.
type actionExecutionContext interface {
	// Input returns the executionInputSchema for the action.
	Input() executionInputSchema
	// ValidateInputData validates the input data against the inputSchema.
	ValidateInputData() bool
	// CopyInputValuesToStash copies specified inputs to the stash.
	CopyInputValuesToStash(inputNames ...string) error
	SetFlowError(FlowError)
	PreventRevert()
	ExecuteHook(HookAction) error
	actionSuspender
	flowContext
}

// actionExecutionContinuationContext represents the context within an action continuation.
type actionExecutionContinuationContext interface {
	Continue(stateNames ...StateName) error
	// Error continues the flow execution to the specified next state with an error.
	Error(flowErr FlowError) error
	// Revert reverts the flow back to the previous state.
	Revert() error

	actionExecutionContext
}

type actionSuspender interface {
	// SuspendAction suspends the current action's execution.
	SuspendAction()
}

type Context interface {
	context
}

// InitializationContext is a shorthand for actionInitializationContext within the flow initialization method.
type InitializationContext interface {
	context
	actionInitializationContext
}

// ExecutionContext is a shorthand for actionExecutionContinuationContext within flow execution method.
type ExecutionContext interface {
	context
	actionExecutionContinuationContext
}

type HookExecutionContext interface {
	context
	actionExecutionContext

	GetFlowError() FlowError
	AddLink(...Link)
	ScheduleStates(...StateName)
}

type BeforeEachActionExecutionContext interface {
	actionExecutionContinuationContext
}

// createAndInitializeFlow initializes the flow and returns a flow Response.
func createAndInitializeFlow(db FlowDB, flow defaultFlow) (FlowResult, error) {
	// Wrap the provided FlowDB with additional functionality.
	dbw := wrapDB(db)
	// Calculate the expiration time for the flow.
	expiresAt := time.Now().Add(flow.ttl).UTC()

	// Initialize the stash and add initial next states.
	s, err := newStash(flow.initialStateNames...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize a new stash: %w", err)
	}

	s.useCompression(flow.useCompression)

	p := newPayload()

	csrfToken, err := generateRandomString(32)
	if err != nil {
		return nil, fmt.Errorf("failed to generate csrf token: %w", err)
	}

	// Create a new flow model with the provided parameters.
	flowCreation := flowCreationParam{
		data:      s.String(),
		csrfToken: csrfToken,
		expiresAt: expiresAt,
	}
	flowModel, err := dbw.createFlowWithParam(flowCreation)
	if err != nil {
		return nil, fmt.Errorf("failed to create flow: %w", err)
	}

	// Create a defaultFlowContext instance.
	fc := &defaultFlowContext{
		flow:      flow,
		dbw:       dbw,
		flowModel: flowModel,
		stash:     s,
		payload:   p,
	}

	er := executionResult{nextStateName: s.getStateName()}

	aec := defaultActionExecutionContext{
		actionName:         "",
		inputSchema:        nil,
		executionResult:    &er,
		defaultFlowContext: fc,
	}

	err = aec.executeBeforeStateHooks(aec.stash.getStateName())
	if err != nil {
		return nil, fmt.Errorf("failed to execute before hook actions: %w", err)
	}

	return er.generateResponse(fc), nil
}

// executeFlowAction processes the flow and returns a Response.
func executeFlowAction(db FlowDB, flow defaultFlow) (FlowResult, error) {
	actionName := flow.queryParam.getActionName()

	// Retrieve the flow model from the database using the flow ID.
	flowModel, err := db.GetFlow(flow.queryParam.getFlowID())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return newFlowResultFromError(flow.errorStateName, ErrorOperationNotPermitted.Wrap(err), flow.debug), nil
		}
		return nil, fmt.Errorf("failed to get flow: %w", err)
	}

	// Check if the flow has expired.
	if time.Now().After(flowModel.ExpiresAt) {
		return newFlowResultFromError(flow.errorStateName, ErrorFlowExpired, flow.debug), nil
	}

	// Parse stash data from the flow model.
	s, err := newStashFromString(flowModel.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse stash from flow: %w", err)
	}

	s.useCompression(flow.useCompression)

	// Initialize JSONManagers for payload and flash data.
	p := newPayload()

	// Parse raw input data into JSONManager.
	inputJSON, err := newActionInputFromInputData(flow.inputData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse input data: %w", err)
	}
	csrfTokenToValidate := flow.inputData.CSRFToken

	if len(flowModel.CSRFToken) <= 0 || flowModel.CSRFToken != csrfTokenToValidate {
		err = errors.New("csrf token mismatch")
		return newFlowResultFromError(flow.errorStateName, ErrorOperationNotPermitted.Wrap(err), flow.debug), nil
	}

	// Create a defaultFlowContext instance.
	fc := &defaultFlowContext{
		flow:      flow,
		dbw:       wrapDB(db),
		flowModel: flowModel,
		stash:     s,
		payload:   p,
	}

	state, err := flow.getState(s.getStateName())
	if err != nil {
		return nil, err
	}

	// Get the action associated with the actionParam name.
	ad, err := state.getActionDetail(actionName)
	if err != nil {
		return newFlowResultFromError(flow.errorStateName, ErrorOperationNotPermitted.Wrap(err), flow.debug), nil
	}

	// Initialize the inputSchema and action context for action execution.
	inputSchema := newSchemaWithInputData(inputJSON)
	aic := &defaultActionInitializationContext{
		inputSchema:        inputSchema.forInitializationContext(),
		defaultFlowContext: fc,
	}

	// Create a actionExecutionContext instance for action execution.
	aec := &defaultActionExecutionContext{
		actionName:         actionName,
		inputSchema:        inputSchema,
		defaultFlowContext: fc,
	}

	err = aec.executeBeforeEachActionHooks()
	if err != nil {
		return newFlowResultFromError(flow.errorStateName, ErrorOperationNotPermitted, flow.debug), nil
	}

	ad.getAction().Initialize(aic)

	// Check if the action is suspended.
	if aic.isSuspended {
		return newFlowResultFromError(flow.errorStateName, ErrorOperationNotPermitted, flow.debug), nil
	}

	// Execute the action and handle any errors.
	err = ad.getAction().Execute(aec)
	if err != nil {
		return nil, fmt.Errorf("the action failed to handle the request: %w", err)
	}

	// Ensure that the action has set a result object.
	if aec.executionResult == nil {
		er := executionResult{nextStateName: s.getStateName()}
		aec.executionResult = &er
	}

	// Generate a response based on the execution result.
	return aec.executionResult.generateResponse(fc), nil
}
