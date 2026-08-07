package cloud

import (
	"encoding/json"
	"time"
)

// Asset is the provider-neutral ingestion unit. One CAI asset -> one Asset.
type Asset struct {
	Name      string
	AssetType string
	Service   string
	Project   string
	Location  string
	Labels    map[string]string
	UpdatedAt time.Time
	Data      json.RawMessage
}
