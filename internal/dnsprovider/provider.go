// Package dnsprovider defines the provider-neutral interface and data types
// that the greydns controller uses to manage DNS records. Concrete backends
// live under internal/providers/<name>.
package dnsprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

type RecordType string

const (
	RecordTypeA     RecordType = "A"
	RecordTypeAAAA  RecordType = "AAAA"
	RecordTypeCNAME RecordType = "CNAME"
)

// Kind identifies the Kubernetes resource kind a record is owned by.
// It's persisted inside OwnerRef so the same namespace/name across kinds
// can't collide on record ownership.
type Kind string

const (
	KindService Kind = "svc"
	KindIngress Kind = "ing"
)

var (
	ErrNotImplemented        = errors.New("not implemented")
	ErrZoneNotFound          = errors.New("zone not found")
	ErrUnsupportedRecordType = errors.New("unsupported record type")
)

type Zone struct {
	ID   string
	Name string
}

// Record is a provider-neutral DNS record.
//
// OwnerRef is the controller's ownership marker "<namespace>/<service>";
// each provider persists it natively (CF Comment, R53 shadow TXT, etc.).
//
// ProviderHints are provider-scoped overrides sourced from Service
// annotations. Providers ignore unknown hints and should echo their
// effective state back on List/Create/Update so drift can be detected.
type Record struct {
	ID            string
	ZoneID        string
	Name          string
	Type          RecordType
	Content       string
	TTL           int
	OwnerRef      string
	ProviderHints map[string]string
}

// OwnerRefFor produces the canonical "<kind>:<namespace>/<name>" owner
// reference that providers persist and the reconciler matches against.
func OwnerRefFor(kind Kind, namespace, name string) string {
	return fmt.Sprintf("%s:%s/%s", kind, namespace, name)
}

// Provider is the surface every DNS backend must implement.
//
// Implementations must be safe for concurrent use: the controller calls
// ListOwnedRecords from a background goroutine while Create/Update/Delete
// run from the informer event handlers.
type Provider interface {
	Name() string

	// SupportedRecordTypes declares the record types this provider accepts
	// in Create/Update calls. The controller uses this to validate the
	// configured record-type at startup so misconfiguration surfaces before
	// any reconcile runs.
	SupportedRecordTypes() []RecordType

	ListZones(ctx context.Context) ([]Zone, error)

	// ListOwnedRecords returns only records in zoneID that carry a greydns
	// owner reference; records not managed by this controller must be omitted.
	ListOwnedRecords(ctx context.Context, zoneID string) ([]Record, error)

	// CreateRecord creates rec; rec.ID is ignored and the returned record
	// has the provider-assigned ID populated.
	CreateRecord(ctx context.Context, rec Record) (Record, error)

	UpdateRecord(ctx context.Context, rec Record) (Record, error)
	DeleteRecord(ctx context.Context, zoneID, recordID string) error
}

// ValidateRecordType returns nil if t is in supported, otherwise
// ErrUnsupportedRecordType wrapped with the offending value.
func ValidateRecordType(t RecordType, supported []RecordType) error {
	if slices.Contains(supported, t) {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedRecordType, t)
}
