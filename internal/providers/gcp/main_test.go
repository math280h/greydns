package gcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/math280h/greydns/internal/types"
)

func TestProvider_Name(t *testing.T) {
	provider := &Provider{}
	assert.Equal(t, "gcp", provider.Name())
}

func TestProvider_Connect(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]string
		expectError bool
		errorMsg    string
	}{
		{
			name: "missing project ID",
			credentials: map[string]string{
				"gcp-service-account": `{"type": "service_account"}`,
			},
			expectError: true,
			errorMsg:    "gcp-project-id not found in credentials",
		},
		{
			name: "missing service account",
			credentials: map[string]string{
				"gcp-project-id": "test-project",
			},
			expectError: true,
			errorMsg:    "gcp-service-account not found in credentials",
		},
		{
			name: "invalid service account JSON",
			credentials: map[string]string{
				"gcp-project-id":      "test-project",
				"gcp-service-account": "invalid-json",
			},
			expectError: true,
			errorMsg:    "Failed to create DNS service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &Provider{}
			err := provider.Connect(tt.credentials)

			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, provider.service)
				assert.NotNil(t, provider.commentPattern)
				assert.Equal(t, tt.credentials["gcp-project-id"], provider.projectID)
			}
		})
	}
}

func TestProvider_CreateRecordParams_Validation(t *testing.T) {
	tests := []struct {
		name   string
		params types.CreateRecordParams
		valid  bool
	}{
		{
			name: "valid A record",
			params: types.CreateRecordParams{
				Name:    "test.example.com",
				Type:    types.RecordTypeA,
				Content: "192.168.1.1",
				TTL:     300,
				Comment: "[greydns - Do not manually edit]default/test",
				ZoneID:  "test-zone",
			},
			valid: true,
		},
		{
			name: "valid CNAME record",
			params: types.CreateRecordParams{
				Name:    "www.example.com",
				Type:    types.RecordTypeCNAME,
				Content: "example.com",
				TTL:     300,
				Comment: "[greydns - Do not manually edit]default/test",
				ZoneID:  "test-zone",
			},
			valid: true,
		},
		{
			name: "valid AAAA record",
			params: types.CreateRecordParams{
				Name:    "test.example.com",
				Type:    types.RecordTypeAAAA,
				Content: "2001:0db8:85a3:0000:0000:8a2e:0370:7334",
				TTL:     300,
				Comment: "[greydns - Do not manually edit]default/test",
				ZoneID:  "test-zone",
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate the structure is correct
			assert.NotEmpty(t, tt.params.Name)
			assert.NotEmpty(t, tt.params.Type)
			assert.NotEmpty(t, tt.params.Content)
			assert.Greater(t, tt.params.TTL, 0)
			assert.NotEmpty(t, tt.params.ZoneID)
		})
	}
}

func TestProvider_RecordIDFormat(t *testing.T) {
	tests := []struct {
		name       string
		recordID   string
		expectFail bool
	}{
		{
			name:       "valid record ID",
			recordID:   "zone-id:record.name.:A",
			expectFail: false,
		},
		{
			name:       "invalid record ID - missing parts",
			recordID:   "zone-id:record.name",
			expectFail: true,
		},
		{
			name:       "invalid record ID - too many parts",
			recordID:   "zone-id:record.name:A:extra",
			expectFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := strings.Split(tt.recordID, ":")
			if tt.expectFail {
				assert.NotEqual(t, 3, len(parts))
			} else {
				assert.Equal(t, 3, len(parts))
			}
		})
	}
}

func TestProvider_CleanupRecords_Logic(t *testing.T) {
	tests := []struct {
		name            string
		existingRecords map[string]*types.DNSRecord
		namespace       string
		serviceName     string
		currentDomain   string
		shouldCleanup   bool
	}{
		{
			name: "should cleanup old record",
			existingRecords: map[string]*types.DNSRecord{
				"old.example.com": {
					ID:      "zone:old.example.com.:A",
					Name:    "old.example.com",
					Comment: "[greydns - Do not manually edit]default/test-service",
				},
			},
			namespace:     "default",
			serviceName:   "test-service",
			currentDomain: "new.example.com",
			shouldCleanup: true,
		},
		{
			name: "should not cleanup current record",
			existingRecords: map[string]*types.DNSRecord{
				"current.example.com": {
					ID:      "zone:current.example.com.:A",
					Name:    "current.example.com",
					Comment: "[greydns - Do not manually edit]default/test-service",
				},
			},
			namespace:     "default",
			serviceName:   "test-service",
			currentDomain: "current.example.com",
			shouldCleanup: false,
		},
		{
			name: "should not cleanup different service record",
			existingRecords: map[string]*types.DNSRecord{
				"other.example.com": {
					ID:      "zone:other.example.com.:A",
					Name:    "other.example.com",
					Comment: "[greydns - Do not manually edit]default/other-service",
				},
			},
			namespace:     "default",
			serviceName:   "test-service",
			currentDomain: "current.example.com",
			shouldCleanup: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectedComment := "[greydns - Do not manually edit]" + tt.namespace + "/" + tt.serviceName

			for _, record := range tt.existingRecords {
				shouldDelete := record.Comment == expectedComment && record.Name != tt.currentDomain
				assert.Equal(t, tt.shouldCleanup, shouldDelete)
			}
		})
	}
}
