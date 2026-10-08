package webui

// LayoutView is embedded by every page view. CSRFToken is rendered into
// state-changing forms when non-empty. All strings are escaped by html/template.
type LayoutView struct {
	Title      string
	Brand      string
	CSRFToken  string
	ActiveNav  string
	User       *UserView
	Flash      *FlashView
	Error      string
	Notice     string
	IsLoggedIn bool
}

type UserView struct {
	ID             string
	Name           string
	Phone          string
	Role           string
	RoleLabel      string
	Status         string
	StatusLabel    string
	Initials       string
	JoinedAt       string
	LastLoginAt    string
	HasKey         bool
	KeyStatus      string
	KeyStatusLabel string
	KeyLast4       string
	KeyCreatedAt   string
	KeyLastUsedAt  string
	KeySyncStatus  string
}

type FlashView struct {
	Kind    string
	Message string
}

type NoticeView struct {
	Kind    string
	Title   string
	Message string
	Time    string
}

type RuleView struct {
	ID      string
	Title   string
	Body    string
	Checked bool
}

type LoginView struct {
	LayoutView
	Phone        string
	Remember     bool
	Next         string
	PasswordHint string
}

type RegisterView struct {
	LayoutView
	Phone         string
	Name          string
	Password      string
	Confirm       string
	Rules         []RuleView
	RulesVersion  string
	RulesRequired bool
}

type ResetView struct {
	LayoutView
	Phone           string
	Code            string
	NewPassword     string
	ConfirmPassword string
	MinimumLength   int
}

type ResubmitView struct {
	LayoutView
	Name         string
	Rules        []RuleView
	RulesVersion string
}

type StatusCardView struct {
	Status      string
	StatusLabel string
	Heading     string
	Description string
	Reason      string
	UpdatedAt   string
}

type KeyView struct {
	Status          string
	StatusLabel     string
	Last4           string
	CreatedAt       string
	LastUsedAt      string
	LastSyncedAt    string
	SyncStatus      string
	SyncStatusLabel string
	CanClaim        bool
	CanTest         bool
	CanRevoke       bool
	CanReset        bool
	OneTimeSecret   string
	APIBaseURL      string
	ConfigSnippet   string
	Alias           string
	Examples        []ConfigExampleView
}

type ConfigExampleView struct {
	Name string
	Hint string
	Code string
}

type UsageSummaryView struct {
	Requests        string
	Successes       string
	Failures        string
	InputTokens     string
	OutputTokens    string
	CacheTokens     string
	ReasoningTokens string
	TotalTokens     string
	AverageDuration string
	Latency         string
	WindowLabel     string
}

type UsagePointView struct {
	BucketMS            int64
	BucketHours         int
	LabelTick           int64
	Date                string
	Requests            string
	Tokens              string
	Percent             int
	TokenPercent        int
	RequestValue        int64
	TokenValue          int64
	SuccessValue        int64
	FailureValue        int64
	HasTokenBreakdown   bool
	InputTokenValue     int64
	CacheTokenValue     int64
	OutputTokenValue    int64
	ReasoningTokenValue int64
}

type HealthTrendPointView struct {
	UsageTrendPointView
	SuccessY, FailureY                              int
	HasRequests, HasTiming                          bool
	SuccessRate, FailureRate, AverageTotal, Samples string
	HasStages                                       bool
	AverageUpload, AverageWait, AverageResponse     string
	Stages                                          []TrendBarSegmentView
}

type TrendBarSegmentView struct {
	Class          string
	BarX, BarWidth int
	Y, Height      float64
	Square         bool
}

type HealthTrendView struct {
	Points                   []HealthTrendPointView
	SuccessPath, FailurePath string
	AxisTicks                []UsageTrendAxisTickView
	ShowSymbols              bool
	TimingError              string
}

type UsageTrendPointView struct {
	X         int
	LabelTick int64
	RequestY  int
	TokenY    int
	BarX      int
	BarWidth  int
	BarHeight int
	Date      string
	Requests  string
	Tokens    string
	ShowLabel bool
}

type UsageTrendView struct {
	ShowSymbols      bool
	Points           []UsageTrendPointView
	RequestPath      string
	RequestAreaPath  string
	AxisTicks        []UsageTrendAxisTickView
	MaxRequests      string
	MaxTokens        string
	GranularityLabel string
}

type UsageTrendAxisTickView struct {
	Y        int
	Requests string
	Tokens   string
}

type ModelUsageView struct {
	Model        string
	Requests     string
	Tokens       string
	Percent      int
	TokenPercent int
	RequestValue int64
	TokenValue   int64
}

type RequestView struct {
	At              string
	CaptureID       string
	Model           string
	Status          string
	StatusLabel     string
	InputTokens     string
	OutputTokens    string
	CacheTokens     string
	ReasoningTokens string
	ReasoningEffort string
	TotalTokens     string
	Latency         string
	TotalLatency    string
	LatencyDetail   string
	Error           string
	ErrorFull       string
}

type ModelView struct {
	Name            string
	Provider        string
	Description     string
	Available       bool
	UpdatedAt       string
	Tested          bool
	TestStatus      string
	TestStatusLabel string
	TestMessage     string
	TestLatency     string
	TestedAt        string
}

type QuotaPoolView struct {
	Providers         []QuotaPoolView
	Show              bool
	Available         bool
	Provider          string
	SlideID           string
	SlideLayout       string
	Accounts          string
	AvailabilityLabel string
	AvailabilityClass string
	NoUsableAccounts  bool
	UnknownCount      int
	Groups            []QuotaGroupView
	Columns           [][]QuotaGroupView
	CSRFToken         string
	ReturnTo          string
	RefreshLabel      string
	RefreshCompleted  bool
	RefreshDisabled   bool
	RefreshRunning    bool
}

type QuotaGroupView struct {
	Label            string
	RemainingPercent int
	StatusClass      string
	Accounts         string
	ResetAt          string
	ObservedAt       string
	Estimated        bool
}

type DashboardView struct {
	LayoutView
	Greeting      string
	StatusCard    StatusCardView
	Key           KeyView
	Usage         UsageSummaryView
	RecentUsage   []RequestView
	Models        []ModelView
	Quota         QuotaPoolView
	Notices       []NoticeView
	ShowClaimHint bool
}

type StatusView struct {
	LayoutView
	StatusCard  StatusCardView
	Timeline    []StatusEventView
	Notices     []NoticeView
	CanResubmit bool
}

type StatusEventView struct {
	Status  string
	Label   string
	At      string
	Note    string
	Current bool
}

type KeyPageView struct {
	LayoutView
	Key       KeyView
	User      UserView
	Confirmed bool
}

type UsageView struct {
	LayoutView
	FormAction       string
	UserID           string
	From             string
	To               string
	Range            string
	IsAdmin          bool
	Target           *UserView
	Summary          UsageSummaryView
	Daily            []UsagePointView
	Trend            UsageTrendView
	HealthTrend      HealthTrendView
	ByModel          []ModelUsageView
	ByModelRequests  []ModelUsageView
	ByModelTokens    []ModelUsageView
	ModelStatsNote   string
	ShowUserStats    bool
	ByUserRequests   []UserUsageView
	ByUserTokens     []UserUsageView
	HasUnlinked      bool
	UnlinkedRequests string
	UnlinkedTokens   string
	Requests         []RequestView
	HasMore          bool
	Models           []string
	SelectedModel    string
	Quota            QuotaPoolView
}

type UserUsageView struct {
	User           UserView
	UsageURL       string
	Requests       string
	Tokens         string
	RequestPercent int
	TokenPercent   int
	RequestValue   int64
	TokenValue     int64
}

type ModelsView struct {
	LayoutView
	Models             []ModelView
	CheckedAt          string
	Available          bool
	UnavailableMessage string
}

type ActivityView struct {
	LayoutView
	Requests []RequestView
}

type HealthView struct {
	LayoutView
	Checks []HealthCheckView
}

type ProfileView struct {
	LayoutView
	Profile      UserView
	CanEditPhone bool
	CanEditName  bool
	PhoneHelp    string
	NameHelp     string
	RulesVersion string
	Rules        []RuleView
}

type PasswordView struct {
	LayoutView
	CurrentPassword string
	NewPassword     string
	ConfirmPassword string
	MinimumLength   int
}

type CountView struct {
	Label string
	Value string
	Hint  string
	Tone  string
}

type HealthCheckView struct {
	StorageBytes     int64
	StorageSizeKnown bool
	Component        string
	Status           string
	StatusLabel      string
	Metric           string
	Message          string
	CheckedAt        string
	Latency          string
}

type UserUsageRankView struct {
	Rank     int
	User     UserView
	Requests string
	Tokens   string
	Percent  int
}

type AuditView struct {
	At         string
	Actor      string
	ActorName  string
	ActorPhone string
	Action     string
	Target     string
	Result     string
	IP         string
	RequestID  string
	Details    string
}

type AdminDashboardView struct {
	LayoutView
	Counts          []CountView
	Usage           UsageSummaryView
	Health          []HealthCheckView
	TopUsers        []UserUsageRankView
	RecentAudit     []AuditView
	UnlinkedSummary UsageSummaryView
}

type UserRowView struct {
	User      UserView
	Pending   bool
	LastUsed  string
	Usage     UsageSummaryView
	CanManage bool
}

type AdminUsersView struct {
	LayoutView
	Query     string
	Status    string
	Sort      string
	Statuses  []string
	Users     []UserRowView
	Page      int
	PageCount int
	Total     string
}

type AdminUserDetailView struct {
	LayoutView
	Target         UserView
	Key            KeyView
	StatusCard     StatusCardView
	Usage          UsageSummaryView
	Daily          []UsagePointView
	Trend          UsageTrendView
	ByModel        []ModelUsageView
	Requests       []RequestView
	CanApprove     bool
	CanReject      bool
	CanSuspend     bool
	CanUnsuspend   bool
	CanDelete      bool
	CanReset       bool
	CanChangePhone bool
	CanChangeRole  bool
	NextRole       string
	RoleAction     string
	RoleConfirm    string
	DeleteWarning  string
}

type ApprovalView struct {
	ApplicationID string
	User          UserView
	SubmittedAt   string
	RulesVersion  string
	Reason        string
	CanApprove    bool
	CanReject     bool
}

type AdminApprovalsView struct {
	LayoutView
	Pending      []ApprovalView
	Rejected     []ApprovalView
	PendingCount string
}

type UpstreamAccountView struct {
	ID          string
	Name        string
	Account     string
	Provider    string
	AuthIndex   string
	Disabled    bool
	StatusLabel string
	Quotas      []UpstreamAccountQuotaView
	QuotaGroups []UpstreamAccountQuotaGroupView
	QuotaStatus string
}

type UpstreamAccountQuotaGroupView struct {
	Label  string
	Quotas []UpstreamAccountQuotaView
}

type UpstreamAccountQuotaView struct {
	Label            string
	Plan             string
	RemainingPercent int
	StatusClass      string
	ResetAt          string
}

type OAuthChannelView struct {
	Value    string
	Label    string
	Selected bool
}

type AdminUpstreamsView struct {
	LayoutView
	Channel                       string
	ChannelLabel                  string
	Channels                      []OAuthChannelView
	ChannelPanels                 []AdminUpstreamsView
	SupportsQuota                 bool
	SupportsReasoning             bool
	SupportsIdentityCompatibility bool
	IdentityCompatibilityEnabled  bool
	IdentityCompatibilityReady    bool
	IdentityCompatibilityError    string
	Accounts                      []UpstreamAccountView
	Presets                       []OAuthPresetView
	PresetError                   string
	PresetCardOrder               string
	PresetLayoutError             string
	AliasCardOrder                string
	AliasLayoutError              string
	Models                        []OAuthModelView
	AliasModels                   []OAuthModelView
	ReasoningModels               []ReasoningCapModelView
	WildcardRules                 []string
	ModelError                    string
	AliasError                    string
	AliasRevision                 string
	AliasReady                    bool
	ReasoningError                string
	ReasoningRevision             string
	ReasoningReady                bool
	QuotaError                    string
	RefreshLabel                  string
	RefreshCompleted              bool
	RefreshDisabled               bool
	RefreshRunning                bool
	Enabled                       int
	Disabled                      int
}

type OAuthPresetView struct {
	ID             string
	Name           string
	Applied        bool
	Summary        string
	UpdatedAt      string
	EnabledModels  []string
	DisabledModels []string
	AliasMappings  []string
	OriginalModels []string
	ReasoningCaps  []string
}

type OAuthModelView struct {
	ID           string
	DisplayName  string
	AliasList    []string
	KeepOriginal bool
	Enabled      bool
	WildcardRule string
}

type ReasoningCapModelView struct {
	ID      string
	Options []ReasoningCapOptionView
}

type ReasoningCapOptionView struct {
	Value    string
	Label    string
	Selected bool
}

type PolicyView struct {
	Version     string
	Title       string
	Body        string
	PublishedAt string
	PublishedBy string
}

type PolicyVersionView struct {
	PolicyView
	Current bool
}

type AdminPolicyView struct {
	LayoutView
	Current          PolicyView
	Versions         []PolicyVersionView
	EditTitle        string
	EditBody         string
	Preview          bool
	RegistrationOpen bool
}

type AdminSystemView struct {
	LayoutView
	Backup       BackupView
	Storage      []HealthCheckView
	Checks       []HealthCheckView
	LastSyncAt   string
	NextSyncAt   string
	SyncInterval string
	HealthReady  bool
	Retrying     bool
	Messages     []NoticeView
	AuditError   string
	Entries      []AuditView
	Total        string
}

type BackupView struct {
	Available       bool
	Configured      bool
	KeyReady        bool
	Repository      string
	TokenReady      bool
	KeySaved        bool
	Automatic       bool
	Weekday         int
	Time            string
	Running         bool
	Pending         bool
	LastSuccess     string
	NextRun         string
	Message         string
	Warning         string
	Size            string
	RestoreMessage  string
	RestoreRollback string
}

type GlobalRequestView struct {
	RequestView
	User      UserView
	UserLabel string
	Linked    bool
}

type AdminRequestsView struct {
	LayoutView
	Requests []GlobalRequestView
	Shown    string
	Total    string
}

type ErrorView struct {
	LayoutView
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	CanGoBack  bool
}
