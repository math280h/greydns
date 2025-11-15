package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/math280h/greydns/internal/providers"
)

func main() {
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	zoneID := os.Getenv("CLOUDFLARE_ZONE_ID")

	if apiToken == "" || zoneID == "" {
		fmt.Println("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ZONE_ID must be set")
		os.Exit(1)
	}

	// Create provider manager
	manager, err := providers.NewManager("cloudflare")
	if err != nil {
		fmt.Printf("Failed to create provider manager: %v\n", err)
		os.Exit(1)
	}

	// Connect to Cloudflare
	credentials := map[string]string{
		"cloudflare": apiToken,
	}
	err = manager.Connect(credentials)
	if err != nil {
		fmt.Printf("Failed to connect to Cloudflare: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Cleaning up test DNS records...")

	// Get zones to use with RefreshRecordsCache
	zones, err := manager.GetZones()
	if err != nil {
		fmt.Printf("Failed to get zones: %v\n", err)
		os.Exit(1)
	}

	// Get all records in all zones (filter by our zone later)
	records, err := manager.RefreshRecordsCache(zones)
	if err != nil {
		fmt.Printf("Failed to refresh records cache: %v\n", err)
		os.Exit(1)
	}

	// Delete any records that look like test records
	deleted := 0
	for name, record := range records {
		// Only process records in our target zone
		if record.ZoneID != zoneID {
			continue
		}

		if isTestRecord(name, record.Comment) {
			fmt.Printf("Deleting test record: %s (%s)\n", name, record.ID)
			err := manager.DeleteRecord(record.ID, zoneID)
			if err != nil {
				fmt.Printf("Failed to delete record %s: %v\n", name, err)
			} else {
				deleted++
			}
		}
	}

	fmt.Printf("Cleanup complete. Deleted %d test records.\n", deleted)
}

// isTestRecord determines if a DNS record is a test record that should be cleaned up
func isTestRecord(name, comment string) bool {
	// Check if record is under int-test.greydns.io domain
	if strings.HasSuffix(name, ".int-test.greydns.io") {
		return true
	}

	// Check if record name contains test indicators (legacy patterns)
	testPrefixes := []string{
		"greydns-integration-test",
		"k8s-integration-test",
		"integration-test",
		"test-",
		"provider-test-",
		"k8s-svc-test-",
	}

	for _, prefix := range testPrefixes {
		if strings.Contains(name, prefix) {
			return true
		}
	}

	// Check if comment indicates it's a test record
	if strings.Contains(comment, "integration/test") ||
		strings.Contains(comment, "test-service-integration") ||
		strings.Contains(comment, "[test]") ||
		strings.Contains(comment, "int-test") {
		return true
	}

	return false
}
