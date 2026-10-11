package durable

type StorageStats struct {
	HotBytes          int64 `json:"hot_bytes"`
	ColdBytes         int64 `json:"cold_bytes"`
	ColdOriginalBytes int64 `json:"cold_original_bytes"`
	ColdRecords       int   `json:"cold_records"`
}

type ServiceStats struct {
	Queues        map[string]QueueStats `json:"queues"`
	Limits        map[string]Limits     `json:"limits"`
	Storage       StorageStats          `json:"storage"`
	RetentionDays int                   `json:"retention_days"`
}

type Compaction struct {
	Scanned    int `json:"scanned"`
	Archived   int `json:"archived"`
	SavedBytes int `json:"saved_bytes"`
}
