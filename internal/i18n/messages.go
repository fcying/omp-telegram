package i18n

type taskMessages struct {
	SubmissionUncertain Message
}

type sessionMessages struct {
	Label         Message
	RestoreFailed Message
}

type queueMessages struct {
	Full      Message
	AlbumFull Message
}

type languageMessages struct {
	Changed            Message
	Current            Message
	Usage              Message
	SaveFailed         Message
	CommandDescription Message
}

type deliveryMessages struct {
	Counts        Message
	OldestPending Message
	Head          Message
	RetryIn       Message
	Ready         Message
	Sending       Message
	Idle          Message
	Uncertain     Message
	Global        Message
}

// Msg is the complete typed catalog. Each entry keeps both languages together.
var Msg = struct {
	Task     taskMessages
	Session  sessionMessages
	Queue    queueMessages
	Language languageMessages
	Delivery deliveryMessages

	Output                                                                    Message
	ResultSummary                                                             Message
	Applying                                                                  Message
	AutoresearchActiveResearchRootManagedUntilOffStopRound                    Message
	AutoresearchDisableNotConfirmedQueuedWorkPaused                           Message
	AutoresearchModeDisabled                                                  Message
	AutoresearchModeEnabledNoActiveResearchRoot                               Message
	CandidateSession                                                          Message
	AppendFormat                                                              Message
	ID                                                                        Message
	NormalInputLockedUseMigrateRecoverOldRestoreSource                        Message
	RuntimeStatus                                                             Message
	Page                                                                      Message
	Role                                                                      Message
	AppendStatus                                                              Message
	ModelSwitchedThinkingUnavailableRuntimeClosed                             Message
	ModelSwitchedThinkingUnavailable                                          Message
	ThinkingChangedQueryUnavailableRuntimeClosed                              Message
	ThinkingChangedQueryUnavailable                                           Message
	UseElsewhere                                                              Message
	Pinned                                                                    Message
	DiskFreeGiB                                                               Message
	DiskFreeMiB                                                               Message
	UncertainCount                                                            Message
	AgeDays                                                                   Message
	AgeDaysHours                                                              Message
	AgeHours                                                                  Message
	AgeMinutes                                                                Message
	NamedSessionIdentity                                                      Message
	Setting                                                                   Message
	NotSupportedBridgeBecauseCanMoveSessionWorkspaceUse                       Message
	PageWorkspace                                                             Message
	EarlierOmitted                                                            Message
	More                                                                      Message
	RecentToolOutcome                                                         Message
	RunningTool                                                               Message
	AutoresearchNotAvailableCurrentNativeExtensionCatalogNoCommand            Message
	SessionDeleteNotSupportedBridgeBecauseDeletesActiveSession                Message
	HostResultDeliveryFailedRuntimeClosed                                     Message
	HostResultDeliveryFailedRuntimeClosedTaskUncertain                        Message
	HostToolResultCouldNotDeliveredOmpTaskOutcome                             Message
	NamedWorkspaceCannotSymbolicLink                                          Message
	PreviousSessionStartStillUncertainUseCloseBeforeStarting                  Message
	RequestedSessionStartInterruptedBeforeOmpIdentitySavedOutcome             Message
	AbortOutcomeUncertainActiveTaskWillNotReplayedAutomatically               Message
	AbortRequestedQueuedPromptsCleared                                        Message
	AbortRequestedCurrentTaskQueuedPromptsWillContinue                        Message
	AlbumContainsTooManyItems                                                 Message
	AlbumCouldNotPrepared                                                     Message
	OMPSessionStillStartingRestoredWorkspaceWaitOperationFinish               Message
	IdleInstanceRequired                                                      Message
	InstanceAlreadyRunningUseNewFreshSessionCloseBefore                       Message
	AnotherBindingDeletionPendingTryAgainLater                                Message
	Args                                                                      Message
	AssistantOutputBudgetExceeded                                             Message
	AssistantOutputBudgetExceededSessionKept                                  Message
	AttachmentCouldNotPrepared                                                Message
	AttachmentDeliveryFailedWillNotReplayedAutomatically                      Message
	AttachmentDeliveryUncertainWillNotReplayedAutomatically                   Message
	AutoresearchAdmissionInterruptedTaskOutcomeUncertainWillNotReplayed       Message
	AutoresearchClearCompleted                                                Message
	AutoresearchClearCouldNotConfirmedRuntimeClosedQueuedWork                 Message
	AutoresearchClearRequiresIdleInstanceEmptyQueueStopResearch               Message
	AutoresearchClearCanceledBecauseInstanceNoLongerIdleCommand               Message
	AutoresearchCommandCompletionCouldNotConfirmedWillNotReplayed             Message
	AutoresearchControlCompletionCouldNotConfirmedTaskOutcomeUncertain        Message
	AutoresearchControlCanceledOutcomeUncertainNoControlWillReplayed          Message
	AutoresearchControlInterruptedWillNotReplayedAutomatically                Message
	AutoresearchControlsMustSentDirectlyClearAlsoRequiresConfirmation         Message
	AutoresearchCouldNotConfirmedDisabledQueuedWorkPaused                     Message
	AutoresearchCouldNotConfirmedDisabledRuntimeClosedQueuedWork              Message
	AutoresearchEndedInterruptedFailedRoundWillNotReplayedAutomatically       Message
	AutoresearchModeCouldNotConfirmedNoWaitingInputSubmitted                  Message
	AutoresearchModeCouldNotConfirmedCurrentOrdinaryTaskLeft                  Message
	AutoresearchModeCouldNotConfirmedOldTaskInterruptedWill                   Message
	AutoresearchModeDisabledOldResearchTaskNotReplayed                        Message
	AutoresearchModeAlreadyDisabledCurrentOrdinaryTaskLeftRunning             Message
	AutoresearchOutputExceededBridgeResourceBudgetOutcomeUncertainWill        Message
	AutoresearchSettlementCouldNotConfirmedTaskOutcomeUncertainWill           Message
	AutoresearchForciblyInterruptedTaskOutcomeUncertainWillNotReplayed        Message
	AutoresearchInterruptedFailedOutcomeUncertainWillNotReplayedAutomatically Message
	Autoresearch                                                              Message
	BindingChanged                                                            Message
	BindingChangedBusyUseBindingsRefresh                                      Message
	BindingChangedUseBindingsRefresh                                          Message
	BindingDeletionUnavailableTryAgainLater                                   Message
	BindingBusyTryAgainLater                                                  Message
	BindingBusyTryAgainWhenTaskQueueIdle                                      Message
	BotAPIUnavailable                                                         Message
	Cancel                                                                    Message
	CancelledWithDetail                                                       Message
	Canceled                                                                  Message
	CancelledQueuedTask                                                       Message
	CannotAccessNamedWorkspace                                                Message
	CannotAccessProjectPath                                                   Message
	CannotCreateMigrationOperationID                                          Message
	CannotCreateBindingListMenu                                               Message
	CannotCreateNativeSelectionMenuDialogCanceled                             Message
	CannotCreateQueueMenu                                                     Message
	CannotCreateSessionSelectionMenu                                          Message
	CannotCreateWorkingDirectoryExistingInstanceStillRunning                  Message
	CannotDetermineAbsoluteUserHomeDirectory                                  Message
	CannotDetermineWorkingDirectoryUseNewNameProjectPath                      Message
	CannotExportInactiveSessionWhenOmpUsesCustomSession                       Message
	CannotObtainOmpSessionIdentityWorkingDirectoryInstanceClosed              Message
	CannotQueryOmpThinkingConfigurationInstanceClosed                         Message
	CannotReadOMPSCycleRolesConfigurationUseModel                             Message
	CannotRegisterTelegramAttachmentDeliveryInstanceClosed                    Message
	CannotResolveProjectPath                                                  Message
	CannotRestoreSourceSessionMigrationNotStarted                             Message
	CheckingMigrationPrerequisites                                            Message
	ChooseCycleRoleModelProviderModel                                         Message
	ChooseThinkingLevelCurrentEffectiveLevelOMPMayAdjust                      Message
	ChooseOMPCycleRoleCurrentModel                                            Message
	ChooseOption                                                              Message
	ToggleNativeFastMode                                                      Message
	ChoosePinOmpSessionWorkingDirectory                                       Message
	ChooseThinkingLevelSession                                                Message
	ClearAutoresearch                                                         Message
	ClearAutoresearchNativeClearCanHardResetTrackedFiles                      Message
	Close                                                                     Message
	CloseForgetIdleBindingActiveTasksPendingWorkCannot                        Message
	CloseOmpKeepWorkspaceSession                                              Message
	Closed                                                                    Message
	CommandsUnknownUnsupportedSourceNotSupportedBridgeNoCommand               Message
	CompactContextWhileIdle                                                   Message
	CompactingContext                                                         Message
	Compacting                                                                Message
	CompactionCompletedContext                                                Message
	CompactionFailedOutcomeUncertainInstanceClosedWillResumeOn                Message
	CompactionCanceledInstanceClosedWillResumeOnNextRequest                   Message
	Config                                                                    Message
	Confirm                                                                   Message
	ConfirmationRequired                                                      Message
	Confirmed                                                                 Message
	Connected                                                                 Message
	CouldNotCloseBindingsMenuTapCloseAgain                                    Message
	CouldNotCloseQueueMenuTapCloseAgain                                       Message
	CurrentOMPArgumentPresetAsteriskMarksCurrentPresetChoose                  Message
	Database                                                                  Message
	Delete                                                                    Message
	DeleteFailed                                                              Message
	DeleteOmpSessionTitleIDDeletesSavedOMPSession                             Message
	DeleteUnavailable                                                         Message
	Deleted                                                                   Message
	Deleting                                                                  Message
	DiagnosticsAlreadyRunning                                                 Message
	DiagnosticsTimedOutRunDoctorAgain                                         Message
	DiagnosticsUnavailableRunDoctorAgain                                      Message
	Disk                                                                      Message
	Export                                                                    Message
	ExportOmpSessionExportHtmlSessionId                                       Message
	ExportOmpSession                                                          Message
	ExtensionCommandsNotSupportedBridgeBecauseTheyCanChange                   Message
	FAIL                                                                      Message
	FailedCheckSessionStateNoSessionDeleted                                   Message
	FailedCloseSavedBinding                                                   Message
	FailedCreateWorkingDirectory                                              Message
	FailedDeleteOmpSessionUseResumeCheckStatusBefore                          Message
	FailedDeleteSavedBinding                                                  Message
	FailedDisplaySavedBindingsUseBindingsTryAgain                             Message
	FailedDisplayNativeSelectionMenuDialogCanceled                            Message
	FailedDisplayQueueUseQueueTryAgain                                        Message
	FailedDisplaySessionSelectionMenuUseTryAgain                              Message
	FailedReadModelSettings                                                   Message
	FailedReadPinnedSessions                                                  Message
	FailedReadSavedBindings                                                   Message
	FailedReadSessionBinding                                                  Message
	FailedReadSessionState                                                    Message
	FailedReadSession                                                         Message
	FailedResumeOmpStartupOutcomeUncertainUseCloseBefore                      Message
	FailedResumeReleasedOMPSessionNoPromptSubmitted                           Message
	FailedSaveOMPArgumentPresetSelection                                      Message
	FailedSaveSessionBindingInstanceClosed                                    Message
	FailedSaveStartupIntent                                                   Message
	FailedSendConfirmationOperationCanceled                                   Message
	FailedStartOmpStartupOutcomeUncertainUseCloseBefore                       Message
	FailedUpdatePinnedSessions                                                Message
	ForgetSavedBindingWorkspaceNativeOMPSessionHistoryWill                    Message
	FormatLine                                                                Message
	HandoffCanceledWithoutResultInstanceClosedWillResumeOn                    Message
	HandoffCompleted                                                          Message
	HandoffFailedOutcomeUncertainInstanceClosedWillResumeOn                   Message
	HandoffRequested                                                          Message
	HandoffResultCouldNotConfirmedInstanceClosedWillResume                    Message
	Inbox                                                                     Message
	InputDialogsNotSupportedCanceledProvideInformationNormalMessage           Message
	InvalidWorkingDirectory                                                   Message
	InvalidWorkingDirectoryExistingInstanceStillRunning                       Message
	ListSavedBindingsBindingsOld                                              Message
	MainChat                                                                  Message
	MigrateContextMigrateRecoverOldSessionIdSessionFile                       Message
	MigrationCompleteWorkspaceSessionOldSessionPreservedCanRestored           Message
	MigrationExplicitlyRecoveredWorkspaceSession                              Message
	MigrationOnlyAllowedWhenTasksQueueSessionControlsAll                      Message
	MigrationUnresolvedInputNeitherQueuedNorSubmittedUseStatus                Message
	MigrationStartedHandoffMayTakeUpSBindingWill                              Message
	MigrationUnresolved                                                       Message
	MigrationUnresolvedOperationPhaseSourceSession                            Message
	MigrationNotConfirmedRuntimeStoppedBindingMigrationLockPreserved          Message
	MigrationNotStartedCheckOmpTelegramMigrateV1Extension                     Message
	ModelSwitched                                                             Message
	NameOmpSessionNameTitle                                                   Message
	NativeCommandCompletionCouldNotConfirmedTaskOutcomeUncertain              Message
	NativeCommandDiscoveryUnavailableWaitingSlashCommandsNotSubmitted         Message
	NativeCommandFailedBeforeProducingConfirmedResultTaskOutcome              Message
	NativeControlUnavailable                                                  Message
	NativeControlUnconfirmed                                                  Message
	NativeDiagnosticNotificationOmitted                                       Message
	NativeOmpSessionListingFailed                                             Message
	NewSessionNewNameProjectPath                                              Message
	Next                                                                      Message
	NoActiveTaskQueuedPromptsCleared                                          Message
	NoCycleRolesConfiguredOMPUseModelProviderModel                            Message
	NoInstanceRunningConversationStartNewNameProjectPath                      Message
	NoInstanceRunningGlobalUncertainRecordsUseResumeRestore                   Message
	NoSavedConversationBindings                                               Message
	NoWorkingDirectorySelectedUseNewNamePathResume                            Message
	OK                                                                        Message
	OMPArgumentPresetChangedUseResumeAgain                                    Message
	OMPBinary                                                                 Message
	OMPDidNotReportThinkingLevelSession                                       Message
	OMPDidNotReportResultingThinkingLevelChangeCould                          Message
	OMPDoesNotSupportNativeCommandDiscoverySlashInput                         Message
	OMPAlreadyStarting                                                        Message
	OMPStarting                                                               Message
	OMPStartingTryAgainAfterReady                                             Message
	OMPNativeCommand                                                          Message
	OnlyBindingAnotherConversationWithoutPendingStartCanDeleted               Message
	Open                                                                      Message
	Outbox                                                                    Message
	PageChanged                                                               Message
	Pending                                                                   Message
	PendingNew                                                                Message
	PendingResume                                                             Message
	Pin                                                                       Message
	PreparingAttachment                                                       Message
	Preset                                                                    Message
	Previous                                                                  Message
	Processing                                                                Message
	PromptExceedsOmpRPCFrameLimitNoTaskSubmitted                              Message
	ProvideOldFullSessionIDAbsolutePathNewJSONL                               Message
	QueueRunningPending                                                       Message
	QueuePromptAfterCurrentTask                                               Message
	QueuedAlbum                                                               Message
	QueuedAttachment                                                          Message
	QueuedTask                                                                Message
	Received                                                                  Message
	RecentTools                                                               Message
	Released                                                                  Message
	ReplyTruncated                                                            Message
	Result                                                                    Message
	RetryingAutomatically                                                     Message
	RunNativeHandoffHandoffInstructions                                       Message
	RunOmpReviewReviewArguments                                               Message
	RunSafeDiagnostics                                                        Message
	Running                                                                   Message
	ToolRunningPhase                                                          Message
	RunningTools                                                              Message
	Runtime                                                                   Message
	SQLiteCheckUnavailable                                                    Message
	SQLiteQuickCheckFailed                                                    Message
	SavedBindingDeletedWorkspaceNativeOMPSessionHistoryPreserved              Message
	SavedBindingsPage                                                         Message
	SavedBindingsOldestFirstPage                                              Message
	SavedOmpSessions                                                          Message
	SelectOMPArgumentPresetArgsNumber                                         Message
	ArgumentPresetSelected                                                    Message
	NamedArgumentPresetSelected                                               Message
	Selected                                                                  Message
	SelectedID                                                                Message
	SelectionNotChanged                                                       Message
	SendAutoresearchDirectlyUsingCurrentNativeExtensionCommandNot             Message
	SessionHTMLExportFailed                                                   Message
	SessionHTMLExportTimedOut                                                 Message
	SessionDeletedButFailedClearSavedBindingsPins                             Message
	SessionDeleted                                                            Message
	SessionExportFailed                                                       Message
	SessionExportTooLargeSendThroughTelegram                                  Message
	SessionMigrationUnresolved                                                Message
	SessionMigrationUnresolvedCannotCreateSwitchBindings                      Message
	SessionMigrationUnresolvedExportUnavailableUseMigrateRecoverFirst         Message
	SessionMigrationUnresolvedSessionSelectionExportUnavailableUseMigrate     Message
	MigrationUnresolvedCannotDeleteBinding                                    Message
	MigrationUnresolvedCannotDeleteOtherBinding                               Message
	SessionMigrationUnresolvedUseMigrateRecoverRecoverExplicitly              Message
	SessionNamed                                                              Message
	SettingsChangeInterruptedResultCouldNotConfirmed                          Message
	ShowCancelPendingBridgeTasks                                              Message
	ShowSessionModelContextSpeedQueue                                         Message
	CommandHelp                                                               Message
	StartNewOmpSessionExistingFilesSessionHistoryWill                         Message
	Starting                                                                  Message
	StateChangedDuringDiagnosticsRunDoctorAgain                               Message
	StatusValue                                                               Message
	SteeringFailedCurrentTaskMayNotIncludeInstruction                         Message
	SteeringOutcomeUncertainWillNotReplayedAutomatically                      Message
	SteeringStateCouldNotConfirmedTaskOutcomeUncertainWill                    Message
	SteeringInterruptedTaskOutcomeUncertainWillNotReplayedAutomatically       Message
	Stop                                                                      Message
	StopTaskClearQueue                                                        Message
	Stopping                                                                  Message
	Storage                                                                   Message
	SubmissionFailedOutcomeUncertainTaskWillNotReplayedAutomatically          Message
	TaskNoLongerQueued                                                        Message
	TaskCancelledBeforeCompletion                                             Message
	Telegram                                                                  Message
	AbortRequestFailed                                                        Message
	InstanceLimitReached                                                      Message
	AutoresearchControlStillLoadingNoCommandSubmitted                         Message
	ConfirmationCouldNotDeliveredOmpStateUncertainWillNot                     Message
	ConversationBecameBusyUseResumeAgainWhenIdle                              Message
	ConversationBusyUseAgainAfterTaskQueueFinish                              Message
	InstanceClosedSessionPreserved                                            Message
	MigrationOperationStillRunningUseStopStopRuntimeBefore                    Message
	ModelSelectionUnavailableOpenModelChooseAgain                             Message
	ModelSwitchCouldNotConfirmedUseStatusCheckActual                          Message
	NumberOptionsUnsupportedDialogCanceled                                    Message
	OmpRequestFailedTaskOutcomeUncertainWillNotReplayed                       Message
	OmpSessionOperationStillLoading                                           Message
	OmpSessionOperationStillLoadingWaitFinish                                 Message
	PreviousRecoveryProcessStillExitingWaitReapedBeforeExplicitly             Message
	ProjectPathMustAccessibleDirectory                                        Message
	QueuedNativeCommandNoLongerAvailableCanceledNotSent                       Message
	ResumedSessionDidNotMatchRequestedOmpSessionID                            Message
	SavedBindingChangedUseAgain                                               Message
	SavedBindingChangedUseBindingsRefresh                                     Message
	SavedBindingChangedUseExportAgain                                         Message
	SavedBindingChangedUseResumeAgain                                         Message
	SavedOmpSessionFileWorkingDirectoryUnavailableSoStartup                   Message
	SavedSessionDeletedUsedElsewhereNoOMPProcessStarted                       Message
	SelectedSessionNotWorkingDirectory                                        Message
	SessionListChangedUseResumeAgain                                          Message
	SessionNameChangeCouldNotConfirmedUseStatusCheck                          Message
	SessionOperationStillLoading                                              Message
	SessionStartedButTelegramTopicTitleCouldNotUpdated                        Message
	ThinkingLevelChangeCouldNotConfirmedOpenThinkingCheck                     Message
	ThinkingSelectionUnavailableOpenThinkingChooseAgain                       Message
	WorkingDirectoryUnavailableNoSessionsLoaded                               Message
	WorkspaceRootMustAbsolutePath                                             Message
	ThereNoUnresolvedMigration                                                Message
	ThinkingLevel                                                             Message
	ThinkingLevelOMPAdjustedRequestedLevel                                    Message
	ActionExpired                                                             Message
	BotSupportsPrivateChatsTelegramTopicsGroupsUseInside                      Message
	OmpSessionAlreadyActiveConversation                                       Message
	OmpSessionUnavailableAlreadyUseAnotherConversation                        Message
	SavedSessionUseAnotherConversationRunStatusThereClose                     Message
	SessionActiveAnotherConversationCloseInstanceFirst                        Message
	SessionActiveIdentityStillRestoredWaitWorkFinishBefore                    Message
	SessionAlreadyRunningAnotherConversationCloseInstanceFirstUse             Message
	SessionDeletedTryAgainAfterOperationFinishes                              Message
	SessionCurrentlyActiveAnotherConversationCloseInstanceBeforeExporting     Message
	SessionCurrentlyActiveProcessedAnotherConversationOperationNoDeletion     Message
	SessionCurrentlyActiveProcessedAnotherConversationNoDeletionStarted       Message
	SessionCurrentlyActiveProcessedConversationNoDeletionStartedStop          Message
	SessionCurrentlyExportedTryAgainAfterExportFinishes                       Message
	TooManyCycleRolesTelegramMenuUseModelProvider                             Message
	Tools                                                                     Message
	Topic                                                                     Message
	Unpin                                                                     Message
	Untitled                                                                  Message
	Update                                                                    Message
	UsageArgsPresetNumberInvalidPresetNumber                                  Message
	UsageBindingsOld                                                          Message
	UsageCompact                                                              Message
	UsageDoctor                                                               Message
	UsageExportHtmlSessionId                                                  Message
	UsageFollowupMessage                                                      Message
	UsageMigrateMigrateRecoverOldMigrateRecoverSessionId                      Message
	UsageModelModelProviderModel                                              Message
	UsageNameSessionTitle                                                     Message
	UsageQueue                                                                Message
	UsageResumeResumeOmpSessionID                                             Message
	UsageThinking                                                             Message
	UseMigrateRecoverOldRestoreSourceSession                                  Message
	UseSingleWorkspaceNameAbsoluteProjectPathPath                             Message
	UseWorkspaceNameAbsoluteProjectPathPath                                   Message
	WARN                                                                      Message
	WaitCurrentSessionExportFinishBeforeChangingName                          Message
	WaitCurrentSessionExportFinish                                            Message
	WaitCurrentTaskQueueFinishBeforeChangingModelSettings                     Message
	WaitCurrentTaskQueueFinishBeforeDeletingSession                           Message
	WaitCurrentTaskQueueFinishBeforeExporting                                 Message
	WaitCurrentTaskQueueFinishBeforeHandoff                                   Message
	WaitCurrentTaskQueueFinishBeforeSelectingSession                          Message
	WaitCurrentTaskFinishBeforeChangingModelSettings                          Message
	Workspace                                                                 Message
	WorkspacePathsCannotContainControlCharacters                              Message
	DetailedStatus                                                            Message
	AlbumNoAttachments                                                        Message
	AlbumMemberNotPhotoDocument                                               Message
	AttachmentMissingFileIDExceedsMBDownloadLimit                             Message
	AutoresearchSessionActiveAnotherConversation                              Message
	AutoresearchSessionDeletedUsedElsewhere                                   Message
	Avg                                                                       Message
	CannotCreateAttachmentDirectory                                           Message
	CannotMarkAttachmentDirectory                                             Message
	CannotOpenAttachmentDirectory                                             Message
	CannotOpenIncomingAttachmentDirectory                                     Message
	CannotPersistAttachmentPreparationMarker                                  Message
	CannotReadDownloadedAttachment                                            Message
	CannotSaveIncomingAttachmentSessionOwner                                  Message
	Completed                                                                 Message
	RuntimeConnected                                                          Message
	CountUnavailable                                                          Message
	DataDirectoryNotWritable                                                  Message
	DirectoryUnavailable                                                      Message
	ExecutableCheckFailed                                                     Message
	Failed                                                                    Message
	Fast                                                                      Message
	FreeSpaceUnavailable                                                      Message
	High                                                                      Message
	InconsistentInternalRuntimeState                                          Message
	InvalidRuntimeConfiguration                                               Message
	JustNow                                                                   Message
	Low                                                                       Message
	Max                                                                       Message
	Medium                                                                    Message
	Minimal                                                                   Message
	Unavailable                                                               Message
	New                                                                       Message
	No                                                                        Message
	NoneSelected                                                              Message
	NotSelected                                                               Message
	Off                                                                       Message
	OmpDidNotRestoreSavedSessionFileInstanceClosed                            Message
	OmpDidNotRestoreSavedSessionIdentityInstanceClosed                        Message
	OmpEndedWithoutConfirmedTextResultTaskOutcomeUncertain                    Message
	OmpExitedDuringAutoresearchTaskOutcomeUncertainWillNot                    Message
	OmpExitedWhileSteeringTaskOutcomeUncertainWillNot                         Message
	OmpExitedTaskOutcomeUncertainWillNotReplayedAutomatically                 Message
	OmpNoSavedSessionsWorkingDirectory                                        Message
	OmpIdleButNoConfirmedTerminalResultReceivedTask                           Message
	OmpIdleButSteeringCompletionCouldNotConfirmedTask                         Message
	OmpReadyWorkspaceSession                                                  Message
	OmpRejectedCompaction                                                     Message
	OmpRejectedHandoff                                                        Message
	OmpReportedTaskFailedBeforeProducingConfirmedResultReason                 Message
	OmpReportedTaskFailedBeforeProducingConfirmedResultTask                   Message
	OmpReportedTaskRateLimitedModelProviderTaskOutcome                        Message
	OmpReturnedUnsupportedSessionID                                           Message
	OmpSelectedDifferentWorkingDirectoryInstanceClosed                        Message
	OmpSNativeSteeringQueueRemainedIdleTooLong                                Message
	OmpTelegramDiagnostics                                                    Message
	On                                                                        Message
	RuntimeReleased                                                           Message
	ReleasedSessionFileUnavailable                                            Message
	Resume                                                                    Message
	SavedAutoresearchOwnerUnavailableRestoreSessionFileWorkingDirectory       Message
	SavedAutoresearchSessionUnavailableRestoreSessionFileWorkingDirectory     Message
	SavedSessionFileUnavailable                                               Message
	SessionHistoryNotPersistedYet                                             Message
	ResearchInstanceLimitReached                                              Message
	Unknown                                                                   Message
	Xhigh                                                                     Message
	Yes                                                                       Message
	GatewayRestartedWhilePreviousTaskActiveInterruptedNotResubmitted          Message
}{
	Task: taskMessages{
		SubmissionUncertain: message("Root submission could not be confirmed while steering. The task outcome is uncertain and will not be replayed automatically.", "引导期间无法确认初始提交. 任务结果不确定, 不会自动重放."),
	},
	Session: sessionMessages{
		Label:         message("Session", "会话"),
		RestoreFailed: message("Cannot restore the saved omp session. Its session file or working directory is unavailable. No replacement session was created; use /resume to select a session.", "无法恢复已保存的 omp 会话. 会话文件或工作目录不可用. 未创建替代会话; 使用 /resume 选择会话."),
	},
	Queue: queueMessages{
		Full:      message("The queue is full. This message was not submitted.", "队列已满. 此消息未提交."),
		AlbumFull: message("The queue is full. This album was not submitted.", "队列已满. 此相册未提交."),
	},
	Language: languageMessages{
		Changed:            message("Global language set to English.", "已将全局语言设为中文."),
		Current:            message("Global language: %s. Use /lang en for English or /lang zh for Chinese.", "全局语言: %s. 使用 /lang en 切换英文, /lang zh 切换中文."),
		Usage:              message("Usage: /lang [en|zh]", "用法: /lang [en|zh]"),
		SaveFailed:         message("Failed to save the language preference. The language was not changed.", "无法保存语言设置. 语言未更改."),
		CommandDescription: message("Set global language: /lang [en|zh]", "设置全局语言: /lang [en|zh]"),
	},
	Delivery: deliveryMessages{
		Counts:        message("pending %d, sending %d", "等待 %d, 发送中 %d"),
		OldestPending: message("Oldest pending: %s", "最早等待: %s"),
		Head:          message("Head: %s", "队首: %s"),
		RetryIn:       message("Waiting for retry (%s)", "等待重试 (%s)"),
		Ready:         message("Ready to send", "等待发送"),
		Sending:       message("Sending for %s", "发送中, 已持续 %s"),
		Idle:          message("Idle", "空闲"),
		Uncertain:     message("Uncertain: %d", "结果不确定: %d"),
		Global:        message("Global outbox: pending %d, sending %d, uncertain %d", "全局发件箱: 等待 %d, 发送中 %d, 结果不确定 %d"),
	},

	Output:        message("\n\nOutput:\n", "\n\n输出:\n"),
	ResultSummary: message("\n\nResult: %s", "\n\n结果: %s"),
	Applying:      message("\nApplying...", "\n正在应用..."),
	AutoresearchActiveResearchRootManagedUntilOffStopRound: message("\nAutoresearch: active research root (managed until off or Stop; a round ending is not research completion).", "\n自动研究: 研究主任务正在运行 (由桥接器管理, 直到 off 或停止; 一轮结束不代表研究完成)."),
	AutoresearchDisableNotConfirmedQueuedWorkPaused:        message("\nAutoresearch: disable not confirmed; queued work paused.", "\n自动研究: 尚未确认已禁用; 排队任务已暂停."),
	AutoresearchModeDisabled:                               message("\nAutoresearch: mode disabled.", "\n自动研究: 模式已禁用."),
	AutoresearchModeEnabledNoActiveResearchRoot:            message("\nAutoresearch: mode enabled; no active research root.", "\n自动研究: 模式已启用; 没有正在运行的研究主任务."),
	CandidateSession: message("\nCandidate session: %s", "\n候选会话: %s"),
	AppendFormat:     message("\nFormat: %s", "\n格式: %s"),
	ID:               message("\nID: %s\n", "\nID: %s\n"),
	NormalInputLockedUseMigrateRecoverOldRestoreSource: message("\nNormal input is locked. Use /migrate recover old to restore the source session, or /migrate recover <session-id or absolute file path> to verify a candidate. No session will be recreated automatically.", "\n普通输入已锁定. 使用 /migrate recover old 恢复源会话, 或 /migrate recover <session-id 或绝对文件路径> 核验候选. 不会自动重新创建会话."),
	RuntimeStatus: message("\nOMP: %s\nIdle: %s", "\nOMP: %s\n空闲: %s"),
	Page:          message("\nPage %d/%d", "\n第 %d/%d 页"),
	Role:          message("\nRole: %s", "\n角色: %s"),
	AppendStatus:  message("\nStatus: %s", "\n状态: %s"),
	ModelSwitchedThinkingUnavailableRuntimeClosed: message("\nThinking: n/a\nThe model switch succeeded, but the follow-up thinking query failed and the runtime was closed. The session was kept open.", "\n思考: 不可用\n模型切换成功, 但后续思考设置查询失败, 运行时已关闭. 会话保持打开."),
	ModelSwitchedThinkingUnavailable:              message("\nThinking: n/a\nThe model switch succeeded, but the follow-up thinking query failed.", "\n思考: 不可用\n模型切换成功, 但后续思考设置查询失败."),
	ThinkingChangedQueryUnavailableRuntimeClosed:  message("\nThinking: n/a\nThe thinking level change succeeded, but the follow-up thinking query failed and the runtime was closed. The session was kept open.", "\n思考: 不可用\n思考级别修改成功, 但后续思考设置查询失败, 运行时已关闭. 会话保持打开."),
	ThinkingChangedQueryUnavailable:               message("\nThinking: n/a\nThe thinking level change succeeded, but the follow-up thinking query failed.", "\n思考: 不可用\n思考级别修改成功, 但后续思考设置查询失败."),
	UseElsewhere:                                  message(" [in use elsewhere]", " [其他对话正在使用]"),
	Pinned:                                        message(" [pinned]", " [已置顶]"),
	DiskFreeGiB:                                   message("%.1f GiB free", "可用 %.1f GiB"),
	DiskFreeMiB:                                   message("%d MiB free", "可用 %d MiB"),
	UncertainCount:                                message("%d uncertain", "%d 条结果不确定"),
	AgeDays:                                       message("%dd", "%d 天"),
	AgeDaysHours:                                  message("%dd %dh", "%d 天 %d 小时"),
	AgeHours:                                      message("%dh", "%d 小时"),
	AgeMinutes:                                    message("%dm", "%d 分钟"),
	NamedSessionIdentity:                          message("%s\nSession ID: %s", "%s\n会话 ID: %s"),
	Setting:                                       message("%s (setting: %s)", "%s (设置: %s)"),
	NotSupportedBridgeBecauseCanMoveSessionWorkspaceUse: message("%s is not supported by the bridge because it can move the session or workspace. Use /new or /resume instead.", "桥接器不支持 %s, 因为它可能移动会话或工作区. 请改用 /new 或 /resume."),
	PageWorkspace:     message("%s | Page %d/%d\nWorkspace: %s\n\n", "%s | 第 %d/%d 页\n工作目录: %s\n\n"),
	EarlierOmitted:    message("- %d earlier omitted", "- 已省略之前的 %d 个"),
	More:              message("- %d more", "- 另有 %d 个"),
	RecentToolOutcome: message("- %s: %s (%s)", "- %s: %s (%s)"),
	RunningTool:       message("- %s: running", "- %s: 运行中"),
	AutoresearchNotAvailableCurrentNativeExtensionCatalogNoCommand: message("/autoresearch is not available from the current native extension catalog. No command was submitted.", "当前原生扩展目录中没有可用的 /autoresearch. 未提交命令."),
	SessionDeleteNotSupportedBridgeBecauseDeletesActiveSession:     message("/session delete is not supported by the bridge because it deletes the active session. Use /close, then the /resume picker Delete control instead.", "桥接器不支持 /session delete, 因为它会删除当前会话. 请先使用 /close, 再使用 /resume 选择器中的删除按钮."),
	HostResultDeliveryFailedRuntimeClosed:                          message("A host tool result could not be delivered to omp. The instance was closed.", "无法将主机工具结果传递给 omp. 实例已关闭."),
	HostResultDeliveryFailedRuntimeClosedTaskUncertain:             message("A host tool result could not be delivered to omp. The instance was closed; the task outcome is uncertain and will not be replayed automatically.", "无法将主机工具结果传递给 omp. 实例已关闭; 任务结果不确定, 不会自动重放."),
	HostToolResultCouldNotDeliveredOmpTaskOutcome:                  message("A host tool result could not be delivered to omp. The task outcome is uncertain and will not be replayed automatically.", "无法将主机工具结果传递给 omp. 任务结果不确定, 不会自动重放."),
	NamedWorkspaceCannotSymbolicLink:                               message("A named workspace cannot be a symbolic link.", "命名工作区不能是符号链接."),
	PreviousSessionStartStillUncertainUseCloseBeforeStarting:       message("A previous session start is still uncertain. Use /close before starting another session.", "上次会话启动的结果仍不确定. 请先使用 /close 再启动其他会话."),
	RequestedSessionStartInterruptedBeforeOmpIdentitySavedOutcome:  message("A requested %s session start was interrupted before omp identity was saved. The outcome is uncertain. Use /close to cancel it, then use /new or /resume explicitly.", "请求的 %s 会话启动在保存 omp 标识前中断. 结果不确定. 使用 /close 取消, 然后显式使用 /new 或 /resume."),
	AbortOutcomeUncertainActiveTaskWillNotReplayedAutomatically:    message("Abort outcome is uncertain. The active task will not be replayed automatically.", "中止结果不确定. 活动任务不会自动重放."),
	AbortRequestedQueuedPromptsCleared:                             message("Abort requested and queued prompts cleared.", "已请求中止并清空排队提示词."),
	AbortRequestedCurrentTaskQueuedPromptsWillContinue:             message("Abort requested for the current task. Queued prompts will continue.", "已请求中止当前任务. 排队提示词将继续执行."),
	AlbumContainsTooManyItems:                                      message("Album contains too many items.", "相册包含的项目过多."),
	AlbumCouldNotPrepared:                                          message("Album could not be prepared: %s", "无法准备相册: %s"),
	OMPSessionStillStartingRestoredWorkspaceWaitOperationFinish:    message("An OMP session is still starting or being restored in this workspace. Wait for that operation to finish before deleting.", "此工作目录中仍有 OMP 会话正在启动或恢复. 请等待操作完成后再删除."),
	IdleInstanceRequired:                                           message("An idle instance is required.", "需要空闲实例."),
	InstanceAlreadyRunningUseNewFreshSessionCloseBefore:            message("An instance is already running. Use /new for a fresh session, or /close before resuming another session.", "已有实例正在运行. 使用 /new 新建会话, 或先 /close 再恢复其他会话."),
	AnotherBindingDeletionPendingTryAgainLater:                     message("Another binding deletion is pending. Try again later.", "另一个绑定删除操作正在等待处理. 请稍后重试."),
	Args:                                     message("Args", "参数"),
	AssistantOutputBudgetExceeded:            message("Assistant output exceeded the bridge resource budget before task completion. The task outcome is uncertain and will not be replayed automatically.", "助手输出在任务完成前超过 Bridge 资源预算. 任务结果不确定, 不会自动重放."),
	AssistantOutputBudgetExceededSessionKept: message("Assistant output exceeded the bridge resource budget before task completion. The task outcome is uncertain and will not be replayed automatically. The session was kept open.", "助手输出在任务完成前超过 Bridge 资源预算. 任务结果不确定, 不会自动重放. 会话保持打开."),
	AttachmentCouldNotPrepared:               message("Attachment could not be prepared: %s", "无法准备附件: %s"),
	AttachmentDeliveryFailedWillNotReplayedAutomatically:                      message("Attachment delivery failed. It will not be replayed automatically.", "附件发送失败. 不会自动重放."),
	AttachmentDeliveryUncertainWillNotReplayedAutomatically:                   message("Attachment delivery uncertain. It will not be replayed automatically.", "附件发送结果不确定. 不会自动重放."),
	AutoresearchAdmissionInterruptedTaskOutcomeUncertainWillNotReplayed:       message("Autoresearch admission was interrupted. Its task outcome is uncertain and will not be replayed automatically.", "自动研究准入已中断. 任务结果不确定, 不会自动重放."),
	AutoresearchClearCompleted:                                                message("Autoresearch clear completed.", "自动研究清理已完成."),
	AutoresearchClearCouldNotConfirmedRuntimeClosedQueuedWork:                 message("Autoresearch clear could not be confirmed. The runtime is closed and queued work is paused; no clear will be replayed automatically.", "无法确认自动研究清理结果. 运行时已关闭, 排队任务已暂停; 不会自动重放清理操作."),
	AutoresearchClearRequiresIdleInstanceEmptyQueueStopResearch:               message("Autoresearch clear requires an idle instance and an empty queue. Stop research first.", "清理自动研究需要实例空闲且队列为空. 请先停止研究."),
	AutoresearchClearCanceledBecauseInstanceNoLongerIdleCommand:               message("Autoresearch clear was canceled because the instance is no longer idle or its command catalog changed.", "自动研究清理已取消, 因为实例不再空闲或命令目录已更改."),
	AutoresearchCommandCompletionCouldNotConfirmedWillNotReplayed:             message("Autoresearch command completion could not be confirmed. It will not be replayed automatically.", "无法确认自动研究命令是否完成. 不会自动重放该命令."),
	AutoresearchControlCompletionCouldNotConfirmedTaskOutcomeUncertain:        message("Autoresearch control completion could not be confirmed. Its task outcome is uncertain and will not be replayed automatically.", "无法确认自动研究控制操作是否完成. 任务结果不确定, 不会自动重放."),
	AutoresearchControlCanceledOutcomeUncertainNoControlWillReplayed:          message("Autoresearch control was canceled. Its outcome is uncertain; no control will be replayed automatically.", "自动研究控制操作已取消. 结果不确定; 不会自动重放控制操作."),
	AutoresearchControlInterruptedWillNotReplayedAutomatically:                message("Autoresearch control was interrupted. It will not be replayed automatically.", "自动研究控制操作已中断. 不会自动重放."),
	AutoresearchControlsMustSentDirectlyClearAlsoRequiresConfirmation:         message("Autoresearch controls must be sent directly; clear also requires confirmation. No command was submitted.", "自动研究控制命令必须直接发送; 清理还需要确认. 未提交命令."),
	AutoresearchCouldNotConfirmedDisabledQueuedWorkPaused:                     message("Autoresearch could not be confirmed disabled. Queued work is paused: %s", "无法确认自动研究已禁用. 排队任务已暂停: %s"),
	AutoresearchCouldNotConfirmedDisabledRuntimeClosedQueuedWork:              message("Autoresearch could not be confirmed disabled. The runtime is closed and queued work is paused. Use /autoresearch off to retry or /close; nothing will be replayed automatically.", "无法确认自动研究已禁用. 运行时已关闭, 排队任务已暂停. 使用 /autoresearch off 重试或使用 /close; 不会自动重放任何操作."),
	AutoresearchEndedInterruptedFailedRoundWillNotReplayedAutomatically:       message("Autoresearch ended with an interrupted or failed round. It will not be replayed automatically.", "自动研究因某轮中断或失败而结束. 不会自动重放."),
	AutoresearchModeCouldNotConfirmedNoWaitingInputSubmitted:                  message("Autoresearch mode could not be confirmed. No waiting input was submitted. Use /autoresearch off to safely disable it, or /close.", "无法确认自动研究模式. 未提交等待中的输入. 使用 /autoresearch off 安全禁用, 或使用 /close."),
	AutoresearchModeCouldNotConfirmedCurrentOrdinaryTaskLeft:                  message("Autoresearch mode could not be confirmed. The current ordinary task was left running; queued work is paused. Use /autoresearch off to retry or /close.", "无法确认自动研究模式. 当前普通任务继续运行; 排队任务已暂停. 使用 /autoresearch off 重试或使用 /close."),
	AutoresearchModeCouldNotConfirmedOldTaskInterruptedWill:                   message("Autoresearch mode could not be confirmed. The old task was interrupted and will not be replayed automatically.", "无法确认自动研究模式. 旧任务已中断, 不会自动重放."),
	AutoresearchModeDisabledOldResearchTaskNotReplayed:                        message("Autoresearch mode disabled. The old research task was not replayed.", "自动研究模式已禁用. 未重放旧研究任务."),
	AutoresearchModeAlreadyDisabledCurrentOrdinaryTaskLeftRunning:             message("Autoresearch mode is already disabled. The current ordinary task was left running.", "自动研究模式已处于禁用状态. 当前普通任务继续运行."),
	AutoresearchOutputExceededBridgeResourceBudgetOutcomeUncertainWill:        message("Autoresearch output exceeded the bridge resource budget. Its outcome is uncertain and it will not be replayed automatically.", "自动研究输出超过 Bridge 资源预算. 结果不确定, 不会自动重放."),
	AutoresearchSettlementCouldNotConfirmedTaskOutcomeUncertainWill:           message("Autoresearch settlement could not be confirmed. The task outcome is uncertain and will not be replayed automatically.", "无法确认自动研究是否已结束. 任务结果不确定, 不会自动重放."),
	AutoresearchForciblyInterruptedTaskOutcomeUncertainWillNotReplayed:        message("Autoresearch was forcibly interrupted. Its task outcome is uncertain and will not be replayed automatically.", "自动研究已被强制中断. 任务结果不确定, 不会自动重放."),
	AutoresearchInterruptedFailedOutcomeUncertainWillNotReplayedAutomatically: message("Autoresearch was interrupted or failed. Its outcome is uncertain and it will not be replayed automatically.", "自动研究已中断或失败. 结果不确定, 不会自动重放."),
	Autoresearch:                                                 message("Autoresearch: %s", "自动研究: %s"),
	BindingChanged:                                               message("Binding changed", "绑定已更改"),
	BindingChangedBusyUseBindingsRefresh:                         message("Binding changed or busy. Use /bindings to refresh.", "绑定已更改或忙碌中. 请使用 /bindings 刷新."),
	BindingChangedUseBindingsRefresh:                             message("Binding changed. Use /bindings to refresh.", "绑定已更改. 使用 /bindings 刷新."),
	BindingDeletionUnavailableTryAgainLater:                      message("Binding deletion is unavailable. Try again later.", "暂时无法删除绑定. 请稍后重试."),
	BindingBusyTryAgainLater:                                     message("Binding is busy. Try again later.", "绑定忙碌中. 请稍后重试."),
	BindingBusyTryAgainWhenTaskQueueIdle:                         message("Binding is busy. Try again when its task and queue are idle.", "绑定忙碌中. 请在任务和队列空闲时重试."),
	BotAPIUnavailable:                                            message("Bot API unavailable", "Bot API 不可用"),
	Cancel:                                                       message("Cancel", "取消"),
	CancelledWithDetail:                                          message("Cancel\n%s", "取消\n%s"),
	Canceled:                                                     message("Canceled", "已取消"),
	CancelledQueuedTask:                                          message("Cancelled queued task.", "已取消排队任务."),
	CannotAccessNamedWorkspace:                                   message("Cannot access the named workspace.", "无法访问命名工作区."),
	CannotAccessProjectPath:                                      message("Cannot access the project path.", "无法访问项目路径."),
	CannotCreateMigrationOperationID:                             message("Cannot create a migration operation ID.", "无法创建迁移操作 ID."),
	CannotCreateBindingListMenu:                                  message("Cannot create the binding list menu.", "无法创建绑定列表菜单."),
	CannotCreateNativeSelectionMenuDialogCanceled:                message("Cannot create the native selection menu. The dialog was canceled.", "无法创建原生选择菜单. 对话框已取消."),
	CannotCreateQueueMenu:                                        message("Cannot create the queue menu.", "无法创建队列菜单."),
	CannotCreateSessionSelectionMenu:                             message("Cannot create the session selection menu.", "无法创建会话选择菜单."),
	CannotCreateWorkingDirectoryExistingInstanceStillRunning:     message("Cannot create the working directory. The existing instance is still running.", "无法创建工作目录. 现有实例仍在运行."),
	CannotDetermineAbsoluteUserHomeDirectory:                     message("Cannot determine an absolute user home directory.", "无法确定用户主目录的绝对路径."),
	CannotDetermineWorkingDirectoryUseNewNameProjectPath:         message("Cannot determine the working directory. Use /new <name or project path>.", "无法确定工作目录. 使用 /new <名称或项目路径>."),
	CannotExportInactiveSessionWhenOmpUsesCustomSession:          message("Cannot export an inactive session when omp uses a custom session directory.", "omp 使用自定义会话目录时, 无法导出未运行的会话."),
	CannotObtainOmpSessionIdentityWorkingDirectoryInstanceClosed: message("Cannot obtain omp session identity and working directory. The instance has been closed.", "无法获取 omp 会话标识和工作目录. 实例已关闭."),
	CannotQueryOmpThinkingConfigurationInstanceClosed:            message("Cannot query omp thinking configuration. The instance has been closed.", "无法查询 omp 思考配置. 实例已关闭."),
	CannotReadOMPSCycleRolesConfigurationUseModel:                message("Cannot read OMP's cycle roles for this configuration. Use /model provider/model.", "无法读取此配置的 OMP 循环角色. 请使用 /model provider/model."),
	CannotRegisterTelegramAttachmentDeliveryInstanceClosed:       message("Cannot register Telegram attachment delivery. The instance has been closed.", "无法注册 Telegram 附件发送功能. 实例已关闭."),
	CannotResolveProjectPath:                                     message("Cannot resolve the project path.", "无法解析项目路径."),
	CannotRestoreSourceSessionMigrationNotStarted:                message("Cannot restore the source session. Migration was not started.", "无法恢复源会话, 未开始迁移."),
	CheckingMigrationPrerequisites:                               message("Checking migration prerequisites.", "正在检查迁移前置条件."),
	ChooseCycleRoleModelProviderModel:                            message("Choose a cycle role or /model provider/model", "选择模型角色或使用 /model provider/model"),
	ChooseThinkingLevelCurrentEffectiveLevelOMPMayAdjust:         message("Choose a thinking level\nCurrent effective level: %s\nOMP may adjust the requested level to match the model's capabilities.", "选择思考级别\n当前有效级别: %s\nOMP 可能根据模型能力调整请求的级别."),
	ChooseOMPCycleRoleCurrentModel:                               message("Choose an OMP cycle role\nCurrent model: %s", "选择 OMP 循环角色\n当前模型: %s"),
	ChooseOption:                                                 message("Choose an option", "请选择一个选项"),
	ToggleNativeFastMode:                                         message("Toggle native fast mode: /fast [on|ultra|off|status]", "切换原生快速模式: /fast [on|ultra|off|status]"),
	ChoosePinOmpSessionWorkingDirectory:                          message("Choose or pin an omp session in this working directory", "选择或置顶当前工作目录中的 omp 会话"),
	ChooseThinkingLevelSession:                                   message("Choose the thinking level for this session", "选择当前会话的思考级别"),
	ClearAutoresearch:                                            message("Clear autoresearch", "清理自动研究"),
	ClearAutoresearchNativeClearCanHardResetTrackedFiles:         message("Clear autoresearch? Native clear can hard-reset tracked files and delete untracked files, unless --keep-tree is supplied. This cannot be undone by the bridge.", "清理自动研究? 除非指定 --keep-tree, 原生清理可能硬重置已跟踪文件并删除未跟踪文件. 桥接器无法撤销此操作."),
	Close: message("Close", "关闭"),
	CloseForgetIdleBindingActiveTasksPendingWorkCannot: message("Close and forget idle binding for %s?\nActive tasks and pending work cannot be deleted. Workspace and native OMP session history will NOT be deleted.", "关闭并忘记 %s 的空闲绑定?\n无法删除活动任务及待处理工作. 工作目录和原生 OMP 会话历史不会被删除."),
	CloseOmpKeepWorkspaceSession:                       message("Close omp, keep workspace and session", "关闭 omp, 保留工作目录和会话"),
	Closed:                                             message("Closed", "已关闭"),
	CommandsUnknownUnsupportedSourceNotSupportedBridgeNoCommand: message("Commands with an unknown or unsupported source are not supported by the bridge. No command was submitted.", "桥接器不支持来源未知或不受支持的命令. 未提交命令."),
	CompactContextWhileIdle:    message("Compact context while idle", "空闲时压缩上下文"),
	CompactingContext:          message("Compacting context...", "正在压缩上下文..."),
	Compacting:                 message("Compacting...", "正在压缩..."),
	CompactionCompletedContext: message("Compaction completed.\nContext: %s", "压缩已完成.\n上下文: %s"),
	CompactionFailedOutcomeUncertainInstanceClosedWillResumeOn: message("Compaction failed or its outcome is uncertain. The instance was closed and will resume on the next request.", "压缩失败或结果不确定. 实例已关闭, 将在下次请求时恢复."),
	CompactionCanceledInstanceClosedWillResumeOnNextRequest:    message("Compaction was canceled. The instance was closed and will resume on the next request.", "压缩已取消. 实例已关闭, 将在下次请求时恢复."),
	Config:                                 message("Config", "配置"),
	Confirm:                                message("Confirm", "确认"),
	ConfirmationRequired:                   message("Confirmation required", "需要确认"),
	Confirmed:                              message("Confirmed", "已确认"),
	Connected:                              message("Connected", "已连接"),
	CouldNotCloseBindingsMenuTapCloseAgain: message("Could not close the bindings menu. Tap Close again.", "无法关闭绑定菜单. 请再次点击关闭."),
	CouldNotCloseQueueMenuTapCloseAgain:    message("Could not close the queue menu. Tap Close again.", "无法关闭队列菜单. 请再次点击关闭."),
	CurrentOMPArgumentPresetAsteriskMarksCurrentPresetChoose: message("Current OMP argument preset: %d\nAsterisk marks the current preset.\nChoose a preset:", "当前 OMP 参数预设: %d\n星号标记当前预设.\n请选择预设:"),
	Database:     message("Database", "数据库"),
	Delete:       message("Delete", "删除"),
	DeleteFailed: message("Delete failed", "删除失败"),
	DeleteOmpSessionTitleIDDeletesSavedOMPSession: message("Delete omp session?\n\nTitle: %s\nID: %s\n\nThis deletes the saved OMP session and its artifacts.\nThe workspace files will not be deleted.", "删除 omp 会话?\n\n标题: %s\nID: %s\n\n此操作将删除已保存的 OMP 会话及其产物.\n工作目录中的文件不会被删除."),
	DeleteUnavailable:                    message("Delete unavailable", "无法删除"),
	Deleted:                              message("Deleted", "已删除"),
	Deleting:                             message("Deleting", "正在删除"),
	DiagnosticsAlreadyRunning:            message("Diagnostics are already running.", "诊断已在运行."),
	DiagnosticsTimedOutRunDoctorAgain:    message("Diagnostics timed out. Run /doctor again.", "诊断超时. 请重新运行 /doctor."),
	DiagnosticsUnavailableRunDoctorAgain: message("Diagnostics unavailable. Run /doctor again.", "诊断不可用. 请重新运行 /doctor."),
	Disk:                                 message("Disk", "磁盘"),
	Export:                               message("Export", "导出"),
	ExportOmpSessionExportHtmlSessionId:  message("Export an omp session: /export [html] [session-id]", "导出 omp 会话: /export [html] [session-id]"),
	ExportOmpSession:                     message("Export omp session", "导出 omp 会话"),
	ExtensionCommandsNotSupportedBridgeBecauseTheyCanChange: message("Extension commands are not supported by the bridge because they can change the native session without updating its binding. No command was submitted.", "桥接器不支持扩展命令, 因为它们可能更改原生会话却不更新绑定. 未提交命令."),
	FAIL:                                    message("FAIL", "失败"),
	FailedCheckSessionStateNoSessionDeleted: message("Failed to check the session state. No session was deleted.", "检查会话状态失败. 未删除任何会话."),
	FailedCloseSavedBinding:                 message("Failed to close the saved binding.", "关闭已保存绑定失败."),
	FailedCreateWorkingDirectory:            message("Failed to create the working directory.", "无法创建工作目录."),
	FailedDeleteOmpSessionUseResumeCheckStatusBefore:       message("Failed to delete the omp session. Use /resume to check its status before trying again.", "删除 omp 会话失败. 请使用 /resume 检查状态后再重试."),
	FailedDeleteSavedBinding:                               message("Failed to delete the saved binding.", "删除已保存绑定失败."),
	FailedDisplaySavedBindingsUseBindingsTryAgain:          message("Failed to display saved bindings. Use /bindings to try again.", "显示已保存绑定失败. 请使用 /bindings 重试."),
	FailedDisplayNativeSelectionMenuDialogCanceled:         message("Failed to display the native selection menu. The dialog was canceled.", "无法显示原生选择菜单. 对话框已取消."),
	FailedDisplayQueueUseQueueTryAgain:                     message("Failed to display the queue. Use /queue to try again.", "显示队列失败. 请使用 /queue 重试."),
	FailedDisplaySessionSelectionMenuUseTryAgain:           message("Failed to display the session selection menu. Use %s to try again.", "显示会话选择菜单失败. 请使用 %s 重试."),
	FailedReadModelSettings:                                message("Failed to read model settings.", "读取模型设置失败."),
	FailedReadPinnedSessions:                               message("Failed to read pinned sessions.", "读取置顶会话失败."),
	FailedReadSavedBindings:                                message("Failed to read saved bindings.", "读取已保存绑定失败."),
	FailedReadSessionBinding:                               message("Failed to read the session binding.", "无法读取会话绑定."),
	FailedReadSessionState:                                 message("Failed to read the session state.", "无法读取会话状态."),
	FailedReadSession:                                      message("Failed to read the session.", "无法读取会话."),
	FailedResumeOmpStartupOutcomeUncertainUseCloseBefore:   message("Failed to resume omp. The startup outcome is uncertain; use /close before retrying.", "无法恢复 omp. 启动结果不确定; 请先使用 /close 再重试."),
	FailedResumeReleasedOMPSessionNoPromptSubmitted:        message("Failed to resume the released OMP session. No prompt was submitted.", "无法恢复已释放的 OMP 会话. 未提交提示词."),
	FailedSaveOMPArgumentPresetSelection:                   message("Failed to save the OMP argument preset selection.", "无法保存 OMP 参数预设选择."),
	FailedSaveSessionBindingInstanceClosed:                 message("Failed to save the session binding. The instance has been closed.", "无法保存会话绑定. 实例已关闭."),
	FailedSaveStartupIntent:                                message("Failed to save the startup intent: %s", "无法保存启动意图: %s"),
	FailedSendConfirmationOperationCanceled:                message("Failed to send the confirmation. The operation was canceled.", "无法发送确认消息. 操作已取消."),
	FailedStartOmpStartupOutcomeUncertainUseCloseBefore:    message("Failed to start omp. The startup outcome is uncertain; use /close before retrying.", "无法启动 omp. 启动结果不确定; 请先使用 /close 再重试."),
	FailedUpdatePinnedSessions:                             message("Failed to update pinned sessions.", "更新置顶会话失败."),
	ForgetSavedBindingWorkspaceNativeOMPSessionHistoryWill: message("Forget saved binding for %s?\nWorkspace and native OMP session history will NOT be deleted.", "忘记 %s 的已保存绑定?\n工作目录和原生 OMP 会话历史不会被删除."),
	FormatLine: message("Format: %s\n", "格式: %s\n"),
	HandoffCanceledWithoutResultInstanceClosedWillResumeOn: message("Handoff canceled without a result. The instance was closed and will resume on the next request.", "交接已取消且没有结果. 实例已关闭, 将在下次请求时恢复."),
	HandoffCompleted: message("Handoff completed.", "交接已完成."),
	HandoffFailedOutcomeUncertainInstanceClosedWillResumeOn: message("Handoff failed or its outcome is uncertain. The instance was closed and will resume on the next request.", "交接失败或结果不确定. 实例已关闭, 将在下次请求时恢复."),
	HandoffRequested: message("Handoff requested.", "已请求交接."),
	HandoffResultCouldNotConfirmedInstanceClosedWillResume: message("Handoff result could not be confirmed. The instance was closed and will resume on the next request.", "无法确认交接结果. 实例已关闭, 将在下次请求时恢复."),
	Inbox: message("Inbox", "收件箱"),
	InputDialogsNotSupportedCanceledProvideInformationNormalMessage: message("Input dialogs are not supported and have been canceled. Provide the information in a normal message.", "不支持输入对话框, 已取消. 请通过普通消息提供信息."),
	InvalidWorkingDirectory:                                          message("Invalid working directory.", "工作目录无效."),
	InvalidWorkingDirectoryExistingInstanceStillRunning:              message("Invalid working directory. The existing instance is still running.", "工作目录无效. 现有实例仍在运行."),
	ListSavedBindingsBindingsOld:                                     message("List saved bindings: /bindings [old]", "列出已保存的绑定: /bindings [old]"),
	MainChat:                                                         message("Main chat", "主聊天"),
	MigrateContextMigrateRecoverOldSessionIdSessionFile:              message("Migrate context, or /migrate recover old|session-id|session-file", "迁移上下文, 或 /migrate recover old|session-id|session-file"),
	MigrationCompleteWorkspaceSessionOldSessionPreservedCanRestored:  message("Migration complete.\nWorkspace: %s\nSession: %s\nThe old session was preserved and can be restored with /resume.", "迁移完成.\n工作目录: %s\n会话: %s\n旧会话已保留, 可通过 /resume 恢复."),
	MigrationExplicitlyRecoveredWorkspaceSession:                     message("Migration explicitly recovered.\nWorkspace: %s\nSession: %s", "迁移已显式恢复.\n工作目录: %s\n会话: %s"),
	MigrationOnlyAllowedWhenTasksQueueSessionControlsAll:             message("Migration is only allowed when tasks, the queue, and session controls are all idle.", "仅允许在任务, 队列及会话控制全部空闲时迁移."),
	MigrationUnresolvedInputNeitherQueuedNorSubmittedUseStatus:       message("Migration is unresolved. This input was neither queued nor submitted. Use /status for explicit recovery options.", "迁移尚未解决, 本条输入未排队也未提交. 使用 /status 查看显式恢复方式."),
	MigrationStartedHandoffMayTakeUpSBindingWill:                     message("Migration started. Handoff may take up to 180s. The binding will not switch until verification completes.", "开始迁移. Handoff 最多等待 180s, 完成验证前不会切换绑定."),
	MigrationUnresolved:                                              message("Migration unresolved", "迁移尚未解决"),
	MigrationUnresolvedOperationPhaseSourceSession:                   message("Migration unresolved.\nOperation: %s\nPhase: %s\nSource session: %s", "迁移尚未解决.\n操作: %s\n阶段: %s\n源会话: %s"),
	MigrationNotConfirmedRuntimeStoppedBindingMigrationLockPreserved: message("Migration was not confirmed. The runtime was stopped; the binding and migration lock were preserved. No automatic retry will occur.\n", "迁移未确认, Runtime 已停止, 绑定和迁移锁已保留. 不会自动重试.\n"),
	MigrationNotStartedCheckOmpTelegramMigrateV1Extension:            message("Migration was not started. Check that the omp_telegram_migrate_v1 extension is explicitly loaded through omp.args, and that the source session can be restored and is fully idle.", "未开始迁移. 请确认已通过 omp.args 显式加载 omp_telegram_migrate_v1 Extension, 且源会话可恢复并已完全空闲."),
	ModelSwitched:           message("Model switched to %s.", "模型已切换为 %s."),
	NameOmpSessionNameTitle: message("Name the omp session: /name <title>", "命名 omp 会话: /name <标题>"),
	NativeCommandCompletionCouldNotConfirmedTaskOutcomeUncertain:      message("Native command completion could not be confirmed. The task outcome is uncertain and will not be replayed automatically.", "无法确认原生命令完成. 任务结果不确定, 不会自动重放."),
	NativeCommandDiscoveryUnavailableWaitingSlashCommandsNotSubmitted: message("Native command discovery is unavailable. Waiting slash commands were not submitted; the running task and runtime were preserved. Use /queue and Cancel on the waiting slash item to let later tasks continue.", "原生命令发现不可用. 未提交等待中的斜杠命令; 当前任务和运行时保持不变. 使用 /queue 并取消等待中的斜杠命令, 让后续任务继续."),
	NativeCommandFailedBeforeProducingConfirmedResultTaskOutcome:      message("Native command failed before producing a confirmed result. The task outcome is uncertain and will not be replayed automatically.", "原生命令在产生确认结果前失败. 任务结果不确定, 不会自动重放."),
	NativeControlUnavailable:                                message("The native control command is unavailable in this session. No command was submitted.", "此会话的原生控制命令不可用. 未提交命令."),
	NativeControlUnconfirmed:                                message("The native control result could not be confirmed and will not be replayed automatically. Use /fast status to check before retrying.", "无法确认原生控制结果, 不会自动重放. 请先使用 /fast status 检查, 再重试."),
	NativeDiagnosticNotificationOmitted:                     message("Native diagnostic notification omitted.", "已省略原生诊断通知."),
	NativeOmpSessionListingFailed:                           message("Native omp session listing failed: %s", "原生 omp 会话列表读取失败: %s"),
	NewSessionNewNameProjectPath:                            message("New session: /new <name or project path>", "新建会话: /new <名称或项目路径>"),
	Next:                                                    message("Next", "下一页"),
	NoActiveTaskQueuedPromptsCleared:                        message("No active task. Queued prompts cleared.", "没有活动任务. 已清空排队提示词."),
	NoCycleRolesConfiguredOMPUseModelProviderModel:          message("No cycle roles are configured in OMP. Use /model provider/model.", "OMP 未配置循环角色. 请使用 /model provider/model."),
	NoInstanceRunningConversationStartNewNameProjectPath:    message("No instance is running in this conversation. Start with /new <name or project path>, or use /resume for a saved session.", "当前对话没有运行中的实例. 使用 /new <名称或项目路径> 创建, 或用 /resume 恢复已保存的会话."),
	NoInstanceRunningGlobalUncertainRecordsUseResumeRestore: message("No instance is running. Global uncertain records: %d. Use /resume to restore a session; tasks are not replayed automatically.", "没有运行中的实例. 全局不确定记录: %d. 使用 /resume 恢复会话; 任务不会自动重放."),
	NoSavedConversationBindings:                             message("No saved conversation bindings.", "没有已保存的会话绑定."),
	NoWorkingDirectorySelectedUseNewNamePathResume:          message("No working directory is selected. Use /new <name or path>, or /resume first.", "尚未选择工作目录. 请先使用 /new <名称或路径> 或 /resume."),
	OK:                                     message("OK", "正常"),
	OMPArgumentPresetChangedUseResumeAgain: message("OMP argument preset changed. Use /resume again.", "OMP 参数预设已更改. 请重新使用 /resume."),
	OMPBinary:                              message("OMP binary", "OMP 程序"),
	OMPDidNotReportThinkingLevelSession:    message("OMP did not report a thinking level for this session.", "OMP 未报告此会话的思考级别."),
	OMPDidNotReportResultingThinkingLevelChangeCould:  message("OMP did not report the resulting thinking level. The change could not be confirmed.", "OMP 未报告修改后的思考级别. 无法确认修改结果."),
	OMPDoesNotSupportNativeCommandDiscoverySlashInput: message("OMP does not support native command discovery. Slash input was cancelled because its command source cannot be verified. Use an OMP version with command discovery, or send ordinary text without a leading slash.", "OMP 不支持原生命令发现. 无法验证命令来源, 已取消斜杠输入. 请使用支持命令发现的 OMP 版本, 或发送不以斜杠开头的普通文本."),
	OMPAlreadyStarting:            message("OMP is already starting.", "OMP 已在启动中."),
	OMPStarting:                   message("OMP is starting", "OMP 正在启动"),
	OMPStartingTryAgainAfterReady: message("OMP is starting. Try again after it is ready.", "OMP 正在启动. 请在就绪后重试."),
	OMPNativeCommand:              message("OMP native command", "OMP 原生命令"),
	OnlyBindingAnotherConversationWithoutPendingStartCanDeleted: message("Only a binding in another conversation without a pending start can be deleted.", "只能删除其他会话中没有待启动操作的绑定."),
	Open:                message("Open", "已打开"),
	Outbox:              message("Outbox", "发件箱"),
	PageChanged:         message("Page changed.", "已切换页面."),
	Pending:             message("Pending %s", "等待 %s"),
	PendingNew:          message("Pending new", "等待新建"),
	PendingResume:       message("Pending resume", "等待恢复"),
	Pin:                 message("Pin", "置顶"),
	PreparingAttachment: message("Preparing attachment...", "正在准备附件..."),
	Preset:              message("Preset %d", "预设 %d"),
	Previous:            message("Previous", "上一页"),
	Processing:          message("Processing...", "处理中..."),
	PromptExceedsOmpRPCFrameLimitNoTaskSubmitted: message("Prompt exceeds the omp RPC frame limit. No task was submitted.", "提示词超过 omp RPC 帧大小限制. 未提交任务."),
	ProvideOldFullSessionIDAbsolutePathNewJSONL:  message("Provide old, a full session ID, or the absolute path to the new JSONL file.", "请提供 old, 完整会话 ID, 或新 JSONL 的绝对路径."),
	QueueRunningPending:                          message("Queue\nRunning: %s\nPending: %d", "队列\n运行中: %s\n待处理: %d"),
	QueuePromptAfterCurrentTask:                  message("Queue a prompt after the current task", "将消息加入当前任务之后的队列"),
	QueuedAlbum:                                  message("Queued album", "排队中的相册"),
	QueuedAttachment:                             message("Queued attachment", "排队中的附件"),
	QueuedTask:                                   message("Queued task", "排队中的任务"),
	Received:                                     message("Received", "已收到"),
	RecentTools:                                  message("Recent tools:", "最近的工具:"),
	Released:                                     message("Released", "已释放"),
	ReplyTruncated:                               message("Reply truncated because it exceeded the Telegram display budget.", "回复超过 Telegram 显示预算, 已截断."),
	Result:                                       message("Result", "结果"),
	RetryingAutomatically:                        message("Retrying automatically...", "正在自动重试..."),
	RunNativeHandoffHandoffInstructions:          message("Run native handoff: /handoff [instructions]", "执行原生 handoff: /handoff [指令]"),
	RunOmpReviewReviewArguments:                  message("Run omp review: /review [arguments]", "执行 omp 审查: /review [参数]"),
	RunSafeDiagnostics:                           message("Run safe diagnostics", "运行安全诊断"),
	Running:                                      message("Running", "运行中"),
	ToolRunningPhase:                             message("Running tool...", "正在运行工具..."),
	RunningTools:                                 message("Running tools...", "正在运行多个工具..."),
	Runtime:                                      message("Runtime", "运行时"),
	SQLiteCheckUnavailable:                       message("SQLite check unavailable", "SQLite 检查不可用"),
	SQLiteQuickCheckFailed:                       message("SQLite quick check failed", "SQLite 快速检查失败"),
	SavedBindingDeletedWorkspaceNativeOMPSessionHistoryPreserved: message("Saved binding deleted. Workspace and native OMP session history were preserved.", "已删除保存的绑定. 工作目录和原生 OMP 会话历史已保留."),
	SavedBindingsPage:                 message("Saved bindings\nPage %d/%d", "已保存的绑定\n第 %d/%d 页"),
	SavedBindingsOldestFirstPage:      message("Saved bindings (oldest first)\nPage %d/%d", "已保存的绑定 (最早使用优先)\n第 %d/%d 页"),
	SavedOmpSessions:                  message("Saved omp sessions", "已保存的 omp 会话"),
	SelectOMPArgumentPresetArgsNumber: message("Select OMP argument preset: /args [number]", "选择 OMP 参数预设: /args [编号]"),
	ArgumentPresetSelected:            message("Selected OMP argument preset %d.", "已选择 OMP 参数预设 %d."),
	NamedArgumentPresetSelected:       message("Selected OMP argument preset %d: %s.", "已选择 OMP 参数预设 %d: %s."),
	Selected:                          message("Selected: %s", "已选择: %s"),
	SelectedID:                        message("Selected: %s\nID: %s", "已选择: %s\nID: %s"),
	SelectionNotChanged:               message("Selection was not changed.", "选择未更改."),
	SendAutoresearchDirectlyUsingCurrentNativeExtensionCommandNot: message("Send /autoresearch directly using the current native extension command, not through a followup or attachment. No command was submitted.", "请直接使用当前原生扩展命令发送 /autoresearch, 不要通过后续消息或附件发送. 未提交命令."),
	SessionHTMLExportFailed:                                               message("Session HTML export failed.", "会话 HTML 导出失败."),
	SessionHTMLExportTimedOut:                                             message("Session HTML export timed out.", "会话 HTML 导出超时."),
	SessionDeletedButFailedClearSavedBindingsPins:                         message("Session deleted, but failed to clear saved bindings and pins.", "会话已删除, 但清理保存的绑定和置顶状态失败."),
	SessionDeleted:                                                        message("Session deleted.", "会话已删除."),
	SessionExportFailed:                                                   message("Session export failed.", "会话导出失败."),
	SessionExportTooLargeSendThroughTelegram:                              message("Session export is too large to send through Telegram.", "会话导出文件过大, 无法通过 Telegram 发送."),
	SessionMigrationUnresolved:                                            message("Session migration is unresolved.", "会话迁移尚未解决."),
	SessionMigrationUnresolvedCannotCreateSwitchBindings:                  message("Session migration is unresolved. Cannot create or switch bindings.", "会话迁移尚未解决. 不能新建或切换绑定."),
	SessionMigrationUnresolvedExportUnavailableUseMigrateRecoverFirst:     message("Session migration is unresolved. Export is unavailable. Use /migrate recover first.", "会话迁移尚未解决, 不能导出. 请先使用 /migrate recover 恢复."),
	SessionMigrationUnresolvedSessionSelectionExportUnavailableUseMigrate: message("Session migration is unresolved. Session selection and export are unavailable. Use /migrate recover first.", "会话迁移尚未解决, 不能选择或导出会话. 请先使用 /migrate recover 恢复."),
	MigrationUnresolvedCannotDeleteBinding:                                message("Session migration is unresolved. The binding cannot be deleted. Use /migrate recover first.", "会话迁移尚未解决, 不能删除绑定. 请先使用 /migrate recover 恢复."),
	MigrationUnresolvedCannotDeleteOtherBinding:                           message("Session migration is unresolved. The binding cannot be deleted. Use /migrate recover in that conversation first.", "会话迁移尚未解决, 不能删除绑定. 请先在对应会话使用 /migrate recover 恢复."),
	SessionMigrationUnresolvedUseMigrateRecoverRecoverExplicitly:          message("Session migration is unresolved. Use /migrate recover to recover explicitly.", "会话迁移尚未解决. 使用 /migrate recover 显式恢复."),
	SessionNamed: message("Session named: %s", "会话已命名: %s"),
	SettingsChangeInterruptedResultCouldNotConfirmed:  message("Settings change interrupted. The result could not be confirmed.", "设置修改已中断. 无法确认结果."),
	ShowCancelPendingBridgeTasks:                      message("Show and cancel pending bridge tasks", "查看或取消 Bridge 等待中的任务"),
	ShowSessionModelContextSpeedQueue:                 message("Show session, model, context, speed and queue", "查看会话, 模型, 上下文, 速度和队列"),
	CommandHelp:                                       message("Show usage help", "显示使用帮助"),
	StartNewOmpSessionExistingFilesSessionHistoryWill: message("Start a new omp session in: %s?\nExisting files and session history will be preserved.", "在 %s 中启动新的 omp 会话?\n现有文件和会话历史将保留."),
	Starting: message("Starting", "启动中"),
	StateChangedDuringDiagnosticsRunDoctorAgain: message("State changed during diagnostics. Run /doctor again.", "诊断期间状态发生变化. 请重新运行 /doctor."),
	StatusValue: message("Status: %s", "状态: %s"),
	SteeringFailedCurrentTaskMayNotIncludeInstruction:                   message("Steering failed. The current task may not include that instruction.", "引导失败. 当前任务可能未包含该指令."),
	SteeringOutcomeUncertainWillNotReplayedAutomatically:                message("Steering outcome is uncertain and will not be replayed automatically.", "引导结果不确定, 不会自动重放."),
	SteeringStateCouldNotConfirmedTaskOutcomeUncertainWill:              message("Steering state could not be confirmed. The task outcome is uncertain and will not be replayed automatically.", "无法确认引导状态. 任务结果不确定, 不会自动重放."),
	SteeringInterruptedTaskOutcomeUncertainWillNotReplayedAutomatically: message("Steering was interrupted. The task outcome is uncertain and will not be replayed automatically.", "引导已中断. 任务结果不确定, 不会自动重放."),
	Stop:               message("Stop", "停止"),
	StopTaskClearQueue: message("Stop task and clear queue", "停止任务并清空队列"),
	Stopping:           message("Stopping", "正在停止"),
	Storage:            message("Storage", "存储"),
	SubmissionFailedOutcomeUncertainTaskWillNotReplayedAutomatically: message("Submission failed or its outcome is uncertain. This task will not be replayed automatically. The session was kept open.", "提交失败或结果不确定. 此任务不会自动重放. 会话保持打开."),
	TaskNoLongerQueued:            message("Task is no longer queued.", "任务已不在队列中."),
	TaskCancelledBeforeCompletion: message("Task was cancelled before completion.", "任务在完成前已取消."),
	Telegram:                      message("Telegram", "Telegram"),
	AbortRequestFailed:            message("The abort request failed.", "中止请求失败."),
	InstanceLimitReached:          message("The active instance limit has been reached.", "已达到活动实例上限."),
	AutoresearchControlStillLoadingNoCommandSubmitted:                     message("The autoresearch control is still loading. No command was submitted.", "自动研究控制仍在加载. 未提交命令."),
	ConfirmationCouldNotDeliveredOmpStateUncertainWillNot:                 message("The confirmation could not be delivered to omp. Its state is uncertain and will not be replayed automatically. The session was kept open.", "无法向 omp 发送确认. 状态不确定, 不会自动重放. 会话保持打开."),
	ConversationBecameBusyUseResumeAgainWhenIdle:                          message("The conversation became busy. Use /resume again when it is idle.", "会话已变为忙碌状态. 请在空闲时重新使用 /resume."),
	ConversationBusyUseAgainAfterTaskQueueFinish:                          message("The conversation is busy. Use %s again after the task and queue finish.", "会话忙碌中. 请在任务和队列完成后重新使用 %s."),
	InstanceClosedSessionPreserved:                                        message("The instance is closed. The session has been preserved.", "实例已关闭. 会话已保留."),
	MigrationOperationStillRunningUseStopStopRuntimeBefore:                message("The migration operation is still running. Use /stop to stop the runtime before explicitly recovering.", "迁移操作仍在执行. 先使用 /stop 停止 Runtime, 再显式恢复."),
	ModelSelectionUnavailableOpenModelChooseAgain:                         message("The model selection is unavailable. Open /model to choose again.", "模型选项不可用. 请打开 /model 重新选择."),
	ModelSwitchCouldNotConfirmedUseStatusCheckActual:                      message("The model switch could not be confirmed. Use /status to check the actual model before retrying.", "无法确认模型切换结果. 请先使用 /status 检查实际模型, 再重试."),
	NumberOptionsUnsupportedDialogCanceled:                                message("The number of options is unsupported. The dialog was canceled.", "不支持此选项数量. 对话框已取消."),
	OmpRequestFailedTaskOutcomeUncertainWillNotReplayed:                   message("The omp request failed. The task outcome is uncertain and will not be replayed automatically. The session was kept open.", "omp 请求失败. 任务结果不确定, 不会自动重放. 会话保持打开."),
	OmpSessionOperationStillLoading:                                       message("The omp session operation is still loading.", "omp 会话操作仍在加载."),
	OmpSessionOperationStillLoadingWaitFinish:                             message("The omp session operation is still loading. Wait for it to finish.", "omp 会话操作仍在加载中. 请等待完成."),
	PreviousRecoveryProcessStillExitingWaitReapedBeforeExplicitly:         message("The previous recovery process is still exiting. Wait for it to be reaped before explicitly recovering. The same session will not be started concurrently.", "上一次恢复进程仍在退出. 等待回收后再显式恢复, 不会并发启动同一会话."),
	ProjectPathMustAccessibleDirectory:                                    message("The project path must be an accessible directory.", "项目路径必须是可访问的目录."),
	QueuedNativeCommandNoLongerAvailableCanceledNotSent:                   message("The queued native command is no longer available. It was canceled and was not sent as a model prompt.", "排队的原生命令已不可用. 已取消该命令, 未将其作为模型提示词发送."),
	ResumedSessionDidNotMatchRequestedOmpSessionID:                        message("The resumed session did not match the requested omp session ID.", "恢复的会话与请求的 omp 会话 ID 不匹配."),
	SavedBindingChangedUseAgain:                                           message("The saved binding changed. Use %s again.", "已保存的绑定已更改. 请重新使用 %s."),
	SavedBindingChangedUseBindingsRefresh:                                 message("The saved binding changed. Use /bindings to refresh.", "已保存的绑定已更改. 请使用 /bindings 刷新."),
	SavedBindingChangedUseExportAgain:                                     message("The saved binding changed. Use /export again.", "已保存的绑定已更改. 请重新使用 /export."),
	SavedBindingChangedUseResumeAgain:                                     message("The saved binding changed. Use /resume again.", "已保存的绑定已更改. 请重新使用 /resume."),
	SavedOmpSessionFileWorkingDirectoryUnavailableSoStartup:               message("The saved omp session file or working directory is unavailable, so startup restore was skipped. Use /new to start a new session.", "保存的 omp 会话文件或工作目录不可用, 已跳过启动恢复. 使用 /new 开始新会话."),
	SavedSessionDeletedUsedElsewhereNoOMPProcessStarted:                   message("The saved session is being deleted or used elsewhere. No OMP process was started.", "已保存的会话正在删除或被其他对话使用. 未启动 OMP 进程."),
	SelectedSessionNotWorkingDirectory:                                    message("The selected session is not in this working directory.", "所选会话不在此工作目录中."),
	SessionListChangedUseResumeAgain:                                      message("The session list changed. Use /resume again.", "会话列表已变更. 请重新使用 /resume."),
	SessionNameChangeCouldNotConfirmedUseStatusCheck:                      message("The session name change could not be confirmed. Use /status to check before retrying.", "无法确认会话名称变更. 重试前请使用 /status 检查."),
	SessionOperationStillLoading:                                          message("The session operation is still loading.", "会话操作仍在加载中."),
	SessionStartedButTelegramTopicTitleCouldNotUpdated:                    message("The session started, but the Telegram topic title could not be updated.", "会话已启动, 但无法更新 Telegram 话题标题."),
	ThinkingLevelChangeCouldNotConfirmedOpenThinkingCheck:                 message("The thinking level change could not be confirmed. Open /thinking to check before retrying.", "无法确认思考级别修改结果. 请先打开 /thinking 检查, 再重试."),
	ThinkingSelectionUnavailableOpenThinkingChooseAgain:                   message("The thinking selection is unavailable. Open /thinking to choose again.", "思考级别选项不可用. 请打开 /thinking 重新选择."),
	WorkingDirectoryUnavailableNoSessionsLoaded:                           message("The working directory is unavailable. No sessions were loaded.", "工作目录不可用. 未加载任何会话."),
	WorkspaceRootMustAbsolutePath:                                         message("The workspace root must be an absolute path.", "工作区根目录必须是绝对路径."),
	ThereNoUnresolvedMigration:                                            message("There is no unresolved migration.", "没有待解决的迁移."),
	ThinkingLevel:                                                         message("Thinking level: %s", "思考级别: %s"),
	ThinkingLevelOMPAdjustedRequestedLevel:                                message("Thinking level: %s (OMP adjusted the requested %s level)", "思考级别: %s (OMP 调整了请求的 %s 级别)"),
	ActionExpired:                                                         message("This action has expired", "此操作已过期"),
	BotSupportsPrivateChatsTelegramTopicsGroupsUseInside:                  message("This bot supports private chats and Telegram topics. In groups, use it inside a topic.", "此机器人支持私聊和 Telegram 话题. 在群组中请在话题内使用."),
	OmpSessionAlreadyActiveConversation:                                   message("This omp session is already active in this conversation.", "此 omp 会话已在当前对话中运行."),
	OmpSessionUnavailableAlreadyUseAnotherConversation:                    message("This omp session is unavailable or already in use by another conversation.", "此 omp 会话不可用或已被其他对话使用."),
	SavedSessionUseAnotherConversationRunStatusThereClose:                 message("This saved session is in use by another conversation. Run /status there, or /close that instance before using /resume here.", "此已保存的会话正被其他对话使用. 请在那里运行 /status, 或先 /close 该实例再在此使用 /resume."),
	SessionActiveAnotherConversationCloseInstanceFirst:                    message("This session is active in another conversation. Close that instance first.", "此会话正在另一个对话中运行. 请先关闭该实例."),
	SessionActiveIdentityStillRestoredWaitWorkFinishBefore:                message("This session is active or its identity is still being restored. Wait for that work to finish before deleting it.", "此会话正在运行或仍在恢复身份. 请等待相关工作完成后再删除."),
	SessionAlreadyRunningAnotherConversationCloseInstanceFirstUse:         message("This session is already running in another conversation. Close that instance first, or use a full session ID.", "此会话已在其他对话中运行. 请先关闭该实例, 或使用完整会话 ID."),
	SessionDeletedTryAgainAfterOperationFinishes:                          message("This session is being deleted. Try again after the operation finishes.", "此会话正在删除. 请在操作完成后重试."),
	SessionCurrentlyActiveAnotherConversationCloseInstanceBeforeExporting: message("This session is currently active in another conversation. Close that instance before exporting it.", "此会话正在另一个对话中运行. 请先关闭该实例再导出."),
	SessionCurrentlyActiveProcessedAnotherConversationOperationNoDeletion: message("This session is currently active or being processed in another conversation or operation. No deletion was started. Stop or close it, or wait for the work to finish, then retry.", "此会话正在另一个对话或操作中运行或处理. 未开始删除. 请停止或关闭会话, 或等待工作完成后重试."),
	SessionCurrentlyActiveProcessedAnotherConversationNoDeletionStarted:   message("This session is currently active or being processed in another conversation. No deletion was started. Stop or close it there, or wait for the operation to finish, then retry.", "此会话正在另一个对话中运行或处理. 未开始删除. 请在该对话中停止或关闭会话, 或等待操作完成后重试."),
	SessionCurrentlyActiveProcessedConversationNoDeletionStartedStop:      message("This session is currently active or being processed in this conversation. No deletion was started. Stop or close it, or wait for the operation to finish, then retry.", "此会话正在当前对话中运行或处理. 未开始删除. 请停止或关闭会话, 或等待操作完成后重试."),
	SessionCurrentlyExportedTryAgainAfterExportFinishes:                   message("This session is currently being exported. Try again after that export finishes.", "此会话正在导出. 请等待导出完成后重试."),
	TooManyCycleRolesTelegramMenuUseModelProvider:                         message("Too many cycle roles for a Telegram menu. Use /model provider/model.", "循环角色过多, 无法放入 Telegram 菜单. 请使用 /model provider/model."),
	Tools:                                    message("Tools:", "工具:"),
	Topic:                                    message("Topic %d", "话题 %d"),
	Unpin:                                    message("Unpin", "取消置顶"),
	Untitled:                                 message("Untitled", "无标题"),
	Update:                                   message("Update", "更新"),
	UsageArgsPresetNumberInvalidPresetNumber: message("Usage: /args [preset-number]. Invalid preset number.", "用法: /args [preset-number]. 预设编号无效."),
	UsageBindingsOld:                         message("Usage: /bindings [old]", "用法: /bindings [old]"),
	UsageCompact:                             message("Usage: /compact", "用法: /compact"),
	UsageDoctor:                              message("Usage: /doctor", "用法: /doctor"),
	UsageExportHtmlSessionId:                 message("Usage: /export [html] [session-id].", "用法: /export [html] [会话 ID]."),
	UsageFollowupMessage:                     message("Usage: /followup <message>", "用法: /followup <消息>"),
	UsageMigrateMigrateRecoverOldMigrateRecoverSessionId:  message("Usage: /migrate, /migrate recover old, or /migrate recover <session-id or absolute file path>.", "用法: /migrate, /migrate recover old, 或 /migrate recover <session-id 或绝对文件路径>."),
	UsageModelModelProviderModel:                          message("Usage: /model or /model provider/model", "用法: /model 或 /model provider/model"),
	UsageNameSessionTitle:                                 message("Usage: /name <session title>", "用法: /name <会话标题>"),
	UsageQueue:                                            message("Usage: /queue", "用法: /queue"),
	UsageResumeResumeOmpSessionID:                         message("Usage: /resume, or /resume <omp session ID>.", "用法: /resume 或 /resume <omp 会话 ID>."),
	UsageThinking:                                         message("Usage: /thinking", "用法: /thinking"),
	UseMigrateRecoverOldRestoreSourceSession:              message("Use /migrate recover old to restore the source session.", "恢复源会话请使用 /migrate recover old."),
	UseSingleWorkspaceNameAbsoluteProjectPathPath:         message("Use a single workspace name, an absolute project path, or ~/path.", "请使用单个工作区名称, 项目绝对路径或 ~/path."),
	UseWorkspaceNameAbsoluteProjectPathPath:               message("Use a workspace name, an absolute project path, or ~/path.", "请使用工作区名称, 项目绝对路径或 ~/path."),
	WARN:                                                  message("WARN", "警告"),
	WaitCurrentSessionExportFinishBeforeChangingName:      message("Wait for the current session export to finish before changing its name.", "请等待当前会话导出完成后再更改名称."),
	WaitCurrentSessionExportFinish:                        message("Wait for the current session export to finish.", "请等待当前会话导出完成."),
	WaitCurrentTaskQueueFinishBeforeChangingModelSettings: message("Wait for the current task and queue to finish before changing model settings.", "请等待当前任务和队列完成后再修改模型设置."),
	WaitCurrentTaskQueueFinishBeforeDeletingSession:       message("Wait for the current task and queue to finish before deleting a session.", "请等待当前任务和队列完成后再删除会话."),
	WaitCurrentTaskQueueFinishBeforeExporting:             message("Wait for the current task and queue to finish before exporting.", "请等待当前任务和队列完成后再导出."),
	WaitCurrentTaskQueueFinishBeforeHandoff:               message("Wait for the current task and queue to finish before handoff.", "请等待当前任务和队列完成后再交接."),
	WaitCurrentTaskQueueFinishBeforeSelectingSession:      message("Wait for the current task and queue to finish before selecting a session.", "请等待当前任务和队列完成后再选择会话."),
	WaitCurrentTaskFinishBeforeChangingModelSettings:      message("Wait for the current task to finish before changing model settings.", "请等待当前任务完成后再修改模型设置."),
	Workspace: message("Workspace", "工作区"),
	WorkspacePathsCannotContainControlCharacters: message("Workspace paths cannot contain control characters.", "工作区路径不能包含控制字符."),
	DetailedStatus:                                message("Workspace: %s\nSession: %s\nModel: %s\nThinking: %s\nFast: %s\nContext: %s\nRunning: %s\nCompacting: %s\nQueued: %d\nSpeed: %s", "工作区: %s\n会话: %s\n模型: %s\n思考: %s\n快速模式: %s\n上下文: %s\n运行中: %s\n压缩中: %s\n排队: %d\n速度: %s"),
	AlbumNoAttachments:                            message("album has no attachments", "相册没有附件"),
	AlbumMemberNotPhotoDocument:                   message("album member is not a photo or document", "相册项目不是照片或文档"),
	AttachmentMissingFileIDExceedsMBDownloadLimit: message("attachment is missing a file ID or exceeds the 20 MB download limit", "附件缺少文件 ID 或超过 20 MB 下载限制"),
	AutoresearchSessionActiveAnotherConversation:  message("autoresearch session is active in another conversation", "自动研究会话正在另一个对话中运行"),
	AutoresearchSessionDeletedUsedElsewhere:       message("autoresearch session is being deleted or used elsewhere", "自动研究会话正在被删除或在其他位置使用"),
	Avg:                                           message("avg %s", "平均 %s"),
	CannotCreateAttachmentDirectory:               message("cannot create attachment directory", "无法创建附件目录"),
	CannotMarkAttachmentDirectory:                 message("cannot mark attachment directory", "无法标记附件目录"),
	CannotOpenAttachmentDirectory:                 message("cannot open attachment directory", "无法打开附件目录"),
	CannotOpenIncomingAttachmentDirectory:         message("cannot open incoming attachment directory", "无法打开传入附件目录"),
	CannotPersistAttachmentPreparationMarker:      message("cannot persist attachment preparation marker", "无法持久化附件准备标记"),
	CannotReadDownloadedAttachment:                message("cannot read downloaded attachment", "无法读取已下载的附件"),
	CannotSaveIncomingAttachmentSessionOwner:      message("cannot save incoming attachment session owner", "无法保存传入附件的会话归属."),
	Completed:                                     message("completed", "已完成"),
	RuntimeConnected:                              message("connected", "已连接"),
	CountUnavailable:                              message("count unavailable", "计数不可用"),
	DataDirectoryNotWritable:                      message("data directory is not writable", "数据目录不可写"),
	DirectoryUnavailable:                          message("directory unavailable", "目录不可用"),
	ExecutableCheckFailed:                         message("executable check failed", "可执行文件检查失败"),
	Failed:                                        message("failed", "失败"),
	Fast:                                          message("fast %s", "快速模式 %s"),
	FreeSpaceUnavailable:                          message("free space unavailable", "可用空间不可获取"),
	High:                                          message("high", "高"),
	InconsistentInternalRuntimeState:              message("inconsistent internal runtime state", "内部运行时状态不一致"),
	InvalidRuntimeConfiguration:                   message("invalid runtime configuration", "运行时配置无效"),
	JustNow:                                       message("just now", "刚刚"),
	Low:                                           message("low", "低"),
	Max:                                           message("max", "最大"),
	Medium:                                        message("medium", "中"),
	Minimal:                                       message("minimal", "最低"),
	Unavailable:                                   message("n/a", "不可用"),
	New:                                           message("new", "新建"),
	No:                                            message("no", "否"),
	NoneSelected:                                  message("none selected", "未选择"),
	NotSelected:                                   message("not selected", "未选择"),
	Off:                                           message("off", "关闭"),
	OmpDidNotRestoreSavedSessionFileInstanceClosed:                        message("omp did not restore the saved session file. The instance has been closed.", "omp 未恢复已保存的会话文件. 实例已关闭."),
	OmpDidNotRestoreSavedSessionIdentityInstanceClosed:                    message("omp did not restore the saved session identity. The instance has been closed.", "omp 未恢复已保存的会话标识. 实例已关闭."),
	OmpEndedWithoutConfirmedTextResultTaskOutcomeUncertain:                message("omp ended without a confirmed text result. The task outcome is uncertain and will not be replayed automatically.", "omp 已结束, 但没有确认的文本结果. 任务结果不确定, 不会自动重放."),
	OmpExitedDuringAutoresearchTaskOutcomeUncertainWillNot:                message("omp exited during autoresearch. The task outcome is uncertain and will not be replayed automatically.", "omp 在自动研究期间退出. 任务结果不确定, 不会自动重放."),
	OmpExitedWhileSteeringTaskOutcomeUncertainWillNot:                     message("omp exited while steering. The task outcome is uncertain and will not be replayed automatically.", "omp 在引导期间退出. 任务结果不确定, 不会自动重放."),
	OmpExitedTaskOutcomeUncertainWillNotReplayedAutomatically:             message("omp exited. The task outcome is uncertain and will not be replayed automatically. The session was kept open.", "omp 已退出. 任务结果不确定, 不会自动重放. 会话保持打开."),
	OmpNoSavedSessionsWorkingDirectory:                                    message("omp has no saved sessions in this working directory.", "此工作目录中没有已保存的 omp 会话."),
	OmpIdleButNoConfirmedTerminalResultReceivedTask:                       message("omp is idle but no confirmed terminal result was received. The task outcome is uncertain and will not be replayed automatically.", "omp 已空闲, 但未收到已确认的最终结果. 任务结果不确定, 不会自动重放."),
	OmpIdleButSteeringCompletionCouldNotConfirmedTask:                     message("omp is idle but steering completion could not be confirmed. The task outcome is uncertain and will not be replayed automatically.", "omp 已空闲, 但无法确认引导已完成. 任务结果不确定, 不会自动重放."),
	OmpReadyWorkspaceSession:                                              message("omp is ready.\nWorkspace: %s\nSession: %s", "omp 已就绪.\n工作目录: %s\n会话: %s"),
	OmpRejectedCompaction:                                                 message("omp rejected compaction.", "omp 拒绝了压缩."),
	OmpRejectedHandoff:                                                    message("omp rejected the handoff.", "omp 拒绝了交接."),
	OmpReportedTaskFailedBeforeProducingConfirmedResultReason:             message("omp reported that the task failed before producing a confirmed result. Reason: %s The task outcome is uncertain and will not be replayed automatically.", "omp 报告任务在产生确认结果前失败. 原因: %s 任务结果不确定, 不会自动重放."),
	OmpReportedTaskFailedBeforeProducingConfirmedResultTask:               message("omp reported that the task failed before producing a confirmed result. The task outcome is uncertain and will not be replayed automatically.", "omp 报告任务在产生确认结果前失败. 任务结果不确定, 不会自动重放."),
	OmpReportedTaskRateLimitedModelProviderTaskOutcome:                    message("omp reported that the task was rate-limited by the model provider. The task outcome is uncertain and will not be replayed automatically.", "omp 报告任务受到模型提供商限流. 任务结果不确定, 不会自动重放."),
	OmpReturnedUnsupportedSessionID:                                       message("omp returned an unsupported session ID.", "omp 返回了不支持的会话 ID."),
	OmpSelectedDifferentWorkingDirectoryInstanceClosed:                    message("omp selected a different working directory. The instance has been closed.", "omp 选择了不同的工作目录. 实例已关闭."),
	OmpSNativeSteeringQueueRemainedIdleTooLong:                            message("omp's native steering queue remained idle too long. The task outcome is uncertain and will not be replayed automatically.", "omp 的原生引导队列空闲时间过长. 任务结果不确定, 不会自动重放."),
	OmpTelegramDiagnostics:                                                message("omp-telegram diagnostics", "omp-telegram 诊断"),
	On:                                                                    message("on", "开启"),
	RuntimeReleased:                                                       message("released", "已释放"),
	ReleasedSessionFileUnavailable:                                        message("released session file unavailable", "已释放会话的文件不可用"),
	Resume:                                                                message("resume", "恢复"),
	SavedAutoresearchOwnerUnavailableRestoreSessionFileWorkingDirectory:   message("saved autoresearch owner is unavailable; restore its session file and working directory before disabling", "保存的自动研究所属会话不可用; 请先恢复其会话文件和工作目录, 再禁用"),
	SavedAutoresearchSessionUnavailableRestoreSessionFileWorkingDirectory: message("saved autoresearch session is unavailable; restore its session file and working directory before disabling", "保存的自动研究会话不可用; 请先恢复其会话文件和工作目录, 再禁用"),
	SavedSessionFileUnavailable:                                           message("saved session file is unavailable", "已保存的会话文件不可用"),
	SessionHistoryNotPersistedYet:                                         message("session history not persisted yet", "会话历史尚未持久化"),
	ResearchInstanceLimitReached:                                          message("the active instance limit has been reached", "已达到运行实例数上限"),
	Unknown:                                                               message("unknown", "未知"),
	Xhigh:                                                                 message("xhigh", "极高"),
	Yes:                                                                   message("yes", "是"),
	GatewayRestartedWhilePreviousTaskActiveInterruptedNotResubmitted:      message("⚠️ Gateway restarted while the previous task was active. It was interrupted and was not resubmitted.", "⚠️ 网关在上一个任务执行期间重启. 任务已中断, 未重新提交."),
}
