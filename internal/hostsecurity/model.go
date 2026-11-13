package hostsecurity

import "time"

const MaxPackages = 100000

type Host struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Codename       string `json:"codename,omitempty"`
	Kernel         string `json:"kernel"`
	Architecture   string `json:"architecture"`
	Manager        string `json:"manager"`
	ManagerVersion string `json:"manager_version"`
	Hostname       string `json:"hostname"`
	MachineHash    string `json:"machine_hash,omitempty"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
}

type Package struct {
	Key           string    `json:"key"`
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	Release       string    `json:"release,omitempty"`
	Architecture  string    `json:"architecture"`
	Source        string    `json:"source"`
	SourceVersion string    `json:"source_version"`
	Repository    string    `json:"repository"`
	Origin        string    `json:"origin"`
	Manager       string    `json:"manager"`
	Status        string    `json:"status"`
	Held          string    `json:"held"`
	Provides      []string  `json:"provides,omitempty"`
	Depends       []string  `json:"depends,omitempty"`
	InventoryAt   time.Time `json:"inventory_at"`
}

type Scan struct {
	ID             string    `json:"id"`
	ServerID       string    `json:"server_id"`
	Domain         string    `json:"domain"`
	Status         string    `json:"status"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	DurationMS     int64     `json:"duration_ms"`
	ScannerVersion string    `json:"scanner_version"`
	MethodVersion  string    `json:"method_version"`
	SnapshotID     string    `json:"snapshot_id"`
	Host           Host      `json:"host"`
	PackageCount   int       `json:"package_count"`
	Coverage       string    `json:"coverage"`
	Warnings       []string  `json:"warnings"`
}

type Page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}
