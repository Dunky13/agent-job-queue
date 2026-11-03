// Package wire defines the JSON that `agent-heavy status --json` and the
// dashboard's /api/status publish. The queue daemon's private state is a
// different shape and does not belong here.
//
// Fields are declared in JSON key order so that the output keeps the sorted keys
// consumers have always seen. Metrics the resource controller measures are
// json.Number so their literals pass through unchanged.
package wire

import "encoding/json"

// Snapshot is one complete status reading. Lanes and Plugins are omitted only
// when nil: an empty list is a real answer from a current daemon.
type Snapshot struct {
	Jobs      []Job      `json:"jobs"`
	Lanes     []Lane     `json:"lanes,omitzero"`
	Plugins   []Plugin   `json:"plugins,omitzero"`
	Queue     Queue      `json:"queue"`
	Scheduler *Scheduler `json:"scheduler,omitempty"`
	UpdatedAt string     `json:"updatedAt"`
}

// Queue summarizes the default queue. Limit is null when the daemon publishes no
// default group. DrainUntil is set only while a drain closes intake, as an RFC
// 3339 UTC time; DrainError only when the drain file cannot be trusted, in which
// case intake is closed as well.
type Queue struct {
	DrainError     string `json:"drainError,omitempty"`
	DrainUntil     string `json:"drainUntil,omitempty"`
	LegacyOccupied bool   `json:"legacyOccupied"`
	Limit          *int   `json:"limit"`
	Running        int    `json:"running"`
	Status         string `json:"status"`
	Waiting        int    `json:"waiting"`
}

// Job is a queued, running or finished command or lease. Lease is set only for
// leases; a command has none of its keys, while a lease has all of them.
type Job struct {
	Branch        string     `json:"branch"`
	CanCancel     bool       `json:"canCancel"`
	CanReorder    bool       `json:"canReorder"`
	CanStart      bool       `json:"canStart"`
	Directory     string     `json:"directory"`
	Executable    string     `json:"executable"`
	ExitCode      *int       `json:"exitCode"`
	FinishedAt    *string    `json:"finishedAt"`
	ID            int        `json:"id"`
	Kind          string     `json:"kind,omitempty"`
	Lane          string     `json:"lane,omitempty"`
	LogPath       string     `json:"logPath"`
	Phase         string     `json:"phase"`
	Priority      int        `json:"priority"`
	Project       string     `json:"project"`
	QueuePosition *int       `json:"queuePosition"`
	Resources     *Resources `json:"resources,omitempty"`
	StartedAt     *string    `json:"startedAt"`
	State         string     `json:"state"`
	SubmittedAt   *string    `json:"submittedAt"`
	Title         string     `json:"title"`
	*Lease
}

// Lease holds the keys only a lease has. Unset values are explicit nulls.
type Lease struct {
	ExpiresAt  *string           `json:"expiresAt"`
	GrantedAt  *string           `json:"grantedAt"`
	Label      string            `json:"label"`
	Meta       map[string]string `json:"meta"`
	OwnerAlive *bool             `json:"ownerAlive"`
	OwnerPid   *int              `json:"ownerPid"`
	Position   *int              `json:"position"`
	QueuedAt   *string           `json:"queuedAt"`
	Reason     *string           `json:"reason"`
	RenewedAt  *string           `json:"renewedAt"`
}

// MarshalJSON merges the embedded lease keys back into sorted order.
func (j Job) MarshalJSON() ([]byte, error) {
	type plain Job
	data, err := json.Marshal(plain(j))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

// Resources is the resource controller's estimate and decision for one job.
type Resources struct {
	Decision   *Decision    `json:"decision"`
	Learning   *Learning    `json:"learning,omitempty"`
	Observed   *Observation `json:"observed"`
	Prediction Prediction   `json:"prediction"`
}

// Decision is an admission decision.
type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

// Learning names the learned workload behind a prediction.
type Learning struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// Observation is what the controller has measured of a running job.
type Observation struct {
	Complete        bool        `json:"complete"`
	CPUCores        json.Number `json:"cpuCores"`
	CPUSeconds      json.Number `json:"cpuSeconds"`
	DurationSeconds json.Number `json:"durationSeconds"`
	MemoryBytes     json.Number `json:"memoryBytes"`
	PeakMemoryBytes json.Number `json:"peakMemoryBytes"`
	Samples         json.Number `json:"samples"`
}

// Prediction estimates a job's resources. FamilySamples is absent from
// predictions written before it existed; the dashboard ignores it.
type Prediction struct {
	Confidence      string       `json:"confidence"`
	CPUCores        json.Number  `json:"cpuCores"`
	DurationSeconds *json.Number `json:"durationSeconds"`
	FamilySamples   json.Number  `json:"familySamples,omitempty"`
	MemoryBytes     json.Number  `json:"memoryBytes"`
	Samples         json.Number  `json:"samples"`
	Source          string       `json:"source"`
}

// Scheduler is the resource controller's published status.
type Scheduler struct {
	Available bool    `json:"available"`
	Host      *Host   `json:"host"`
	Mode      string  `json:"mode"`
	Reason    string  `json:"reason"`
	UpdatedAt *string `json:"updatedAt"`
}

// Host is one sample of the machine's resources.
type Host struct {
	AvailableMemoryBytes  json.Number `json:"availableMemoryBytes"`
	Complete              *bool       `json:"complete,omitempty"`
	CPUBusyCores          json.Number `json:"cpuBusyCores"`
	CPUCores              json.Number `json:"cpuCores"`
	DiskFreeBytes         json.Number `json:"diskFreeBytes"`
	Machine               string      `json:"machine"`
	MemoryPressure        string      `json:"memoryPressure"`
	SampledAt             json.Number `json:"sampledAt"`
	SwapInBytesPerSecond  json.Number `json:"swapInBytesPerSecond"`
	SwapOutBytesPerSecond json.Number `json:"swapOutBytesPerSecond"`
	SwapUsedBytes         json.Number `json:"swapUsedBytes"`
	TotalMemoryBytes      json.Number `json:"totalMemoryBytes"`
}

// Lane is one lane's definition and current use.
type Lane struct {
	Concurrency *int    `json:"concurrency"`
	Held        int     `json:"held"`
	Label       string  `json:"label"`
	Name        string  `json:"name"`
	Plugin      *string `json:"plugin"`
	Priority    int     `json:"priority"`
	Removed     bool    `json:"removed,omitempty"`
	Running     int     `json:"running"`
	SharesLimit bool    `json:"sharesLimit"`
	Waiting     int     `json:"waiting"`
}

// Plugin is one plugin's load result.
type Plugin struct {
	Error   *string  `json:"error"`
	Lanes   []string `json:"lanes"`
	Loaded  bool     `json:"loaded"`
	Name    string   `json:"name"`
	Warning *string  `json:"warning"`
}
