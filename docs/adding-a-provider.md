# Adding a DNS provider

greydns treats DNS backends as pluggable. Adding a new provider is a single-package change: implement `dnsprovider.Provider`, register a factory, and add a blank import. The controller, record cache, and handler logic stay provider-agnostic.

## 1. Create the package

Pick a short, lowercase name (this will be both the package name and the value of the `provider` ConfigMap key). Create `internal/providers/<name>/<name>.go`.

```go
package myprovider

import (
    "context"

    "github.com/math280h/greydns/internal/dnsprovider"
    "github.com/math280h/greydns/internal/dnsprovider/registry"
)

const providerName = "myprovider"

func init() {
    registry.Register(providerName, New)
}

type Provider struct { /* client, config, ... */ }

func New(cfg registry.ProviderConfig, secret map[string][]byte) (dnsprovider.Provider, error) {
    // read cfg.GetRequired("foo") -> configmap key "myprovider.foo"
    // read secret["my-token"]
    return &Provider{ /* ... */ }, nil
}

func (p *Provider) Name() string { return providerName }
// ... implement ListZones, ListOwnedRecords, CreateRecord, UpdateRecord, DeleteRecord
```

## 2. Register it

Add one blank-import line to `internal/providers/all.go`:

```go
import _ "github.com/math280h/greydns/internal/providers/myprovider"
```

### Ownership

Every managed record carries `Record.OwnerRef` in the form `<namespace>/<service>`. Your provider must persist this natively and return it on `ListOwnedRecords`. Pick whichever mechanism fits the provider:

| Provider   | Mechanism                                          |
| ---------- | -------------------------------------------------- |
| Cloudflare | the record's `Comment` field                       |

`ListOwnedRecords` **must** return only records that carry a greydns owner ref; records created out-of-band by humans or other tooling must be filtered out.

### Concurrency

`ListOwnedRecords` runs from the background cache-refresh goroutine concurrently with `Create`/`Update`/`Delete` from the informer event handlers. Protect any shared mutable state on your provider with a mutex or use a client that's already safe for concurrent use.

## 3. Configuration

Generic DNS concepts (`record-type`, `record-ttl`, `ingress-destination`) live at the top level of the ConfigMap and are read by the controller, not your provider. Your provider only needs to namespace keys that are genuinely specific to it (e.g. Cloudflare's `proxy-enabled`).

Namespace those keys under your provider name (`myprovider.foo`). The `registry.ProviderConfig` helpers do this for you:

```go
cfg.GetRequired("foo")         // reads "myprovider.foo", errors if unset
cfg.GetOptional("foo", "bar")  // reads "myprovider.foo", returns "bar" if unset
cfg.GetBool("enabled", false)  // typed bool
```

Add your keys to the "Configuration" section of `README.md`.

## 4. Tests

Use `internal/dnsprovider/fake` as a template for an in-memory Provider. Exercise the handler logic against your provider if you can run it in a test environment; otherwise, unit-test the translation between the neutral `dnsprovider.Record` and your SDK's native record type.

## 5. Verify

```sh
go build ./...
go test ./...
go vet ./...
```
