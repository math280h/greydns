package fake_test

import (
	"testing"

	"github.com/math280h/greydns/internal/dnsprovider"
	"github.com/math280h/greydns/internal/dnsprovider/fake"
	"github.com/math280h/greydns/internal/dnsprovidertest"
)

// TestFakeContract runs the shared provider contract suite against the
// fake. The fake is the reference implementation for the contract so
// any regression here means the contract itself or the fake drifted.
func TestFakeContract(t *testing.T) {
	dnsprovidertest.RunContractTests(t, func(t *testing.T) dnsprovidertest.Harness {
		t.Helper()
		zone := dnsprovider.Zone{ID: "z1", Name: "example.com"}
		p := fake.New(zone)
		return dnsprovidertest.Harness{
			Provider: p,
			Zones:    []dnsprovider.Zone{zone},
			SeedUnmanaged: func(rec dnsprovider.Record) {
				p.Seed(rec)
			},
		}
	})
}
