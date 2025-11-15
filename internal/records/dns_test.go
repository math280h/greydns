package records

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	cfg "github.com/math280h/greydns/internal/config"
	"github.com/math280h/greydns/internal/types"
	"github.com/math280h/greydns/internal/utils"
)

// Mock provider for testing records
type mockProvider struct {
	zones                    map[string]string
	records                  map[string]*types.DNSRecord
	createRecordCalls        []types.CreateRecordParams
	updateRecordCalls        []types.UpdateRecordParams
	deleteRecordCalls        []deleteRecordCall
	cleanupRecordCalls       []cleanupRecordCall
	shouldFailZoneCheck      bool
	shouldFailRecordCreation bool
	shouldFailRecordUpdate   bool
	shouldFailRecordDeletion bool
}

type deleteRecordCall struct {
	recordID string
	zoneID   string
}

type cleanupRecordCall struct {
	namespace     string
	serviceName   string
	zoneID        string
	currentDomain string
}

func (m *mockProvider) Name() string {
	return "mock"
}

func (m *mockProvider) Connect(credentials map[string]string) error {
	return nil
}

func (m *mockProvider) GetZones() (map[string]string, error) {
	return m.zones, nil
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
	if m.shouldFailZoneCheck {
		return nil, types.NewProviderError("mock", "zone not found", nil)
	}
	if zoneID, exists := zones[zoneName]; exists {
		return &types.Zone{ID: zoneID, Name: zoneName}, nil
	}
	return nil, types.NewProviderError("mock", "zone not found", nil)
}

func (m *mockProvider) CreateRecord(params types.CreateRecordParams) (*types.DNSRecord, error) {
	if m.shouldFailRecordCreation {
		return nil, types.NewProviderError("mock", "failed to create record", nil)
	}

	m.createRecordCalls = append(m.createRecordCalls, params)

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
	if m.shouldFailRecordUpdate {
		return nil, types.NewProviderError("mock", "failed to update record", nil)
	}

	m.updateRecordCalls = append(m.updateRecordCalls, params)

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
	if m.shouldFailRecordDeletion {
		return types.NewProviderError("mock", "failed to delete record", nil)
	}

	m.deleteRecordCalls = append(m.deleteRecordCalls, deleteRecordCall{
		recordID: recordID,
		zoneID:   zoneID,
	})
	return nil
}

func (m *mockProvider) GetRecords(zoneID string) (map[string]*types.DNSRecord, error) {
	return m.records, nil
}

func (m *mockProvider) RefreshRecordsCache(zones map[string]string) (map[string]*types.DNSRecord, error) {
	return m.records, nil
}

func (m *mockProvider) CleanupRecords(existingRecords map[string]*types.DNSRecord, namespace, serviceName, zoneID, currentDomain string) error {
	m.cleanupRecordCalls = append(m.cleanupRecordCalls, cleanupRecordCall{
		namespace:     namespace,
		serviceName:   serviceName,
		zoneID:        zoneID,
		currentDomain: currentDomain,
	})
	return nil
}

func setupTestConfig() {
	cfg.ConfigMap = &v1.ConfigMap{
		Data: map[string]string{
			"record-ttl":  "300",
			"record-type": "A",
		},
	}

	// Setup mock event recorder
	utils.Recorder = record.NewFakeRecorder(100)
}

func createTestService(name, namespace, domain, zone string, enableDNS bool) *v1.Service {
	annotations := map[string]string{
		"greydns.io/dns":    "false",
		"greydns.io/domain": domain,
		"greydns.io/zone":   zone,
	}

	if enableDNS {
		annotations["greydns.io/dns"] = "true"
	}

	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

func TestMain(m *testing.M) {
	setupTestConfig()
	code := m.Run()
	os.Exit(code)
}

func TestHandleAnnotations_DNSDisabled(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", false)

	HandleAnnotations(provider, existingRecords, "192.168.1.1", zonesToNames, service)

	// Should not create any records when DNS is disabled
	assert.Empty(t, provider.createRecordCalls)
	assert.Empty(t, existingRecords)
}

func TestHandleAnnotations_ZoneNotExists(t *testing.T) {
	provider := &mockProvider{
		zones:               map[string]string{"example.com": "zone123"},
		shouldFailZoneCheck: true,
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleAnnotations(provider, existingRecords, "192.168.1.1", zonesToNames, service)

	// Should not create any records when zone doesn't exist
	assert.Empty(t, provider.createRecordCalls)
	assert.Empty(t, existingRecords)
}

func TestHandleAnnotations_CreateNewRecord(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleAnnotations(provider, existingRecords, "192.168.1.1", zonesToNames, service)

	// Should create a new record
	require.Len(t, provider.createRecordCalls, 1)
	createCall := provider.createRecordCalls[0]
	assert.Equal(t, "api.example.com", createCall.Name)
	assert.Equal(t, types.RecordTypeA, createCall.Type)
	assert.Equal(t, "192.168.1.1", createCall.Content)
	assert.Equal(t, 300, createCall.TTL)
	assert.Equal(t, "[greydns - Do not manually edit]default/test-service", createCall.Comment)
	assert.Equal(t, "zone123", createCall.ZoneID)

	// Should add record to cache
	assert.Len(t, existingRecords, 1)
	assert.Contains(t, existingRecords, "api.example.com")
}

func TestHandleAnnotations_RecordExists_SameOwner(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	// Pre-populate with existing record owned by same service
	existingRecords := map[string]*types.DNSRecord{
		"api.example.com": {
			ID:      "existing-123",
			Name:    "api.example.com",
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: "[greydns - Do not manually edit]default/test-service",
			ZoneID:  "zone123",
		},
	}

	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleAnnotations(provider, existingRecords, "192.168.1.1", zonesToNames, service)

	// Should not create a new record
	assert.Empty(t, provider.createRecordCalls)

	// Should call cleanup
	require.Len(t, provider.cleanupRecordCalls, 1)
	cleanupCall := provider.cleanupRecordCalls[0]
	assert.Equal(t, "default", cleanupCall.namespace)
	assert.Equal(t, "test-service", cleanupCall.serviceName)
	assert.Equal(t, "zone123", cleanupCall.zoneID)
	assert.Equal(t, "api.example.com", cleanupCall.currentDomain)
}

func TestHandleAnnotations_RecordExists_DifferentOwner(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	// Pre-populate with existing record owned by different service
	existingRecords := map[string]*types.DNSRecord{
		"api.example.com": {
			ID:      "existing-123",
			Name:    "api.example.com",
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: "[greydns - Do not manually edit]default/other-service",
			ZoneID:  "zone123",
		},
	}

	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleAnnotations(provider, existingRecords, "192.168.1.1", zonesToNames, service)

	// Should not create a new record
	assert.Empty(t, provider.createRecordCalls)

	// Should not call cleanup (different owner)
	assert.Empty(t, provider.cleanupRecordCalls)
}

func TestHandleDeletions_RecordExists_CorrectOwner(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	// Pre-populate with existing record owned by the service
	existingRecords := map[string]*types.DNSRecord{
		"api.example.com": {
			ID:      "existing-123",
			Name:    "api.example.com",
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: "[greydns - Do not manually edit]default/test-service",
			ZoneID:  "zone123",
		},
	}

	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleDeletions(provider, existingRecords, zonesToNames, service)

	// Should delete the record
	require.Len(t, provider.deleteRecordCalls, 1)
	deleteCall := provider.deleteRecordCalls[0]
	assert.Equal(t, "existing-123", deleteCall.recordID)
	assert.Equal(t, "zone123", deleteCall.zoneID)

	// Should remove from cache
	assert.NotContains(t, existingRecords, "api.example.com")
}

func TestHandleDeletions_RecordExists_WrongOwner(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	// Pre-populate with existing record owned by different service
	existingRecords := map[string]*types.DNSRecord{
		"api.example.com": {
			ID:      "existing-123",
			Name:    "api.example.com",
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: "[greydns - Do not manually edit]default/other-service",
			ZoneID:  "zone123",
		},
	}

	zonesToNames := map[string]string{"example.com": "zone123"}
	service := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleDeletions(provider, existingRecords, zonesToNames, service)

	// Should not delete the record (wrong owner)
	assert.Empty(t, provider.deleteRecordCalls)

	// Should keep in cache
	assert.Contains(t, existingRecords, "api.example.com")
}

func TestHandleUpdates_RecordDoesNotExist(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone123"}

	service := createTestService("test-service", "default", "api.example.com", "example.com", true)
	oldService := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleUpdates(provider, existingRecords, "192.168.1.1", zonesToNames, service, oldService)

	// Should create a new record (calls HandleAnnotations internally)
	require.Len(t, provider.createRecordCalls, 1)
}

func TestHandleUpdates_RecordExists_UpdateContent(t *testing.T) {
	provider := &mockProvider{
		zones: map[string]string{"example.com": "zone123"},
	}

	// Pre-populate with existing record
	existingRecords := map[string]*types.DNSRecord{
		"api.example.com": {
			ID:      "existing-123",
			Name:    "api.example.com",
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: "[greydns - Do not manually edit]default/test-service",
			ZoneID:  "zone123",
		},
	}

	zonesToNames := map[string]string{"example.com": "zone123"}

	service := createTestService("test-service", "default", "api.example.com", "example.com", true)
	oldService := createTestService("test-service", "default", "api.example.com", "example.com", true)

	HandleUpdates(provider, existingRecords, "192.168.1.2", zonesToNames, service, oldService)

	// Should update the existing record
	require.Len(t, provider.updateRecordCalls, 1)
	updateCall := provider.updateRecordCalls[0]
	assert.Equal(t, "existing-123", updateCall.RecordID)
	assert.Equal(t, "api.example.com", updateCall.Name)
	assert.Equal(t, "192.168.1.2", updateCall.Content)
}

func TestNewDNSManager(t *testing.T) {
	provider := &mockProvider{}
	manager := NewDNSManager(provider)

	assert.NotNil(t, manager)
	assert.Equal(t, provider, manager.provider)
}
