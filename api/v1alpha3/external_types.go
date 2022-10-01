package v1alpha3

import "time"

// DrainSpec encapsulates `kubectl drain` parameters minus node/pod selectors.
type DrainSpec struct {
	Timeout                  *time.Duration `json:"timeout,omitempty"`
	GracePeriod              *int32         `json:"gracePeriod,omitempty"`
	DeleteLocalData          *bool          `json:"deleteLocalData,omitempty"`
	IgnoreDaemonSets         *bool          `json:"ignoreDaemonSets,omitempty"`
	Force                    bool           `json:"force,omitempty"`
	DisableEviction          bool           `json:"disableEviction,omitempty"`
	SkipWaitForDeleteTimeout int            `json:"skipWaitForDeleteTimeout,omitempty"`
}
