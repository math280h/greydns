package providers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/math280h/greydns/internal/types"
)

func TestNewManager(t *testing.T) {
	tests := []struct {
		name         string
		providerName string
		expectError  bool
		errorMessage string
	}{
		{
			name:         "valid cloudflare provider",
			providerName: "cloudflare",
			expectError:  false,
		},
		{
			name:         "unsupported provider",
			providerName: "unsupported",
			expectError:  true,
			errorMessage: "unsupported provider: unsupported",
		},
		{
			name:         "empty provider name",
			providerName: "",
			expectError:  true,
			errorMessage: "unsupported provider: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, err := NewManager(tt.providerName)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, manager)
				assert.Contains(t, err.Error(), tt.errorMessage)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, manager)
				assert.NotNil(t, manager.GetProvider())
			}
		})
	}
}

func TestManager_GetProvider(t *testing.T) {
	manager, err := NewManager("cloudflare")
	require.NoError(t, err)
	require.NotNil(t, manager)

	provider := manager.GetProvider()
	assert.NotNil(t, provider)
	assert.Equal(t, "cloudflare", provider.Name())
}

// Mock provider for testing
type mockProvider struct {
	connectCalled            bool
	connectCredentials       map[string]string
	connectError             error
	zones                    map[string]string
	getZonesError            error
	records                  map[string]*types.DNSRecord
	refreshRecordsCacheError error
}

func (m *mockProvider) Name() string {
	return "mock"
}

func (m *mockProvider) Connect(credentials map[string]string) error {
	m.connectCalled = true
	m.connectCredentials = credentials
	return m.connectError
}

func (m *mockProvider) GetZones() (map[string]string, error) {
	return m.zones, m.getZonesError
}

func (m *mockProvider) GetZone(zoneID string) (*types.Zone, error) {
	for name, id := range m.zones {
		if id == zoneID {
			return &types.Zone{ID: id, Name: name}, nil
		}
	}
	return nil, types.NewProviderError("mock", "zone not found", nil)
}

func (m *mockProvider) CheckZoneExists(zoneName string, zones map[string]string) (*types.Zone, error) {
	if zoneID, exists := zones[zoneName]; exists {
		return &types.Zone{ID: zoneID, Name: zoneName}, nil
	}
	return nil, types.NewProviderError("mock", "zone not found", nil)
}

func (m *mockProvider) CreateRecord(params types.CreateRecordParams) (*types.DNSRecord, error) {
	record := &types.DNSRecord{
		ID:      "mock-" + params.Name,
		Name:    params.Name,
		Type:    string(params.Type),
		Content: params.Content,
		TTL:     params.TTL,
		Comment: params.Comment,
		ZoneID:  params.ZoneID,
	}
	return record, nil
}

func (m *mockProvider) UpdateRecord(params types.UpdateRecordParams) (*types.DNSRecord, error) {
	record := &types.DNSRecord{
		ID:      params.RecordID,
		Name:    params.Name,
		Type:    string(params.Type),
		Content: params.Content,
		TTL:     params.TTL,
		Comment: params.Comment,
		ZoneID:  params.ZoneID,
	}
	return record, nil
}

func (m *mockProvider) DeleteRecord(recordID, zoneID string) error {
	return nil
}

func (m *mockProvider) GetRecords(zoneID string) (map[string]*types.DNSRecord, error) {
	return m.records, nil
}

func (m *mockProvider) RefreshRecordsCache(zones map[string]string) (map[string]*types.DNSRecord, error) {
	return m.records, m.refreshRecordsCacheError
}

func (m *mockProvider) CleanupRecords(existingRecords map[string]*types.DNSRecord, namespace, serviceName, zoneID, currentDomain string) error {
	return nil
}

func TestManager_WithMockProvider(t *testing.T) {
	// Create mock provider to test the structure
	mock := &mockProvider{
		zones: map[string]string{
			"example.com": "zone123",
		},
		records: map[string]*types.DNSRecord{
			"test.example.com": {
				ID:      "record123",
				Name:    "test.example.com",
				Type:    "A",
				Content: "192.0.2.1",
				TTL:     300,
				ZoneID:  "zone123",
			},
		},
	}

	// Test mock provider methods work
	assert.Equal(t, "mock", mock.Name())

	err := mock.Connect(map[string]string{"test": "token"})
	assert.NoError(t, err)
	assert.True(t, mock.connectCalled)

	zones, err := mock.GetZones()
	assert.NoError(t, err)
	assert.Equal(t, "zone123", zones["example.com"])

	zone, err := mock.CheckZoneExists("example.com", zones)
	assert.NoError(t, err)
	assert.Equal(t, "example.com", zone.Name)

	// Test the actual cloudflare provider integration
	manager, err := NewManager("cloudflare")
	require.NoError(t, err)

	// Test that we can get the provider without errors
	provider := manager.GetProvider()
	assert.NotNil(t, provider)
	assert.Equal(t, "cloudflare", provider.Name())
}

func TestManager_Connect(t *testing.T) {
	manager, err := NewManager("cloudflare")
	require.NoError(t, err)

	credentials := map[string]string{
		"cloudflare": "test-token",
	}

	// Connect should succeed with any token string (validation happens later)
	err = manager.Connect(credentials)
	assert.NoError(t, err) // Connection setup should succeed
}

func TestManager_ProviderMethods(t *testing.T) {
	manager, err := NewManager("cloudflare")
	require.NoError(t, err)

	// Test that all manager methods delegate to the provider properly
	assert.Equal(t, "cloudflare", manager.Name())

	// Test that methods exist but don't call them without proper connection
	// (they would panic or fail without a valid Cloudflare client)

	// Just test the structure exists
	provider := manager.GetProvider()
	assert.NotNil(t, provider)
}
