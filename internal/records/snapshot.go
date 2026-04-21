package records

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/math280h/greydns/internal/dnsprovider"
)

// Snapshot holds the subset of controller configuration that the
// Reconciler reads on every reconcile. Held behind an atomic pointer
// in Reconciler so hot-reload swaps are lock-free for readers.
//
// Provider-scoped settings (e.g. cloudflare.proxy-enabled) and the
// provider name itself are not in here: they're baked into the Provider
// at construction, so changing them still requires a restart.
type Snapshot struct {
	RecordTTL          int
	RecordType         dnsprovider.RecordType
	IngressDestination string
	OverridePolicy     OverridePolicy
}

// ParseSnapshot validates a ConfigMap data map and returns a Snapshot
// or an error explaining which key is wrong. Callers use the error to
// reject a hot-reload attempt; the reconciler keeps its previous
// snapshot when parse fails.
func ParseSnapshot(data map[string]string, provider dnsprovider.Provider) (Snapshot, error) {
	if data == nil {
		return Snapshot{}, errors.New("config data is nil")
	}

	ingress, ok := data["ingress-destination"]
	if !ok || ingress == "" {
		return Snapshot{}, errors.New("ingress-destination is required")
	}

	ttlRaw, ok := data["record-ttl"]
	if !ok {
		return Snapshot{}, errors.New("record-ttl is required")
	}
	ttl, err := strconv.Atoi(ttlRaw)
	if err != nil || ttl <= 0 {
		return Snapshot{}, fmt.Errorf("record-ttl must be a positive integer, got %q", ttlRaw)
	}

	typeRaw, ok := data["record-type"]
	if !ok || typeRaw == "" {
		return Snapshot{}, errors.New("record-type is required")
	}
	rt := dnsprovider.RecordType(typeRaw)
	if vErr := dnsprovider.ValidateRecordType(rt, provider.SupportedRecordTypes()); vErr != nil {
		return Snapshot{}, fmt.Errorf("record-type: %w", vErr)
	}

	overrideRaw, hasOverrideKey := data["allowed-overrides"]

	return Snapshot{
		RecordTTL:          ttl,
		RecordType:         rt,
		IngressDestination: ingress,
		OverridePolicy:     NewOverridePolicy(overrideRaw, hasOverrideKey),
	}, nil
}
