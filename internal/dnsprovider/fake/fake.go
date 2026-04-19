// Package fake is an in-memory dnsprovider.Provider for tests.
package fake

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/math280h/greydns/internal/dnsprovider"
)

type Provider struct {
	mu      sync.Mutex
	zones   map[string]dnsprovider.Zone
	records map[string]dnsprovider.Record
	nextID  int

	// Err is injected into every method to exercise caller error paths.
	Err error
}

func New(zones ...dnsprovider.Zone) *Provider {
	p := &Provider{
		zones:   make(map[string]dnsprovider.Zone),
		records: make(map[string]dnsprovider.Record),
	}
	for _, z := range zones {
		p.zones[z.ID] = z
	}
	return p
}

// Seed inserts a record, bypassing CreateRecord (useful for fixtures).
func (p *Provider) Seed(rec dnsprovider.Record) dnsprovider.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec.ID == "" {
		p.nextID++
		rec.ID = "fake-" + strconv.Itoa(p.nextID)
	}
	p.records[rec.ID] = rec
	return rec
}

func (p *Provider) Snapshot() []dnsprovider.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]dnsprovider.Record, 0, len(p.records))
	for _, r := range p.records {
		out = append(out, r)
	}
	return out
}

func (p *Provider) Name() string { return "fake" }

func (p *Provider) SupportedRecordTypes() []dnsprovider.RecordType {
	return []dnsprovider.RecordType{dnsprovider.RecordTypeA, dnsprovider.RecordTypeCNAME}
}

func (p *Provider) ListZones(_ context.Context) ([]dnsprovider.Zone, error) {
	if p.Err != nil {
		return nil, p.Err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]dnsprovider.Zone, 0, len(p.zones))
	for _, z := range p.zones {
		out = append(out, z)
	}
	return out, nil
}

func (p *Provider) ListOwnedRecords(_ context.Context, zoneID string) ([]dnsprovider.Record, error) {
	if p.Err != nil {
		return nil, p.Err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]dnsprovider.Record, 0)
	for _, r := range p.records {
		if r.ZoneID == zoneID && r.OwnerRef != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (p *Provider) CreateRecord(_ context.Context, rec dnsprovider.Record) (dnsprovider.Record, error) {
	if p.Err != nil {
		return dnsprovider.Record{}, p.Err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.zones[rec.ZoneID]; !ok {
		return dnsprovider.Record{}, fmt.Errorf("fake: unknown zone %q", rec.ZoneID)
	}
	p.nextID++
	rec.ID = "fake-" + strconv.Itoa(p.nextID)
	p.records[rec.ID] = rec
	return rec, nil
}

func (p *Provider) UpdateRecord(_ context.Context, rec dnsprovider.Record) (dnsprovider.Record, error) {
	if p.Err != nil {
		return dnsprovider.Record{}, p.Err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.records[rec.ID]; !ok {
		return dnsprovider.Record{}, fmt.Errorf("fake: unknown record %q", rec.ID)
	}
	p.records[rec.ID] = rec
	return rec, nil
}

func (p *Provider) DeleteRecord(_ context.Context, zoneID, recordID string) error {
	if p.Err != nil {
		return p.Err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.records[recordID]
	if !ok || r.ZoneID != zoneID {
		return fmt.Errorf("fake: record %q not in zone %q", recordID, zoneID)
	}
	delete(p.records, recordID)
	return nil
}
