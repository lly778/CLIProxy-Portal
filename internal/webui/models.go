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
	Latency         string
	WindowLabel     string
}

type UsagePointView struct {
	Date         string
	Requests     string
	Tokens       string
	Percent      int
	TokenPercent int
	RequestValue int64
	TokenValue   int64
}

type UsageTrendPointView struct {
	X         int
	RequestY  int
	TokenY    int
	Date      string
	Requests  string
	Tokens    string
	ShowLabel bool
}

type UsageTrendView struct {
	Points           []UsageTrendPointView
	RequestPoints    string
	TokenPoints      string
	MaxRequests      string
	MaxTokens        string
	GranularityLabel string
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
	Error           string
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
	Show              bool
	Available         bool
	Provider          string
	Accounts          string
	AvailabilityLabel string
	AvailabilityClass string
	NoUsableAccounts  bool
	UnknownCount      int
	Groups            []QuotaGroupView
	CSRFToken         string
	ReturnTo          string
	RefreshLabel      string
	RefreshMessage    string
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
	ByModel          []ModelUsageView
	ByModelRequests  []ModelUsageView
	ByModelTokens    []ModelUsageView
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
	Component   string
	Status      string
	StatusLabel string
	Message     string
	CheckedAt   string
	Latency     string
}

type UserUsageRankView struct {
	Rank     int
	User     UserView
	Requests string
	Tokens   string
	Percent  int
}

type AuditView struct {
	At        string
	Actor     string
	Action    string
	Target    string
	Result    string
	IP        string
	RequestID string
	Details   string
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
	ByModel        []ModelUsageView
	Requests       []RequestView
	CanApprove     bool
	CanReject      bool
	CanSuspend     bool
	CanUnsuspend   bool
	CanDelete      bool
	CanReset       bool
	CanChangePhone bool
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

type AdminRowView struct {
	User        UserView
	LastLoginAt string
	CreatedAt   string
	CanDisable  bool
	IsLastAdmin bool
}

type AdminAdminsView struct {
	LayoutView
	Admins      []AdminRowView
	CanCreate   bool
	CanManage   bool
	NewPhone    string
	NewName     string
	NewPassword string
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

type AdminAuditView struct {
	LayoutView
	From      string
	To        string
	Actor     string
	Action    string
	Query     string
	Actions   []string
	Entries   []AuditView
	Page      int
	PageCount int
	Total     string
}

type AdminHealthView struct {
	LayoutView
	Checks       []HealthCheckView
	LastSyncAt   string
	NextSyncAt   string
	SyncInterval string
	Retrying     bool
	Messages     []NoticeView
}

type ErrorView struct {
	LayoutView
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	CanGoBack  bool
}
