// Package restore verifies Klio backups by restoring them into a short-lived
// CloudNativePG cluster, checking the data, and tearing everything down.
package restore

import (
	"errors"
	"time"
)

const (
	LabelTest = "klioweb.obara.dev/restore-test" // marks every object this package creates
	LabelRun  = "klioweb.obara.dev/run"
	LabelRuns = "klioweb.obara.dev/restore-run" // marks result ConfigMaps
)

const (
	StatusPending   = "pending"
	StatusRestoring = "restoring"
	StatusChecking  = "checking"
	StatusCleaning  = "cleaning-up"
	StatusPassed    = "passed"
	StatusFailed    = "failed"
)

var (
	ErrInvalid   = errors.New("invalid request")
	ErrForbidden = errors.New("namespace not enabled for restore tests")
	ErrBusy      = errors.New("too many restore tests running")
	ErrNotFound  = errors.New("not found")
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type Run struct {
	ID           string     `json:"id"`
	Namespace    string     `json:"namespace"`
	Cluster      string     `json:"cluster"`
	Backup       string     `json:"backup"`
	BackupID     string     `json:"backupId"`
	TestCluster  string     `json:"testCluster"`
	TriggeredBy  string     `json:"triggeredBy"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	DurationMs   int64      `json:"durationMs,omitempty"`
	Error        string     `json:"error,omitempty"`
	Checks       []Check    `json:"checks"`
	Diagnostics  []string   `json:"diagnostics,omitempty"`
	CleanupError string     `json:"cleanupError,omitempty"`
}

func (r Run) Terminal() bool { return r.Status == StatusPassed || r.Status == StatusFailed }
