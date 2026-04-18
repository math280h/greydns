// Package route53 is a scaffolded AWS Route 53 provider. It registers
// under the name "route53" so the registry plumbing is exercised by a
// second backend with a different shape from Cloudflare (different
// credential model, no native comment field, requires TXT-shadow
// records for ownership). The factory currently returns
// ErrNotImplemented so selecting `provider: route53` fails fast at
// startup instead of letting the controller boot and error on the
// first reconcile.
//
// When implemented, the provider will consume:
//
//	route53.hosted-zone-id  required, the hosted zone to manage
//	route53.region          optional, defaults to us-east-1
//
// Secret keys (Secret greydns-secret):
//
//	aws-access-key-id       required
//	aws-secret-access-key   required
package route53

import (
	"fmt"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/registry"
)

const providerName = "route53"

func init() { //nolint:gochecknoinits // required for provider self-registration
	registry.Register(providerName, New)
}

// New is the registry.Factory for the route53 provider. It always fails
// with ErrNotImplemented until the real implementation lands.
func New(_ registry.ProviderConfig, _ map[string][]byte) (dnsprovider.Provider, error) {
	return nil, fmt.Errorf("route53: %w", dnsprovider.ErrNotImplemented)
}
