package gcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/api/dns/v1"
	"google.golang.org/api/option"

	"github.com/math280h/greydns/internal/types"
)

// Provider implements the DNS provider interface for Google Cloud DNS
type Provider struct {
	service        *dns.Service
	projectID      string
	commentPattern *regexp.Regexp
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "gcp"
}

// Connect initializes the Google Cloud DNS client with credentials
func (p *Provider) Connect(credentials map[string]string) error {
	projectID, exists := credentials["gcp-project-id"]
	if !exists {
		return errors.New("gcp-project-id not found in credentials")
	}
	p.projectID = projectID

	// Check for service account JSON key
	serviceAccountJSON, exists := credentials["gcp-service-account"]
	if !exists {
		return errors.New("gcp-service-account not found in credentials")
	}

	ctx := context.Background()
	var err error
	p.service, err = dns.NewService(ctx, option.WithCredentialsJSON([]byte(serviceAccountJSON)))
	if err != nil {
		return types.NewProviderError("gcp", "Failed to create DNS service", err)
	}

	p.commentPattern = regexp.MustCompile(`^\[greydns - Do not manually edit].*$`)

	return nil
}

// GetZones returns all available managed zones
func (p *Provider) GetZones() (map[string]string, error) {
	zonesToNames := make(map[string]string)

	ctx := context.Background()
	zonesListCall := p.service.ManagedZones.List(p.projectID)

	err := zonesListCall.Pages(ctx, func(resp *dns.ManagedZonesListResponse) error {
		for _, zone := range resp.ManagedZones {
			// Remove trailing dot from DNS name
			dnsName := strings.TrimSuffix(zone.DnsName, ".")
			zonesToNames[dnsName] = zone.Name
			log.Debug().Msgf("[GCP Provider] Found zone: %s (ID: %s)", dnsName, zone.Name)
		}
		return nil
	})

	if err != nil {
		return nil, types.NewProviderError("gcp", "Failed to get zones", err)
	}

	log.Info().Msgf("[GCP Provider] Found %d zones", len(zonesToNames))
	return zonesToNames, nil
}

// GetZone gets a specific zone by ID (zone name in GCP)
func (p *Provider) GetZone(zoneID string) (*types.Zone, error) {
	ctx := context.Background()
	zone, err := p.service.ManagedZones.Get(p.projectID, zoneID).Context(ctx).Do()
	if err != nil {
		return nil, types.NewProviderError("gcp", "Failed to get zone", err)
	}

	return &types.Zone{
		ID:   zone.Name,
		Name: strings.TrimSuffix(zone.DnsName, "."),
	}, nil
}

// CheckZoneExists checks if a zone exists and returns it
func (p *Provider) CheckZoneExists(zoneName string, zones map[string]string) (*types.Zone, error) {
	zoneID, exists := zones[zoneName]
	if !exists {
		return nil, types.NewProviderError("gcp", "Zone not found", nil)
	}

	return p.GetZone(zoneID)
}

// CreateRecord creates a new DNS record
func (p *Provider) CreateRecord(params types.CreateRecordParams) (*types.DNSRecord, error) {
	ctx := context.Background()

	// Ensure the record name ends with a dot for GCP
	recordName := params.Name
	if !strings.HasSuffix(recordName, ".") {
		recordName += "."
	}

	// Create the resource record set
	rrset := &dns.ResourceRecordSet{
		Name: recordName,
		Type: string(params.Type),
		Ttl:  int64(params.TTL),
	}

	// Set the content based on record type
	switch params.Type {
	case types.RecordTypeA, types.RecordTypeAAAA:
		rrset.Rrdatas = []string{params.Content}
	case types.RecordTypeCNAME:
		// CNAME content must end with a dot
		content := params.Content
		if !strings.HasSuffix(content, ".") {
			content += "."
		}
		rrset.Rrdatas = []string{content}
	case types.RecordTypeTXT:
		// TXT records need to be quoted
		rrset.Rrdatas = []string{fmt.Sprintf(`"%s"`, params.Comment)}
	default:
		return nil, types.NewProviderError("gcp", "Invalid record type: "+string(params.Type), nil)
	}

	// Create a change to add the record
	change := &dns.Change{
		Additions: []*dns.ResourceRecordSet{rrset},
	}

	// Execute the change
	changeResp, err := p.service.Changes.Create(p.projectID, params.ZoneID, change).Context(ctx).Do()
	if err != nil {
		return nil, types.NewProviderError("gcp", "Failed to create record", err)
	}

	log.Info().Msgf("[GCP Provider] [%s] Record created (Change ID: %s)", params.Name, changeResp.Id)

	// Wait for the change to complete
	err = p.waitForChange(ctx, params.ZoneID, changeResp.Id)
	if err != nil {
		log.Warn().Err(err).Msg("[GCP Provider] Change creation succeeded but waiting for propagation failed")
	}

	return &types.DNSRecord{
		ID:      fmt.Sprintf("%s:%s:%s", params.ZoneID, recordName, params.Type),
		Name:    strings.TrimSuffix(recordName, "."),
		Type:    string(params.Type),
		Content: params.Content,
		TTL:     params.TTL,
		Comment: params.Comment,
		ZoneID:  params.ZoneID,
	}, nil
}

// UpdateRecord updates an existing DNS record
func (p *Provider) UpdateRecord(params types.UpdateRecordParams) (*types.DNSRecord, error) {
	ctx := context.Background()

	// Ensure the record name ends with a dot for GCP
	recordName := params.Name
	if !strings.HasSuffix(recordName, ".") {
		recordName += "."
	}

	// First, get the existing record to delete it
	existingRrsets, err := p.service.ResourceRecordSets.List(p.projectID, params.ZoneID).
		Name(recordName).
		Type(string(params.Type)).
		Context(ctx).
		Do()

	if err != nil {
		return nil, types.NewProviderError("gcp", "Failed to find existing record", err)
	}

	if len(existingRrsets.Rrsets) == 0 {
		return nil, types.NewProviderError("gcp", "Record not found for update", nil)
	}

	existingRrset := existingRrsets.Rrsets[0]

	// Create the new resource record set
	newRrset := &dns.ResourceRecordSet{
		Name: recordName,
		Type: string(params.Type),
		Ttl:  int64(params.TTL),
	}

	// Set the content based on record type
	switch params.Type {
	case types.RecordTypeA, types.RecordTypeAAAA:
		newRrset.Rrdatas = []string{params.Content}
	case types.RecordTypeCNAME:
		// CNAME content must end with a dot
		content := params.Content
		if !strings.HasSuffix(content, ".") {
			content += "."
		}
		newRrset.Rrdatas = []string{content}
	case types.RecordTypeTXT:
		// TXT records need to be quoted
		newRrset.Rrdatas = []string{fmt.Sprintf(`"%s"`, params.Comment)}
	default:
		return nil, types.NewProviderError("gcp", "Invalid record type: "+string(params.Type), nil)
	}

	// Create a change to replace the record
	change := &dns.Change{
		Deletions: []*dns.ResourceRecordSet{existingRrset},
		Additions: []*dns.ResourceRecordSet{newRrset},
	}

	// Execute the change
	changeResp, err := p.service.Changes.Create(p.projectID, params.ZoneID, change).Context(ctx).Do()
	if err != nil {
		return nil, types.NewProviderError("gcp", "Failed to update record", err)
	}

	log.Info().Msgf("[GCP Provider] [%s] Record updated (Change ID: %s)", params.Name, changeResp.Id)

	// Wait for the change to complete
	err = p.waitForChange(ctx, params.ZoneID, changeResp.Id)
	if err != nil {
		log.Warn().Err(err).Msg("[GCP Provider] Change update succeeded but waiting for propagation failed")
	}

	return &types.DNSRecord{
		ID:      fmt.Sprintf("%s:%s:%s", params.ZoneID, recordName, params.Type),
		Name:    strings.TrimSuffix(recordName, "."),
		Type:    string(params.Type),
		Content: params.Content,
		TTL:     params.TTL,
		Comment: params.Comment,
		ZoneID:  params.ZoneID,
	}, nil
}

// DeleteRecord deletes a DNS record
func (p *Provider) DeleteRecord(recordID, zoneID string) error {
	ctx := context.Background()

	// Parse the recordID: format is "zoneID:recordName:recordType"
	parts := strings.Split(recordID, ":")
	if len(parts) != 3 {
		return types.NewProviderError("gcp", "Invalid record ID format", nil)
	}

	recordName := parts[1]
	recordType := parts[2]

	// Get the existing record
	existingRrsets, err := p.service.ResourceRecordSets.List(p.projectID, zoneID).
		Name(recordName).
		Type(recordType).
		Context(ctx).
		Do()

	if err != nil {
		return types.NewProviderError("gcp", "Failed to find record for deletion", err)
	}

	if len(existingRrsets.Rrsets) == 0 {
		return types.NewProviderError("gcp", "Record not found for deletion", nil)
	}

	existingRrset := existingRrsets.Rrsets[0]

	// Create a change to delete the record
	change := &dns.Change{
		Deletions: []*dns.ResourceRecordSet{existingRrset},
	}

	// Execute the change
	changeResp, err := p.service.Changes.Create(p.projectID, zoneID, change).Context(ctx).Do()
	if err != nil {
		return types.NewProviderError("gcp", "Failed to delete record", err)
	}

	log.Info().Msgf("[GCP Provider] Record deleted (Change ID: %s)", changeResp.Id)

	// Wait for the change to complete
	err = p.waitForChange(ctx, zoneID, changeResp.Id)
	if err != nil {
		log.Warn().Err(err).Msg("[GCP Provider] Change deletion succeeded but waiting for propagation failed")
	}

	return nil
}

// GetRecords gets all managed records for a zone
func (p *Provider) GetRecords(zoneID string) (map[string]*types.DNSRecord, error) {
	ctx := context.Background()
	records := make(map[string]*types.DNSRecord)

	rrsetsList := p.service.ResourceRecordSets.List(p.projectID, zoneID)

	err := rrsetsList.Pages(ctx, func(resp *dns.ResourceRecordSetsListResponse) error {
		for _, rrset := range resp.Rrsets {
			// Skip NS and SOA records at the zone apex
			if rrset.Type == "NS" || rrset.Type == "SOA" {
				continue
			}

			// Check if this is a TXT record with our comment pattern
			isManaged := false
			comment := ""

			if rrset.Type == "TXT" {
				for _, txtData := range rrset.Rrdatas {
					// Remove quotes from TXT data
					unquoted := strings.Trim(txtData, `"`)
					if p.commentPattern.MatchString(unquoted) {
						isManaged = true
						comment = unquoted
						break
					}
				}
			}

			// For now, we'll track all records but only manage those with our comment
			// This allows the cleanup logic to work properly
			if isManaged || rrset.Type != "TXT" {
				recordName := strings.TrimSuffix(rrset.Name, ".")
				content := ""
				if len(rrset.Rrdatas) > 0 {
					content = strings.TrimSuffix(rrset.Rrdatas[0], ".")
				}

				record := &types.DNSRecord{
					ID:      fmt.Sprintf("%s:%s:%s", zoneID, rrset.Name, rrset.Type),
					Name:    recordName,
					Type:    rrset.Type,
					Content: content,
					TTL:     int(rrset.Ttl),
					Comment: comment,
					ZoneID:  zoneID,
				}

				records[recordName] = record

				if isManaged {
					log.Debug().Msgf("[GCP Provider] Found managed record: %s (Type: %s)", recordName, rrset.Type)
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, types.NewProviderError("gcp", "Failed to get records", err)
	}

	// Filter to only return managed records
	managedRecords := make(map[string]*types.DNSRecord)
	for name, record := range records {
		if record.Comment != "" && p.commentPattern.MatchString(record.Comment) {
			managedRecords[name] = record
		}
	}

	return managedRecords, nil
}

// RefreshRecordsCache refreshes the cache of all managed DNS records
func (p *Provider) RefreshRecordsCache(zones map[string]string) (map[string]*types.DNSRecord, error) {
	newExistingRecords := make(map[string]*types.DNSRecord)

	for _, zoneID := range zones {
		zoneRecords, err := p.GetRecords(zoneID)
		if err != nil {
			return nil, err
		}
		for name, record := range zoneRecords {
			newExistingRecords[name] = record
		}
	}

	log.Info().Msgf("[GCP Provider] Refresh found %d records", len(newExistingRecords))
	return newExistingRecords, nil
}

// CleanupRecords removes old records for a service
func (p *Provider) CleanupRecords(existingRecords map[string]*types.DNSRecord, namespace, serviceName, zoneID, currentDomain string) error {
	expectedComment := "[greydns - Do not manually edit]" + namespace + "/" + serviceName

	for _, record := range existingRecords {
		if record.Comment == expectedComment && record.Name != currentDomain {
			log.Info().Msgf("[GCP Provider] [%s/%s] Found old record, cleaning up", namespace, serviceName)
			err := p.DeleteRecord(record.ID, zoneID)
			if err != nil {
				log.Error().Err(err).Msgf("[GCP Provider] [%s/%s] Failed to delete record", namespace, serviceName)
				return err
			}
			delete(existingRecords, record.Name)
		}
	}
	return nil
}

// waitForChange waits for a DNS change to complete
func (p *Provider) waitForChange(ctx context.Context, zoneID, changeID string) error {
	for {
		change, err := p.service.Changes.Get(p.projectID, zoneID, changeID).Context(ctx).Do()
		if err != nil {
			return err
		}

		if change.Status == "done" {
			return nil
		}

		// Wait a bit before checking again
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
			// Continue waiting
		}
	}
}
