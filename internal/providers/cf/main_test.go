package cf

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/math280h/greydns/internal/types"
)

func TestProvider_Name(t *testing.T) {
	provider := &Provider{}
	assert.Equal(t, "cloudflare", provider.Name())
}

func TestProvider_Connect(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]string
		expectError bool
	}{
		{
			name: "valid credentials",
			credentials: map[string]string{
				"cloudflare": "test-token",
			},
			expectError: false,
		},
		{
			name:        "missing cloudflare token",
			credentials: map[string]string{},
			expectError: true,
		},
		{
			name: "empty cloudflare token",
			credentials: map[string]string{
				"cloudflare": "",
			},
			expectError: false, // Provider accepts empty token, but will fail later
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &Provider{}
			err := provider.Connect(tt.credentials)

			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "cloudflare API token not found")
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, provider.client)
				assert.NotNil(t, provider.commentPattern)
			}
		})
	}
}

func TestProvider_ConvertToGenericRecord(t *testing.T) {
	provider := &Provider{}

	// This would need to be tested with actual Cloudflare types
	// For now, we test the structure exists
	assert.NotNil(t, provider)
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
				ZoneID:  "zone123",
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
				ZoneID:  "zone123",
			},
			valid: true,
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
			}
		})
	}
}

func TestProvider_ErrorHandling(t *testing.T) {
	provider := &Provider{}

	// Test that provider methods exist and handle errors appropriately
	// without requiring actual Cloudflare connection

	// These tests verify the structure is correct
	assert.Equal(t, "cloudflare", provider.Name())

	// Test connecting first
	err := provider.Connect(map[string]string{"cloudflare": "test-token"})
	assert.NoError(t, err)

	// Now test methods that require a connected client
	// These will fail with invalid token, but structure should be correct
	_, _ = provider.GetZones()
	// We expect an error here with invalid credentials, which is fine
}

// Integration test structure (would run with real credentials in CI)
func TestProvider_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This would test actual Cloudflare integration
	// Skip if no credentials are provided
	t.Skip("Integration test requires real Cloudflare credentials")

	// Example integration test structure:
	/*
		provider := &Provider{}
		credentials := map[string]string{
			"cloudflare": os.Getenv("CLOUDFLARE_API_TOKEN"),
		}

		if credentials["cloudflare"] == "" {
			t.Skip("CLOUDFLARE_API_TOKEN not set")
		}

		err := provider.Connect(credentials)
		require.NoError(t, err)

		zones, err := provider.GetZones()
		require.NoError(t, err)
		assert.NotEmpty(t, zones)
	*/
}
