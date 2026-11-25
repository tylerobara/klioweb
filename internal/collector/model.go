package collector

import "time"

// Health is the overall verdict for a cluster's backups.
type Health string

const (
	HealthOK       Health = "ok"
	HealthWarning  Health = "warning"
	HealthCritical Health = "critical"
	HealthNone     Health = "none" // klio not enabled
)

// Snapshot is the full payload served to the dashboard.
type Snapshot struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	Summary     Summary         `json:"summary"`
	Clusters    []ClusterStatus `json:"clusters"`
	Servers     []ServerStatus  `json:"servers"`
}

type Summary struct {
	TotalClusters  int `json:"totalClusters"`
	KlioEnabled    int `json:"klioEnabled"`
	KlioDisabled   int `json:"klioDisabled"`
	Healthy        int `json:"healthy"`
	Warning        int `json:"warning"`
	Critical       int `json:"critical"`
	Servers        int `json:"servers"`
	ServersNotRead int `json:"serversNotReady"`
}

type ClusterStatus struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Phase          string `json:"phase"`
	Instances      int64  `json:"instances"`
	ReadyInstances int64  `json:"readyInstances"`
	Primary        string `json:"primary,omitempty"`
	ImageName      string `json:"imageName,omitempty"`

	KlioEnabled bool  `json:"klioEnabled"`
	Klio        *Klio `json:"klio,omitempty"`
}

type Klio struct {
	Health        Health   `json:"health"`
	Reasons       []string `json:"reasons"`
	IsWALArchiver bool     `json:"isWALArchiver"`

	PluginConfig  string `json:"pluginConfig,omitempty"`
	ConfigApplied *bool  `json:"configApplied,omitempty"`
	ConfigMessage string `json:"configMessage,omitempty"`
	ServerAddress string `json:"serverAddress,omitempty"`
	ServerName    string `json:"serverName,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Tier2Backup   bool   `json:"tier2Backup"`
	Tier2Recovery bool   `json:"tier2Recovery"`

	Schedule     string     `json:"schedule,omitempty"`
	ScheduleName string     `json:"scheduleName,omitempty"`
	IntervalSecs int64      `json:"intervalSeconds,omitempty"`
	LastSchedule *time.Time `json:"lastSchedule,omitempty"`
	NextSchedule *time.Time `json:"nextSchedule,omitempty"`
	Suspended    bool       `json:"suspended"`

	Total     int `json:"totalBackups"`
	Completed int `json:"completedBackups"`
	Failed    int `json:"failedBackups"`
	Running   int `json:"runningBackups"`

	LastBackup          *Backup `json:"lastBackup,omitempty"`
	LastSuccess         *Backup `json:"lastSuccess,omitempty"`
	LastFailure         *Backup `json:"lastFailure,omitempty"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`

	// Recent holds the newest backups, newest first.
	Recent []Backup `json:"recent"`

	serverNS string
	// History holds oldest→newest phase for a compact strip.
	History []string `json:"history"`
}

type Backup struct {
	Name       string     `json:"name"`
	Phase      string     `json:"phase"`
	BackupID   string     `json:"backupId,omitempty"`
	Online     *bool      `json:"online,omitempty"`
	Pod        string     `json:"pod,omitempty"`
	Created    time.Time  `json:"created"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	StoppedAt  *time.Time `json:"stoppedAt,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`
	BeginWal   string     `json:"beginWal,omitempty"`
	EndWal     string     `json:"endWal,omitempty"`
	Error      string     `json:"error,omitempty"`
	Tier1      string     `json:"tier1,omitempty"`
}

type ServerStatus struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	Mode      string   `json:"mode,omitempty"`
	Storage   string   `json:"storage,omitempty"`
	Tier2     string   `json:"tier2,omitempty"`
	Ready     bool     `json:"ready"`
	PodsReady int      `json:"podsReady"`
	Pods      int      `json:"pods"`
	Clusters  []string `json:"clusters"`
}
