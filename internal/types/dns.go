package types

// DNSRecord represents a generic DNS record independent of any provider
type DNSRecord struct {
	ID      string
	Name    string
	Type    string
	Content string
	TTL     int
	Comment string
	Proxied *bool // Optional, not all providers support this
	ZoneID  string
}

// Zone represents a DNS zone
type Zone struct {
	ID   string
	Name string
}

// RecordType represents the supported DNS record types
type RecordType string

const (
	RecordTypeA     RecordType = "A"
	RecordTypeCNAME RecordType = "CNAME"
	RecordTypeAAAA  RecordType = "AAAA"
	RecordTypeTXT   RecordType = "TXT"
	RecordTypeMX    RecordType = "MX"
)

// CreateRecordParams holds the parameters for creating a DNS record
type CreateRecordParams struct {
	Name    string
	Type    RecordType
	Content string
	TTL     int
	Comment string
	Proxied *bool
	ZoneID  string
}

// UpdateRecordParams holds the parameters for updating a DNS record
type UpdateRecordParams struct {
	RecordID string
	Name     string
	Type     RecordType
	Content  string
	TTL      int
	Comment  string
	Proxied  *bool
	ZoneID   string
}

// Provider represents the interface that all DNS providers must implement
type Provider interface {
	// Initialize the provider with credentials
	Connect(credentials map[string]string) error

	// Zone operations
	GetZones() (map[string]string, error) // Returns map[zoneName]zoneID
	GetZone(zoneID string) (*Zone, error)
	CheckZoneExists(zoneName string, zones map[string]string) (*Zone, error)

	// Record operations
	CreateRecord(params CreateRecordParams) (*DNSRecord, error)
	UpdateRecord(params UpdateRecordParams) (*DNSRecord, error)
	DeleteRecord(recordID, zoneID string) error
	GetRecords(zoneID string) (map[string]*DNSRecord, error) // Returns map[recordName]*DNSRecord
	RefreshRecordsCache(zones map[string]string) (map[string]*DNSRecord, error)

	// Provider-specific cleanup
	CleanupRecords(existingRecords map[string]*DNSRecord, namespace, serviceName, zoneID, currentDomain string) error

	// Provider information
	Name() string
}

// ProviderError represents errors from DNS providers
type ProviderError struct {
	Provider string
	Message  string
	Err      error
}

func (e *ProviderError) Error() string {
	if e.Err != nil {
		return e.Provider + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Provider + ": " + e.Message
}

// NewProviderError creates a new provider error
func NewProviderError(provider, message string, err error) *ProviderError {
	return &ProviderError{
		Provider: provider,
		Message:  message,
		Err:      err,
	}
}
