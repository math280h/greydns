# GreyDNS 🌐

> A lightweight Kubernetes controller for automated DNS management through service annotations

GreyDNS enables development teams to manage their DNS records directly through Kubernetes service annotations, working seamlessly with centrally managed ingress patterns.

**Disclaimer:** _GreyDNS is not meant to replace ExternalDNS for a lot of use cases. It's designed to be a solution to a specific problem that Platform Engineers may run into when trying to empower development teams to manage their own DNS records while maintaining a central point of control for ingress._

![Go Version](https://img.shields.io/badge/go-1.24-blue.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)

## 🚀 Features

- **Annotation-Driven**: Create and manage DNS records using simple Kubernetes service annotations
- **Central Ingress**: Works with centrally managed ingress controllers
- **Real-time Updates**: Automatically syncs DNS records when annotations change
- **Lightweight**: Minimal resource footprint with efficient caching

## 📦 Supported DNS Providers

GreyDNS uses a pluggable provider system. Exactly one provider is active at a time, selected via the `provider` key in the ConfigMap.

- **CloudFlare**

### Coming Soon

- **Route 53**
- **Google Cloud DNS Support**
- **Azure DNS Support**

See [`docs/adding-a-provider.md`](docs/adding-a-provider.md) for how to add a new provider in a single package.

## 📋 Prerequisites

- Kubernetes cluster (1.19+)
- `kubectl` configured to access your cluster

### CloudFlare

- CloudFlare API token with the following permissions:
  - Zone: Read
  - DNS: Edit

## 🛠️ Installation

1. Deploy GreyDNS using kubectl:

    ```sh
    kubectl apply -f https://raw.githubusercontent.com/math280h/greydns/refs/heads/main/deployment.yaml
    ```

2. Create the required ConfigMap. Generic keys live at the top level; provider-specific keys are namespaced by the provider name (e.g. `cloudflare.proxy-enabled`), so switching providers is purely a config change:

    ```yaml
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: greydns-config
      namespace: default
    data:
      provider: "cloudflare"
      record-type: "A"
      record-ttl: "60"
      cache-refresh-seconds: "60"
      ingress-destination: "YOUR_INGRESS_IP"
      # Cloudflare-specific
      cloudflare.proxy-enabled: "true"
    ```

3. Create the provider secret. The secret is always named `greydns-secret`; keys are provider-specific:

    ```sh
    # Cloudflare
    kubectl create secret generic greydns-secret \
      --from-literal=cloudflare-token=YOUR_API_TOKEN
    ```

## 📝 Usage

Add annotations to your Kubernetes service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    greydns.io/dns: "true"
    greydns.io/domain: "api.example.com"
    greydns.io/zone: "example.com"
spec:
  # ... rest of service spec
```

### Duplicate Records

GreyDNS will automatically deduplicate records based on the namespace and service name. If you create two records at the same time it's first come first serve.

GreyDNS will create an event on the service if it detects a record that is already owned by another service.

![Duplicate Record](assets/duplicate.png)

## 🔍 Configuration

### Generic keys

Universal DNS concepts live at the top level so they're not duplicated across providers.

| Config Key | Description | Required |
|------------|-------------|----------|
| `provider` | Active provider name (today: `cloudflare`) | Yes |
| `record-type` | DNS record type (`A`, `CNAME`, ...) | Yes |
| `record-ttl` | DNS record time-to-live in seconds | Yes |
| `cache-refresh-seconds` | Cache refresh interval | Yes |
| `ingress-destination` | Ingress controller IP address or hostname | Yes |

### Cloudflare keys (`provider: cloudflare`)

| Config Key | Description | Required |
|------------|-------------|----------|
| `cloudflare.proxy-enabled` | Enable Cloudflare proxy (`true`/`false`) | No (default `false`) |

Secret keys: `cloudflare-token`.

## 🤔 Why Not ExternalDNS?

While ExternalDNS is a powerful tool for DNS automation in Kubernetes, GreyDNS takes a different approach:

- Seamlessly integrates with centrally managed ingress controllers
- No CRDs required - uses native Kubernetes annotations
- Reduced complexity compared to ExternalDNS

## 💡 Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## 📄 License

This project is licensed under the MIT License - see the LICENSE file for details.

---

_Named after Greyson, my rubber duck (who happens to be a cat) 🐱_

<img src="assets/cat.JPG" width="200" height="200">
