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
		t.Skip("CLOUDFLARE_API_TOKEN, CLOUDFLARE_ZONE_ID, and CLOUDFLARE_ZONE_NAME must be set for integration tests")
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

	// Test record operations with real DNS under int-test.greydns.io
	testDomain := fmt.Sprintf("provider-test-%d.int-test.greydns.io", time.Now().Unix())

	t.Run("CreateRecord", func(t *testing.T) {
		params := types.CreateRecordParams{
			Name:    testDomain,
			Type:    types.RecordTypeA,
			Content: "192.0.2.1", // Test IP from RFC5737
			TTL:     60,
			Comment: "[greydns - Do not manually edit]integration/test",
			ZoneID:  zoneID,
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
			}

			updatedRecord, err := manager.UpdateRecord(updateParams)
			require.NoError(t, err)
			assert.Equal(t, testDomain, updatedRecord.Name)
			assert.Equal(t, "192.0.2.2", updatedRecord.Content)
			assert.Equal(t, 120, updatedRecord.TTL)
		})
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
	if zoneName := os.Getenv("CLOUDFLARE_ZONE_NAME"); zoneName == "int-test.greydns.io" {
		testDomain := fmt.Sprintf("k8s-svc-test-%d.int-test.greydns.io", time.Now().Unix())

		t.Run("ServiceDNSIntegration", func(t *testing.T) {
			// Create test service
			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-integration",
					Namespace: namespace,
					Annotations: map[string]string{
						"greydns.io/dns":    "true",
						"greydns.io/domain": testDomain,
						"greydns.io/zone":   "int-test.greydns.io",
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
					err := manager.DeleteRecord(record.ID, zones["int-test.greydns.io"])
					if err != nil {
						t.Logf("Failed to cleanup DNS record: %v", err)
					}
				}
			}
		})
	}
}
