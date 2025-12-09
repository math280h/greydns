//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/math280h/greydns/internal/config"
	"github.com/math280h/greydns/internal/providers"
	"github.com/math280h/greydns/internal/types"
)

func TestIntegration_CloudflareProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Get Cloudflare credentials from environment
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	zoneID := os.Getenv("CLOUDFLARE_ZONE_ID")
	zoneName := os.Getenv("CLOUDFLARE_ZONE_NAME")

	if apiToken == "" || zoneID == "" || zoneName == "" {
		t.Fatal("CLOUDFLARE_API_TOKEN, CLOUDFLARE_ZONE_ID, and CLOUDFLARE_ZONE_NAME must be set for integration tests")
	}

	// Initialize ConfigMap for provider to use
	config.ConfigMap = &v1.ConfigMap{
		Data: map[string]string{
			"proxy-enabled": "false",
		},
	}

	// Create provider manager
	manager, err := providers.NewManager("cloudflare")
	require.NoError(t, err)

	// Connect to Cloudflare
	credentials := map[string]string{
		"cloudflare": apiToken,
	}
	err = manager.Connect(credentials)
	require.NoError(t, err)

	// Test zone operations
	t.Run("GetZones", func(t *testing.T) {
		zones, err := manager.GetZones()
		require.NoError(t, err)
		assert.Contains(t, zones, zoneName)
		assert.Equal(t, zoneID, zones[zoneName])
	})

	// Test zone check
	t.Run("CheckZoneExists", func(t *testing.T) {
		zones := map[string]string{zoneName: zoneID}
		zone, err := manager.CheckZoneExists(zoneName, zones)
		require.NoError(t, err)
		assert.Equal(t, zoneName, zone.Name)
		assert.Equal(t, zoneID, zone.ID)
	})

	// Test record operations with real DNS under int-test.greydns.io subdomain
	testDomain := fmt.Sprintf("provider-test-%d.int-test.%s", time.Now().Unix(), zoneName)

	t.Run("CreateRecord", func(t *testing.T) {
		params := types.CreateRecordParams{
			Name:    testDomain,
			Type:    types.RecordTypeA,
			Content: "192.0.2.1", // Test IP from RFC5737
			TTL:     60,
			Comment: "[greydns - Do not manually edit]integration/test",
			ZoneID:  zoneID,
			Proxied: &[]bool{false}[0], // Explicitly set proxied to false
		}

		record, err := manager.CreateRecord(params)
		require.NoError(t, err)
		assert.Equal(t, testDomain, record.Name)
		assert.Equal(t, "A", record.Type)
		assert.Equal(t, "192.0.2.1", record.Content)

		// Clean up
		defer func() {
			err := manager.DeleteRecord(record.ID, zoneID)
			if err != nil {
				t.Logf("Failed to cleanup test record: %v", err)
			}
		}()

		// Test updating the same record
		t.Run("UpdateRecord", func(t *testing.T) {
			updateParams := types.UpdateRecordParams{
				RecordID: record.ID,
				Name:     testDomain,
				Type:     types.RecordTypeA,
				Content:  "192.0.2.2", // Different test IP
				TTL:      120,
				Comment:  "[greydns - Do not manually edit]integration/test-updated",
				ZoneID:   zoneID,
				Proxied:  &[]bool{false}[0], // Explicitly set proxied to false
			}

			updatedRecord, err := manager.UpdateRecord(updateParams)
			require.NoError(t, err)
			assert.Equal(t, testDomain, updatedRecord.Name)
			assert.Equal(t, "192.0.2.2", updatedRecord.Content)
			assert.Equal(t, 120, updatedRecord.TTL)
		})
	})
}

func TestIntegration_GCPProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Get GCP credentials from environment
	projectID := os.Getenv("GCP_PROJECT_ID")
	serviceAccount := os.Getenv("GCP_SERVICE_ACCOUNT_JSON")
	zoneName := os.Getenv("GCP_ZONE_NAME")
	managedZoneID := os.Getenv("GCP_MANAGED_ZONE_ID")

	if projectID == "" || serviceAccount == "" || zoneName == "" || managedZoneID == "" {
		t.Fatal("GCP_PROJECT_ID, GCP_SERVICE_ACCOUNT_JSON, GCP_ZONE_NAME, and GCP_MANAGED_ZONE_ID must be set for integration tests")
	}

	// Initialize ConfigMap for provider to use
	config.ConfigMap = &v1.ConfigMap{
		Data: map[string]string{
			"proxy-enabled": "false",
		},
	}

	// Create provider manager
	manager, err := providers.NewManager("gcp")
	require.NoError(t, err)

	// Connect to GCP
	credentials := map[string]string{
		"gcp-project-id":      projectID,
		"gcp-service-account": serviceAccount,
	}
	err = manager.Connect(credentials)
	require.NoError(t, err)

	// Test provider name
	t.Run("ProviderName", func(t *testing.T) {
		assert.Equal(t, "gcp", manager.Name())
	})

	// Test zone operations
	t.Run("GetZones", func(t *testing.T) {
		zones, err := manager.GetZones()
		require.NoError(t, err)
		assert.Contains(t, zones, zoneName)
		assert.Equal(t, managedZoneID, zones[zoneName])
	})

	// Test zone check
	t.Run("CheckZoneExists", func(t *testing.T) {
		zones := map[string]string{zoneName: managedZoneID}
		zone, err := manager.CheckZoneExists(zoneName, zones)
		require.NoError(t, err)
		assert.Equal(t, zoneName, zone.Name)
		assert.Equal(t, managedZoneID, zone.ID)
	})

	// Test record operations with real DNS
	testDomain := fmt.Sprintf("provider-test-%d.int-test.%s", time.Now().Unix(), zoneName)

	t.Run("CreateRecord", func(t *testing.T) {
		params := types.CreateRecordParams{
			Name:    testDomain,
			Type:    types.RecordTypeA,
			Content: "192.0.2.1", // Test IP from RFC5737
			TTL:     60,
			Comment: "[greydns - Do not manually edit]integration/test",
			ZoneID:  managedZoneID,
		}

		record, err := manager.CreateRecord(params)
		require.NoError(t, err)
		assert.Equal(t, testDomain, record.Name)
		assert.Equal(t, "A", record.Type)
		assert.Equal(t, "192.0.2.1", record.Content)

		// Clean up
		defer func() {
			err := manager.DeleteRecord(record.ID, managedZoneID)
			if err != nil {
				t.Logf("Failed to cleanup test record: %v", err)
			}
		}()

		// Test updating the same record
		t.Run("UpdateRecord", func(t *testing.T) {
			updateParams := types.UpdateRecordParams{
				RecordID: record.ID,
				Name:     testDomain,
				Type:     types.RecordTypeA,
				Content:  "192.0.2.2", // Different test IP
				TTL:      120,
				Comment:  "[greydns - Do not manually edit]integration/test-updated",
				ZoneID:   managedZoneID,
			}

			updatedRecord, err := manager.UpdateRecord(updateParams)
			require.NoError(t, err)
			assert.Equal(t, testDomain, updatedRecord.Name)
			assert.Equal(t, "192.0.2.2", updatedRecord.Content)
			assert.Equal(t, 120, updatedRecord.TTL)
		})
	})

	// Test CNAME record
	t.Run("CreateCNAMERecord", func(t *testing.T) {
		cnameTestDomain := fmt.Sprintf("cname-test-%d.int-test.%s", time.Now().Unix(), zoneName)
		params := types.CreateRecordParams{
			Name:    cnameTestDomain,
			Type:    types.RecordTypeCNAME,
			Content: testDomain,
			TTL:     60,
			Comment: "[greydns - Do not manually edit]integration/cname-test",
			ZoneID:  managedZoneID,
		}

		record, err := manager.CreateRecord(params)
		require.NoError(t, err)
		assert.Equal(t, cnameTestDomain, record.Name)
		assert.Equal(t, "CNAME", record.Type)

		// Clean up
		defer func() {
			err := manager.DeleteRecord(record.ID, managedZoneID)
			if err != nil {
				t.Logf("Failed to cleanup CNAME test record: %v", err)
			}
		}()
	})

	// Test GetRecords
	t.Run("GetRecords", func(t *testing.T) {
		zones := map[string]string{zoneName: managedZoneID}
		records, err := manager.RefreshRecordsCache(zones)
		require.NoError(t, err)
		assert.NotEmpty(t, records)
	})

	// Test cleanup functionality
	t.Run("CleanupRecords", func(t *testing.T) {
		// Create two records for the same service
		oldDomain := fmt.Sprintf("cleanup-old-%d.int-test.%s", time.Now().Unix(), zoneName)
		newDomain := fmt.Sprintf("cleanup-new-%d.int-test.%s", time.Now().Unix(), zoneName)

		// Create old record
		oldParams := types.CreateRecordParams{
			Name:    oldDomain,
			Type:    types.RecordTypeA,
			Content: "192.0.2.3",
			TTL:     60,
			Comment: "[greydns - Do not manually edit]integration/cleanup-test",
			ZoneID:  managedZoneID,
		}
		oldRecord, err := manager.CreateRecord(oldParams)
		require.NoError(t, err)

		// Create new record
		newParams := types.CreateRecordParams{
			Name:    newDomain,
			Type:    types.RecordTypeA,
			Content: "192.0.2.4",
			TTL:     60,
			Comment: "[greydns - Do not manually edit]integration/cleanup-test",
			ZoneID:  managedZoneID,
		}
		newRecord, err := manager.CreateRecord(newParams)
		require.NoError(t, err)

		// Get existing records
		existingRecords := map[string]*types.DNSRecord{
			oldDomain: oldRecord,
			newDomain: newRecord,
		}

		// Run cleanup - should delete old record but keep new one
		err = manager.CleanupRecords(existingRecords, "integration", "cleanup-test", managedZoneID, newDomain)
		require.NoError(t, err)

		// Verify old record was removed from map
		_, oldExists := existingRecords[oldDomain]
		_, newExists := existingRecords[newDomain]
		assert.False(t, oldExists, "Old record should be cleaned up")
		assert.True(t, newExists, "New record should remain")

		// Clean up remaining record
		defer func() {
			err := manager.DeleteRecord(newRecord.ID, managedZoneID)
			if err != nil {
				t.Logf("Failed to cleanup new record: %v", err)
			}
		}()
	})
}

func TestIntegration_MultiProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Test both providers can be initialized
	t.Run("CloudflareProviderInit", func(t *testing.T) {
		manager, err := providers.NewManager("cloudflare")
		require.NoError(t, err)
		assert.Equal(t, "cloudflare", manager.Name())
	})

	t.Run("GCPProviderInit", func(t *testing.T) {
		manager, err := providers.NewManager("gcp")
		require.NoError(t, err)
		assert.Equal(t, "gcp", manager.Name())
	})

	t.Run("GoogleAliasProviderInit", func(t *testing.T) {
		manager, err := providers.NewManager("google")
		require.NoError(t, err)
		assert.Equal(t, "gcp", manager.Name())
	})

	t.Run("UnsupportedProvider", func(t *testing.T) {
		_, err := providers.NewManager("unsupported-provider")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported provider")
	})
}

func TestIntegration_KubernetesController(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no kubeconfig available (not in cluster)
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Skip("No kubeconfig available for integration test")
	}

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	// Test namespace
	namespace := "greydns-integration"

	// Verify GreyDNS is running
	t.Run("VerifyDeployment", func(t *testing.T) {
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), "greydns", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, int32(1), *deployment.Spec.Replicas)
		assert.Equal(t, int32(1), deployment.Status.ReadyReplicas)
	})

	// Create a test service and verify DNS record creation
	if zoneName := os.Getenv("CLOUDFLARE_ZONE_NAME"); zoneName != "" {
		testDomain := fmt.Sprintf("k8s-svc-test-%d.int-test.%s", time.Now().Unix(), zoneName)

		t.Run("ServiceDNSIntegration", func(t *testing.T) {
			// Create test service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-integration",
					Namespace: namespace,
					Annotations: map[string]string{
						"greydns.io/dns":    "true",
						"greydns.io/domain": testDomain,
						"greydns.io/zone":   zoneName,
					},
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Port:     80,
							Protocol: v1.ProtocolTCP,
						},
					},
					Selector: map[string]string{
						"app": "test",
					},
				},
			}

			_, err := clientset.CoreV1().Services(namespace).Create(context.Background(), service, metav1.CreateOptions{})
			require.NoError(t, err)

			// Clean up service
			defer func() {
				err := clientset.CoreV1().Services(namespace).Delete(context.Background(), "test-service-integration", metav1.DeleteOptions{})
				if err != nil {
					t.Logf("Failed to cleanup test service: %v", err)
				}
			}()

			// Wait a bit for the controller to process
			time.Sleep(30 * time.Second)

			// Verify DNS record was created by checking Cloudflare
			if apiToken := os.Getenv("CLOUDFLARE_API_TOKEN"); apiToken != "" {
				manager, err := providers.NewManager("cloudflare")
				require.NoError(t, err)

				err = manager.Connect(map[string]string{"cloudflare": apiToken})
				require.NoError(t, err)

				zones, err := manager.GetZones()
				require.NoError(t, err)

				records, err := manager.RefreshRecordsCache(zones)
				require.NoError(t, err)

				// Check if our test domain was created
				record, exists := records[testDomain]
				assert.True(t, exists, "DNS record should have been created")
				if exists {
					assert.Equal(t, testDomain, record.Name)
					assert.Contains(t, record.Comment, "test-service-integration")

					// Cleanup the DNS record
					err := manager.DeleteRecord(record.ID, zones[zoneName])
					if err != nil {
						t.Logf("Failed to cleanup DNS record: %v", err)
					}
				}
			}
		})
	}

	// Create a test service and verify DNS record creation with GCP
	if gcpZoneName := os.Getenv("GCP_ZONE_NAME"); gcpZoneName != "" {
		testDomain := fmt.Sprintf("k8s-svc-test-%d.int-test.%s", time.Now().Unix(), gcpZoneName)

		t.Run("ServiceDNSIntegration_GCP", func(t *testing.T) {
			// Create test service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-integration-gcp",
					Namespace: namespace,
					Annotations: map[string]string{
						"greydns.io/dns":    "true",
						"greydns.io/domain": testDomain,
						"greydns.io/zone":   gcpZoneName,
					},
				},
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Port:     80,
							Protocol: v1.ProtocolTCP,
						},
					},
					Selector: map[string]string{
						"app": "test",
					},
				},
			}

			_, err := clientset.CoreV1().Services(namespace).Create(context.Background(), service, metav1.CreateOptions{})
			require.NoError(t, err)

			// Clean up service
			defer func() {
				err := clientset.CoreV1().Services(namespace).Delete(context.Background(), "test-service-integration-gcp", metav1.DeleteOptions{})
				if err != nil {
					t.Logf("Failed to cleanup test service: %v", err)
				}
			}()

			// Wait a bit for the controller to process
			time.Sleep(30 * time.Second)

			// Verify DNS record was created by checking GCP
			projectID := os.Getenv("GCP_PROJECT_ID")
			serviceAccount := os.Getenv("GCP_SERVICE_ACCOUNT_JSON")
			managedZoneID := os.Getenv("GCP_MANAGED_ZONE_ID")

			if projectID != "" && serviceAccount != "" && managedZoneID != "" {
				manager, err := providers.NewManager("gcp")
				require.NoError(t, err)

				credentials := map[string]string{
					"gcp-project-id":      projectID,
					"gcp-service-account": serviceAccount,
				}
				err = manager.Connect(credentials)
				require.NoError(t, err)

				zones, err := manager.GetZones()
				require.NoError(t, err)

				records, err := manager.RefreshRecordsCache(zones)
				require.NoError(t, err)

				// Check if our test domain was created
				record, exists := records[testDomain]
				assert.True(t, exists, "DNS record should have been created")
				if exists {
					assert.Equal(t, testDomain, record.Name)
					assert.Contains(t, record.Comment, "test-service-integration-gcp")

					// Cleanup the DNS record
					err := manager.DeleteRecord(record.ID, managedZoneID)
					if err != nil {
						t.Logf("Failed to cleanup DNS record: %v", err)
					}
				}
			}
		})
	}
}
