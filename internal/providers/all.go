// Package providers is the registration manifest for every DNS backend
// shipped with greydns. Each provider self-registers via its init()
// function; this file exists solely to pull those init() calls into the
// dependency graph.
//
// Adding a new provider is a two-step change and touches no core code:
//  1. Create internal/providers/<name>/<name>.go implementing dnsprovider.Provider.
//  2. Add one blank-import line below.
package providers

import (
	_ "github.com/math280h/greydns/internal/providers/cloudflare"
	_ "github.com/math280h/greydns/internal/providers/route53"
)
