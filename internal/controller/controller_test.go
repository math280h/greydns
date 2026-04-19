package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
	krecord "k8s.io/client-go/tools/record"

	"github.com/math280h/greydns/internal/controller"
	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/fake"
	"github.com/math280h/greydns/internal/records"
	"github.com/math280h/greydns/internal/utils"
)

const (
	itZoneID     = "zone-id"
	itZoneName   = "example.com"
	itDomain     = "api.example.com"
	itIngressDst = "1.2.3.4"
	itNamespace  = "default"
	itSvcName    = "api"
)

// itRig wires a Controller against a fake k8s clientset and a fake DNS
// provider, returning all three so tests can drive one side and assert
// on the other.
type itRig struct {
	clientset *kfake.Clientset
	provider  *fake.Provider
	recorder  *krecord.FakeRecorder
}

func newRig(t *testing.T) *itRig {
	t.Helper()
	clientset := kfake.NewSimpleClientset()
	provider := fake.New(dnsprovider.Zone{ID: itZoneID, Name: itZoneName})
	reconciler := records.NewReconciler(
		provider,
		map[string]string{itZoneName: itZoneID},
		itIngressDst,
		60,
		dnsprovider.RecordTypeA,
		records.NewOverridePolicy("", false),
	)
	recorder := krecord.NewFakeRecorder(32)
	prev := utils.Recorder
	utils.Recorder = recorder //nolint:reassign // integration tests stub the event recorder
	t.Cleanup(func() {
		utils.Recorder = prev //nolint:reassign // restore the production recorder
	})

	ctrl := controller.New(clientset, reconciler)
	ctx, cancel := context.WithCancel(context.Background())
	if err := ctrl.Start(ctx); err != nil {
		cancel()
		t.Fatalf("controller.Start: %v", err)
	}
	t.Cleanup(cancel)
	if !ctrl.Ready() {
		t.Fatal("controller should report ready after Start")
	}
	return &itRig{
		clientset: clientset,
		provider:  provider,
		recorder:  recorder,
	}
}

const eventuallyTimeout = 5 * time.Second

// eventually polls cond until it returns true or eventuallyTimeout
// elapses. The informer delivers events asynchronously on its own
// goroutine so direct assertions after Create/Update/Delete on the
// clientset would race with event delivery.
func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(eventuallyTimeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %s", eventuallyTimeout, msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newService(name string, annotations map[string]string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   itNamespace,
			Name:        name,
			Annotations: annotations,
		},
	}
}

func dnsOn() map[string]string {
	return map[string]string{
		records.AnnotationDNS:    "true",
		records.AnnotationZone:   itZoneName,
		records.AnnotationDomain: itDomain,
	}
}

func TestController_AddCreatesRecord(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	svc := newService(itSvcName, dnsOn())
	if _, err := rig.clientset.CoreV1().Services(itNamespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	eventually(t, "provider to hold 1 record", func() bool {
		return len(rig.provider.Snapshot()) == 1
	})
	rec := rig.provider.Snapshot()[0]
	if rec.Name != itDomain {
		t.Fatalf("record name = %q, want %q", rec.Name, itDomain)
	}
	if rec.OwnerRef != dnsprovider.OwnerRefFor(itNamespace, itSvcName) {
		t.Fatalf("owner ref = %q, want %q", rec.OwnerRef, dnsprovider.OwnerRefFor(itNamespace, itSvcName))
	}
}

func TestController_AddIgnoresServiceWithoutDNSAnnotation(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	// No greydns annotations.
	svc := newService(itSvcName, nil)
	if _, err := rig.clientset.CoreV1().Services(itNamespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	// Also a Service with dns explicitly false.
	off := newService("off", map[string]string{
		records.AnnotationDNS:    "false",
		records.AnnotationZone:   itZoneName,
		records.AnnotationDomain: "off.example.com",
	})
	if _, err := rig.clientset.CoreV1().Services(itNamespace).Create(ctx, off, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create off service: %v", err)
	}

	// Give the informer time to deliver both Add events.
	time.Sleep(200 * time.Millisecond)
	if got := len(rig.provider.Snapshot()); got != 0 {
		t.Fatalf("provider should hold 0 records, has %d", got)
	}
}

func TestController_UpdateRenamesDomain(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	svc, err := rig.clientset.CoreV1().Services(itNamespace).Create(
		ctx,
		newService(itSvcName, dnsOn()),
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, "initial record", func() bool {
		return len(rig.provider.Snapshot()) == 1
	})

	const newDomain = "renamed.example.com"
	svc.Annotations[records.AnnotationDomain] = newDomain
	_, updateErr := rig.clientset.CoreV1().Services(itNamespace).Update(ctx, svc, metav1.UpdateOptions{})
	if updateErr != nil {
		t.Fatalf("update: %v", updateErr)
	}

	eventually(t, "record renamed", func() bool {
		snap := rig.provider.Snapshot()
		return len(snap) == 1 && snap[0].Name == newDomain
	})
}

func TestController_UpdateIgnoresNonGreydnsAnnotations(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	svc, err := rig.clientset.CoreV1().Services(itNamespace).Create(
		ctx,
		newService(itSvcName, dnsOn()),
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, "initial record", func() bool {
		return len(rig.provider.Snapshot()) == 1
	})

	initial := rig.provider.Snapshot()[0]

	// Mutate an unrelated annotation. UpdateRecord must NOT be called.
	svc.Annotations["unrelated.example.com/foo"] = "bar"
	_, updateErr := rig.clientset.CoreV1().Services(itNamespace).Update(ctx, svc, metav1.UpdateOptions{})
	if updateErr != nil {
		t.Fatalf("update: %v", updateErr)
	}

	time.Sleep(200 * time.Millisecond)
	after := rig.provider.Snapshot()
	if len(after) != 1 || after[0].ID != initial.ID {
		t.Fatalf("non-greydns annotation change should not touch the record, before=%+v after=%+v", initial, after)
	}
}

func TestController_DeleteRemovesRecord(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	if _, err := rig.clientset.CoreV1().Services(itNamespace).Create(
		ctx,
		newService(itSvcName, dnsOn()),
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, "record created", func() bool {
		return len(rig.provider.Snapshot()) == 1
	})

	if err := rig.clientset.CoreV1().Services(itNamespace).Delete(ctx, itSvcName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	eventually(t, "record removed", func() bool {
		return len(rig.provider.Snapshot()) == 0
	})
}

func TestController_DuplicateDomainEmitsEvent(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	if _, err := rig.clientset.CoreV1().Services(itNamespace).Create(
		ctx,
		newService("first", dnsOn()),
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create first: %v", err)
	}
	eventually(t, "first record", func() bool {
		return len(rig.provider.Snapshot()) == 1
	})

	if _, err := rig.clientset.CoreV1().Services(itNamespace).Create(
		ctx,
		newService("second", dnsOn()),
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create second: %v", err)
	}

	eventually(t, "DuplicateDomain event", func() bool {
		select {
		case ev := <-rig.recorder.Events:
			return strings.Contains(ev, "DuplicateDomain")
		default:
			return false
		}
	})
	if got := len(rig.provider.Snapshot()); got != 1 {
		t.Fatalf("provider should still hold exactly one record, has %d", got)
	}
}

func TestController_PerServiceTTLTriggersUpdate(t *testing.T) {
	rig := newRig(t)
	ctx := context.Background()

	svc, err := rig.clientset.CoreV1().Services(itNamespace).Create(
		ctx,
		newService(itSvcName, dnsOn()),
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	eventually(t, "initial record at default TTL", func() bool {
		snap := rig.provider.Snapshot()
		return len(snap) == 1 && snap[0].TTL == 60
	})

	svc.Annotations[records.AnnotationTTL] = "900"
	if _, updateErr := rig.clientset.CoreV1().Services(itNamespace).Update(ctx, svc, metav1.UpdateOptions{}); updateErr != nil {
		t.Fatalf("update: %v", updateErr)
	}

	eventually(t, "TTL updated to 900", func() bool {
		snap := rig.provider.Snapshot()
		return len(snap) == 1 && snap[0].TTL == 900
	})
}
