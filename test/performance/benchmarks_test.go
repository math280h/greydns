package performance

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/types"
)

// Mock provider for performance testing
type mockProvider struct {
	records map[string]*types.DNSRecord
}

func (m *mockProvider) Name() string                                { return "mock" }
func (m *mockProvider) Connect(credentials map[string]string) error { return nil }
func (m *mockProvider) GetZones() (map[string]string, error) {
	return map[string]string{"example.com": "zone-123"}, nil
}
func (m *mockProvider) GetZone(zoneID string) (*types.Zone, error) {
	return &types.Zone{ID: "zone-123", Name: "example.com"}, nil
}
func (m *mockProvider) CheckZoneExists(zoneName string, zones map[string]string) (*types.Zone, error) {
	return &types.Zone{ID: "zone-123", Name: "example.com"}, nil
}
func (m *mockProvider) CreateRecord(params types.CreateRecordParams) (*types.DNSRecord, error) {
	record := &types.DNSRecord{
		ID:      fmt.Sprintf("mock-%s", params.Name),
		Name:    params.Name,
		Type:    string(params.Type),
		Content: params.Content,
		TTL:     params.TTL,
		Comment: params.Comment,
		ZoneID:  params.ZoneID,
	}
	m.records[params.Name] = record
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
	m.records[params.Name] = record
	return record, nil
}
func (m *mockProvider) DeleteRecord(recordID, zoneID string) error { return nil }
func (m *mockProvider) GetRecords(zoneID string) (map[string]*types.DNSRecord, error) {
	return m.records, nil
}
func (m *mockProvider) RefreshRecordsCache(zones map[string]string) (map[string]*types.DNSRecord, error) {
	return m.records, nil
}
func (m *mockProvider) CleanupRecords(existingRecords map[string]*types.DNSRecord, namespace, serviceName, zoneID, currentDomain string) error {
	return nil
}

func createTestService(name, namespace string, index int) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", name, index),
			Namespace: namespace,
			Annotations: map[string]string{
				"greydns.io/dns":    "true",
				"greydns.io/domain": fmt.Sprintf("api%d.example.com", index),
				"greydns.io/zone":   "example.com",
			},
		},
	}
}

func BenchmarkHandleAnnotations(b *testing.B) {
	provider := &mockProvider{
		records: make(map[string]*types.DNSRecord),
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone-123"}
	ingressDestination := "192.168.1.1"

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		service := createTestService("test-service", "default", i)
		records.HandleAnnotations(provider, existingRecords, ingressDestination, zonesToNames, service)
	}
}

func BenchmarkHandleAnnotations_WithExistingRecords(b *testing.B) {
	provider := &mockProvider{
		records: make(map[string]*types.DNSRecord),
	}

	// Pre-populate with many existing records
	existingRecords := make(map[string]*types.DNSRecord)
	for i := 0; i < 1000; i++ {
		domain := fmt.Sprintf("existing%d.example.com", i)
		existingRecords[domain] = &types.DNSRecord{
			ID:      fmt.Sprintf("existing-%d", i),
			Name:    domain,
			Type:    "A",
			Content: "192.168.1.100",
			TTL:     300,
			Comment: fmt.Sprintf("[greydns - Do not manually edit]default/existing-%d", i),
			ZoneID:  "zone-123",
		}
	}

	zonesToNames := map[string]string{"example.com": "zone-123"}
	ingressDestination := "192.168.1.1"

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		service := createTestService("test-service", "default", i+10000) // Avoid conflicts
		records.HandleAnnotations(provider, existingRecords, ingressDestination, zonesToNames, service)
	}
}

func BenchmarkHandleUpdates(b *testing.B) {
	provider := &mockProvider{
		records: make(map[string]*types.DNSRecord),
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone-123"}

	// Pre-populate with existing records to update
	for i := 0; i < b.N; i++ {
		domain := fmt.Sprintf("api%d.example.com", i)
		existingRecords[domain] = &types.DNSRecord{
			ID:      fmt.Sprintf("mock-%d", i),
			Name:    domain,
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: fmt.Sprintf("[greydns - Do not manually edit]default/test-service-%d", i),
			ZoneID:  "zone-123",
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		service := createTestService("test-service", "default", i)
		oldService := createTestService("test-service", "default", i)
		records.HandleUpdates(provider, existingRecords, "192.168.1.2", zonesToNames, service, oldService)
	}
}

func BenchmarkHandleDeletions(b *testing.B) {
	provider := &mockProvider{
		records: make(map[string]*types.DNSRecord),
	}

	existingRecords := make(map[string]*types.DNSRecord)
	zonesToNames := map[string]string{"example.com": "zone-123"}

	// Pre-populate with existing records to delete
	for i := 0; i < b.N; i++ {
		domain := fmt.Sprintf("api%d.example.com", i)
		existingRecords[domain] = &types.DNSRecord{
			ID:      fmt.Sprintf("mock-%d", i),
			Name:    domain,
			Type:    "A",
			Content: "192.168.1.1",
			TTL:     300,
			Comment: fmt.Sprintf("[greydns - Do not manually edit]default/test-service-%d", i),
			ZoneID:  "zone-123",
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		service := createTestService("test-service", "default", i)
		records.HandleDeletions(provider, existingRecords, zonesToNames, service)
	}
}

func BenchmarkProviderOperations(b *testing.B) {
	provider := &mockProvider{
		records: make(map[string]*types.DNSRecord),
	}

	require.NoError(b, provider.Connect(map[string]string{"mock": "test"}))

	b.Run("CreateRecord", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			params := types.CreateRecordParams{
				Name:    fmt.Sprintf("bench%d.example.com", i),
				Type:    types.RecordTypeA,
				Content: "192.168.1.1",
				TTL:     300,
				Comment: fmt.Sprintf("[greydns - Do not manually edit]default/bench-%d", i),
				ZoneID:  "zone-123",
			}
			_, err := provider.CreateRecord(params)
			require.NoError(b, err)
		}
	})

	b.Run("UpdateRecord", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			params := types.UpdateRecordParams{
				RecordID: fmt.Sprintf("mock-bench%d.example.com", i),
				Name:     fmt.Sprintf("bench%d.example.com", i),
				Type:     types.RecordTypeA,
				Content:  "192.168.1.2",
				TTL:      300,
				Comment:  fmt.Sprintf("[greydns - Do not manually edit]default/bench-%d", i),
				ZoneID:   "zone-123",
			}
			_, err := provider.UpdateRecord(params)
			require.NoError(b, err)
		}
	})
}

func BenchmarkMemoryUsage(b *testing.B) {
	provider := &mockProvider{
		records: make(map[string]*types.DNSRecord),
	}

	// This benchmark helps measure memory allocation patterns
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		existingRecords := make(map[string]*types.DNSRecord)
		zonesToNames := map[string]string{"example.com": "zone-123"}
		service := createTestService("test-service", "default", i)

		records.HandleAnnotations(provider, existingRecords, "192.168.1.1", zonesToNames, service)
	}
}
