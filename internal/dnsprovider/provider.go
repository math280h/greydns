// Package dnsprovider defines the provider-neutral interface and data types
// that the greydns controller uses to manage DNS records. Concrete backends
// live under internal/providers/<name>.
package dnsprovider

import (
	"context"
	"errors"
	"fmt"
)

type RecordType string

const (
	RecordTypeA     RecordType = "A"
	RecordTypeCNAME RecordType = "CNAME"
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
// OwnerRef carries the controller's ownership marker in the form
// "<namespace>/<service>". Each provider persists this using whatever native
// mechanism it has available (Cloudflare: Comment; Route53: sibling TXT;
// GCP DNS: RRSet description; etc.).
type Record struct {
	ID       string
	ZoneID   string
	Name     string
	Type     RecordType
	Content  string
	TTL      int
	OwnerRef string
}

func OwnerRefFor(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
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
	for _, s := range supported {
		if s == t {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnsupportedRecordType, t)
}
