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

## 📦 DNS Providers

GreyDNS uses a modular provider architecture that makes it easy to support multiple DNS services.

### Currently Supported

- **CloudFlare**: Full support with proxy features, A/CNAME records, and automatic cleanup
- **Google Cloud DNS**: Full support with A/CNAME/AAAA/TXT records, managed zones, and automatic cleanup

### Provider Configuration

Configure your DNS provider in the ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: greydns-config
  namespace: default
data:
  provider: "cloudflare"  # Options: "cloudflare", "gcp", "google"
  record-ttl: "60"
  record-type: "A"
  cache-refresh-seconds: "60"
  ingress-destination: "YOUR_INGRESS_IP"
  proxy-enabled: "true"  # CloudFlare only
```

If no `provider` is specified, it defaults to CloudFlare for backward compatibility.

## 📋 Prerequisites

- Kubernetes cluster (1.19+)
- `kubectl` configured to access your cluster

### CloudFlare Setup

- CloudFlare API token with the following permissions:
  - Zone: Read
  - DNS: Edit

Create the CloudFlare API token secret:

```sh
kubectl create secret generic greydns-secret \
  --from-literal=cloudflare=YOUR_API_TOKEN
```

### Google Cloud DNS Setup

- GCP project with Cloud DNS API enabled
- Service account with the following IAM role:
  - `roles/dns.admin` (DNS Administrator)

Create a service account and download the JSON key:

```sh
# Create service account
gcloud iam service-accounts create greydns-sa \
  --display-name="GreyDNS Service Account"

# Grant DNS admin role
gcloud projects add-iam-policy-binding YOUR_PROJECT_ID \
  --member="serviceAccount:greydns-sa@YOUR_PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/dns.admin"

# Create and download key
gcloud iam service-accounts keys create ~/greydns-key.json \
  --iam-account=greydns-sa@YOUR_PROJECT_ID.iam.gserviceaccount.com
```

Create the GCP credentials secret:

```sh
kubectl create secret generic greydns-secret \
  --from-literal=gcp-project-id=YOUR_PROJECT_ID \
  --from-file=gcp-service-account=~/greydns-key.json
```

## 🛠️ Installation

1. Deploy GreyDNS using kubectl:

    ```sh
    kubectl apply -f https://raw.githubusercontent.com/math280h/greydns/refs/heads/main/deployment.yaml
    ```

2. Create the required ConfigMap:

    ```yaml
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: greydns-config
      namespace: default
    data:
      provider: "cloudflare"  # Options: "cloudflare", "gcp", "google"
      record-ttl: "60"
      record-type: "A"
      cache-refresh-seconds: "60"
      ingress-destination: "YOUR_INGRESS_IP"
      proxy-enabled: "true"  # CloudFlare only
    ```

3. Create your DNS provider credentials secret:

    **For CloudFlare:**
    ```sh
    kubectl create secret generic greydns-secret \
      --from-literal=cloudflare=YOUR_API_TOKEN
    ```

    **For Google Cloud DNS:**
    ```sh
    kubectl create secret generic greydns-secret \
      --from-literal=gcp-project-id=YOUR_PROJECT_ID \
      --from-file=gcp-service-account=~/greydns-key.json
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
    greydns.io/zone: "example.com"  # For CloudFlare: zone name; For GCP: managed zone name
spec:
  # ... rest of service spec
```

**Note for GCP users:** The `greydns.io/zone` annotation should match the DNS name of your managed zone (e.g., `example.com`), not the managed zone ID.

### Duplicate Records

GreyDNS will automatically deduplicate records based on the namespace and service name. If you create two records at the same time it's first come first serve.

GreyDNS will create an event on the service if it detects a record that is already owned by another service.

![Duplicate Record](assets/duplicate.png)

## 🔍 Configuration

| Config Key | Description | Required |
|------------|-------------|---------|
| provider | DNS provider to use (default: cloudflare) | False |
| record-ttl | DNS record time-to-live in seconds | True |
| record-type | DNS record type (A or CNAME) | True |
| proxy-enabled | Enable CloudFlare proxy | True |
| cache-refresh-seconds | Cache refresh interval | True |
| ingress-destination | Ingress controller IP address | True |

## 🔧 Adding New DNS Providers

GreyDNS uses a modular architecture that makes adding new DNS providers straightforward:

1. **Create a provider package** under `internal/providers/yourprovider/`
2. **Implement the Provider interface** with methods like `Connect()`, `CreateRecord()`, `UpdateRecord()`, etc.
3. **Register the provider** in the provider manager
4. **Add credentials** to the secret with the appropriate key

The system uses generic DNS record types that work across all providers, making the core logic provider-agnostic.

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
