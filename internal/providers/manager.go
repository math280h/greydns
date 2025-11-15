package providers

import (
	"fmt"

	cfprovider "github.com/math280h/greydns/internal/providers/cf"
	"github.com/math280h/greydns/internal/types"
)

// Manager handles different DNS providers
type Manager struct {
	provider types.Provider
}

// NewManager creates a new provider manager
func NewManager(providerName string) (*Manager, error) {
	var provider types.Provider

	switch providerName {
	case "cloudflare":
		provider = &cfprovider.Provider{}
	default:
		return nil, fmt.Errorf("unsupported provider: %s", providerName)
	}

	return &Manager{
		provider: provider,
	}, nil
}

// GetProvider returns the current provider
func (m *Manager) GetProvider() types.Provider {
	return m.provider
}

// Connect initializes the provider with credentials
func (m *Manager) Connect(credentials map[string]string) error {
	return m.provider.Connect(credentials)
}

// GetZones returns available zones from the provider
func (m *Manager) GetZones() (map[string]string, error) {
	return m.provider.GetZones()
}

// GetZone gets a specific zone by ID
func (m *Manager) GetZone(zoneID string) (*types.Zone, error) {
	return m.provider.GetZone(zoneID)
}

// CheckZoneExists checks if a zone exists
func (m *Manager) CheckZoneExists(zoneName string, zones map[string]string) (*types.Zone, error) {
	return m.provider.CheckZoneExists(zoneName, zones)
}

// CreateRecord creates a DNS record
func (m *Manager) CreateRecord(params types.CreateRecordParams) (*types.DNSRecord, error) {
	return m.provider.CreateRecord(params)
}

// UpdateRecord updates a DNS record
func (m *Manager) UpdateRecord(params types.UpdateRecordParams) (*types.DNSRecord, error) {
	return m.provider.UpdateRecord(params)
}

// DeleteRecord deletes a DNS record
func (m *Manager) DeleteRecord(recordID, zoneID string) error {
	return m.provider.DeleteRecord(recordID, zoneID)
}

// RefreshRecordsCache refreshes the cache of DNS records
func (m *Manager) RefreshRecordsCache(zones map[string]string) (map[string]*types.DNSRecord, error) {
	return m.provider.RefreshRecordsCache(zones)
}

// CleanupRecords performs provider-specific cleanup of old records
func (m *Manager) CleanupRecords(existingRecords map[string]*types.DNSRecord, namespace, serviceName, zoneID, currentDomain string) error {
	return m.provider.CleanupRecords(existingRecords, namespace, serviceName, zoneID, currentDomain)
}

// Name returns the provider name
func (m *Manager) Name() string {
	return m.provider.Name()
}
