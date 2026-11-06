package domain

// Only sanitized request identities and results are retained, independently of
// the bounded timing/detail log. No content, raw API key or durations are copied.
type RequestOutcomeEvent struct {
	Hash   string `json:"hash"`
	AtMS   int64  `json:"at_ms"`
	Failed bool   `json:"failed"`
}

type GatewayFailure struct {
	ID, UserID, APIKeyHash, RequestID, Reason string
	StartedMS, EndedMS                        int64
	Status                                    int
	CheckedMS, GeneratedMS                    int64
	Upstream                                  []RequestOutcomeEvent
}

type RequestRejection struct {
	UserID      string
	AtMS, Calls int64
}
type RequestUserDelta struct {
	UserID string
	Calls  int64
}
type RequestLogData struct {
	Failures           []GatewayFailure
	Rejections         []RequestRejection
	RequiredUpstreamMS int64
	Note               string
	// Prepared corrections come from the usage checkpoint, not a new recount.
	Corrections   []RequestCorrection
	ReconcileAtMS int64
}

type RequestCorrection struct {
	AtMS                    int64
	UserID, APIKeyHash      string
	Calls, Success, Failure int64
}
