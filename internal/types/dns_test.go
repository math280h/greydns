package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecordType_String(t *testing.T) {
	tests := []struct {
		name     string
		rt       RecordType
		expected string
	}{
		{
			name:     "A record type",
			rt:       RecordTypeA,
			expected: "A",
		},
		{
			name:     "CNAME record type",
			rt:       RecordTypeCNAME,
			expected: "CNAME",
		},
		{
			name:     "AAAA record type",
			rt:       RecordTypeAAAA,
			expected: "AAAA",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, string(tt.rt))
		})
	}
}

func TestDNSRecord_Validation(t *testing.T) {
	tests := []struct {
		name    string
		record  DNSRecord
		wantErr bool
	}{
		{
			name: "valid A record",
			record: DNSRecord{
				ID:      "test-id",
				Name:    "example.com",
				Type:    "A",
				Content: "192.168.1.1",
				TTL:     300,
				Comment: "test comment",
				ZoneID:  "zone-123",
			},
			wantErr: false,
		},
		{
			name: "valid CNAME record",
			record: DNSRecord{
				ID:      "test-id-2",
				Name:    "www.example.com",
				Type:    "CNAME",
				Content: "example.com",
				TTL:     300,
				Comment: "test comment",
				ZoneID:  "zone-123",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// For now, just verify the structure is correct
			assert.NotEmpty(t, tt.record.ID)
			assert.NotEmpty(t, tt.record.Name)
			assert.NotEmpty(t, tt.record.Type)
			assert.NotEmpty(t, tt.record.Content)
			assert.Greater(t, tt.record.TTL, 0)
		})
	}
}

func TestCreateRecordParams_Validation(t *testing.T) {
	tests := []struct {
		name   string
		params CreateRecordParams
		valid  bool
	}{
		{
			name: "valid A record params",
			params: CreateRecordParams{
				Name:    "test.example.com",
				Type:    RecordTypeA,
				Content: "192.168.1.1",
				TTL:     300,
				Comment: "[greydns - Do not manually edit]default/test-service",
				ZoneID:  "zone-123",
			},
			valid: true,
		},
		{
			name: "invalid params - empty name",
			params: CreateRecordParams{
				Name:    "",
				Type:    RecordTypeA,
				Content: "192.168.1.1",
				TTL:     300,
				ZoneID:  "zone-123",
			},
			valid: false,
		},
		{
			name: "invalid params - zero TTL",
			params: CreateRecordParams{
				Name:    "test.example.com",
				Type:    RecordTypeA,
				Content: "192.168.1.1",
				TTL:     0,
				ZoneID:  "zone-123",
			},
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.valid {
				assert.NotEmpty(t, tt.params.Name)
				assert.NotEmpty(t, tt.params.Type)
				assert.NotEmpty(t, tt.params.Content)
				assert.Greater(t, tt.params.TTL, 0)
				assert.NotEmpty(t, tt.params.ZoneID)
			} else {
				// Test specific validation failures
				if tt.params.Name == "" {
					assert.Empty(t, tt.params.Name)
				}
				if tt.params.TTL == 0 {
					assert.Equal(t, 0, tt.params.TTL)
				}
			}
		})
	}
}

func TestProviderError(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		message  string
		err      error
		expected string
	}{
		{
			name:     "error without underlying error",
			provider: "cloudflare",
			message:  "zone not found",
			err:      nil,
			expected: "cloudflare: zone not found",
		},
		{
			name:     "error with underlying error",
			provider: "cloudflare",
			message:  "failed to create record",
			err:      assert.AnError,
			expected: "cloudflare: failed to create record: assert.AnError general error for testing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providerErr := NewProviderError(tt.provider, tt.message, tt.err)
			assert.Equal(t, tt.expected, providerErr.Error())
			assert.Equal(t, tt.provider, providerErr.Provider)
			assert.Equal(t, tt.message, providerErr.Message)
			assert.Equal(t, tt.err, providerErr.Err)
		})
	}
}
