package usage

import "time"

type Bucket struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Group         string  `json:"group"`
	UsedPercent   float64 `json:"usedPercent"`
	WindowMinutes int     `json:"windowMinutes,omitempty"`
	ResetsAt      int64   `json:"resetsAt,omitempty"`
	Severity      string  `json:"severity,omitempty"`
	IsActive      bool    `json:"isActive,omitempty"`
	Primary       bool    `json:"primary,omitempty"`
}

type ResetCredit struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Status    string `json:"status,omitempty"`
}

type ProviderSnapshot struct {
	Provider           string        `json:"provider"`
	PlanType           string        `json:"planType,omitempty"`
	ObservedAt         time.Time     `json:"observedAt,omitempty"`
	Buckets            []Bucket      `json:"buckets"`
	Error              string        `json:"error,omitempty"`
	ErrorDetail        string        `json:"errorDetail,omitempty"`
	Notice             string        `json:"notice,omitempty"`
	ResetCredits       int           `json:"resetCredits,omitempty"`
	ResetCreditDetails []ResetCredit `json:"resetCreditDetails,omitempty"`
}

func (snapshot *ProviderSnapshot) Fail(message string, cause error) *ProviderSnapshot {
	snapshot.Error = message
	if cause != nil {
		snapshot.ErrorDetail = cause.Error()
	}
	return snapshot
}

func (snapshot *ProviderSnapshot) Wait(message string) *ProviderSnapshot {
	snapshot.Notice = message
	return snapshot
}

type Attempt struct {
	At         time.Time `json:"at"`
	Provider   string    `json:"provider"`
	OK         bool      `json:"ok"`
	DurationMS int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Note       string    `json:"note,omitempty"`
	Buckets    int       `json:"buckets"`
}

type SeriesPoint struct {
	At      time.Time `json:"at"`
	Percent float64   `json:"percent"`
}

type Series struct {
	Provider string        `json:"provider"`
	Key      string        `json:"key"`
	Label    string        `json:"label"`
	Group    string        `json:"group"`
	Points   []SeriesPoint `json:"points"`
}
