package muse

type payloadType string

const (
	payloadRunModelConfigured                       payloadType = "run.model.configured"
	payloadRuntimeCommandIntakeReceived             payloadType = "runtime.command_intake.received"
	payloadRuntimeCommandIntakeSessionNameReceived  payloadType = "runtime.command_intake.session_name.received"
	payloadRuntimeCommandIntakeSettled              payloadType = "runtime.command_intake.settled"
	payloadRuntimeRetainedFact                      payloadType = "runtime.retained_fact"
	payloadRuntimeSession                           payloadType = "runtime.session"
	payloadRuntimeSessionCaptureSelection           payloadType = "runtime.session.capture_selection"
	payloadRuntimeSessionMetadata                   payloadType = "runtime.session.metadata"
	payloadRuntimeSessionPermissionFormatDeclared   payloadType = "runtime.session.permission_format_declared"
	payloadRuntimeSessionPermissionProfileCommitted payloadType = "runtime.session.permission_profile_committed"
	payloadRuntimeSessionRouteFacts                 payloadType = "runtime.session.route_facts"
	payloadRuntimeSessionTask                       payloadType = "runtime.session.task"
	payloadRuntimeSessionStartIntakeV3              payloadType = "runtime.session_start.intake_v3"
	payloadRuntimeUserIntentAccepted                payloadType = "runtime.user_intent.accepted"
	payloadRuntimeUserIntentMaterialized            payloadType = "runtime.user_intent.materialized"
	payloadSessionEnd                               payloadType = "session.end"
	payloadSessionNameChanged                       payloadType = "session.name.changed"
	payloadSessionOpenedObserved                    payloadType = "session.opened.observed"
	payloadSessionResumed                           payloadType = "session.resumed"
	payloadSessionStarted                           payloadType = "session.started"
)

type runtimeRole string

const (
	roleRun                  runtimeRole = "run"
	roleTask                 runtimeRole = "task"
	roleAgentTreeInitialized runtimeRole = "agent_tree_initialized"
)

type runtimeEvent string

const (
	eventAccepted                         runtimeEvent = "accepted"
	eventAssistantMessageCommitted        runtimeEvent = "assistant_message_committed"
	eventAssistantToolCallsCommitted      runtimeEvent = "assistant_tool_calls_committed"
	eventCancelled                        runtimeEvent = "cancelled"
	eventCompleted                        runtimeEvent = "completed"
	eventContextBlockDiagnostic           runtimeEvent = "context_block_diagnostic"
	eventContextBlockUpdated              runtimeEvent = "context_block_updated"
	eventFailed                           runtimeEvent = "failed"
	eventGoalUsageAttribution             runtimeEvent = "goal_usage_attribution"
	eventHookRunStarted                   runtimeEvent = "hook_run_started"
	eventHookRunTerminal                  runtimeEvent = "hook_run_terminal"
	eventModelCompleted                   runtimeEvent = "model_completed"
	eventModelInputTraceRecorded          runtimeEvent = "model_input_trace_recorded"
	eventModelRequestConfigured           runtimeEvent = "model_request_configured"
	eventModelResponseCreated             runtimeEvent = "model_response_created"
	eventProposed                         runtimeEvent = "proposed"
	eventProviderRequestOptionsConfigured runtimeEvent = "provider_request_options_configured"
	eventResourceUsageSampled             runtimeEvent = "resource_usage_sampled"
	eventScheduled                        runtimeEvent = "scheduled"
	eventSideEffectIntent                 runtimeEvent = "side_effect_intent"
	eventStarted                          runtimeEvent = "started"
	eventStatus                           runtimeEvent = "status"
	eventTaskStreamLinked                 runtimeEvent = "task_stream_linked"
	eventTerminal                         runtimeEvent = "terminal"
	eventToolResultBatchCommitted         runtimeEvent = "tool_result_batch_committed"
)

type fieldName string

const (
	fieldPrompt                  fieldName = "prompt"
	fieldKind                    fieldName = "kind"
	fieldEventKind               fieldName = "event_kind"
	fieldSourceRunRecordID       fieldName = "source_run_record_id"
	fieldModelCallIndex          fieldName = "model_call_index"
	fieldText                    fieldName = "text"
	fieldNewName                 fieldName = "new_name"
	fieldPreviousName            fieldName = "previous_name"
	fieldSchemaVersion           fieldName = "schema_version"
	fieldPermissionEpoch         fieldName = "permission_epoch"
	fieldEnvelopeSequence        fieldName = "envelope_sequence"
	fieldSourceRunRecordSequence fieldName = "source_run_record_sequence"
	fieldResume                  fieldName = "resume"
)

type streamKind string

const (
	streamSession streamKind = "session"
)

// approvalEvent is the closed newly qualified private review lane.
type approvalEvent string

const (
	approvalRequested       approvalEvent = "requested"
	approvalReviewStarted   approvalEvent = "automated_review_started"
	approvalReviewCompleted approvalEvent = "automated_review_completed"
	approvalDecisionApplied approvalEvent = "decision_applied"
)

type omittedKind string

const (
	omittedRun      omittedKind = "run"
	omittedTask     omittedKind = "task"
	omittedApproval omittedKind = "approval"
	omittedWait     omittedKind = "approval_wait_effect"
	omittedTool     omittedKind = "tool_batch_effect"
)
