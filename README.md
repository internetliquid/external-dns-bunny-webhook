# ExternalDNS - Bunny Webhook Provider

ExternalDNS is a Kubernetes add-on for automatically managing Domain Name System (DNS) records for
Kubernetes services by using different DNS providers. By default, Kubernetes manages DNS records
internally, but ExternalDNS takes this functionality a step further by delegating the management of
DNS records to an external DNS provider such as this one.

This repository contains a provider that implements an ExternalDNS webhook provider for [Bunny.net](https://bunny.net).

## This fork

This is Internet Liquid's fork of
[contaimlabs/external-dns-bunny-webhook](https://github.com/contaimlabs/external-dns-bunny-webhook).

**Image:** `ghcr.io/internetliquid/external-dns-bunny-webhook`, public, `linux/amd64` only. Our
versions start at `v0.100.0`, so none of them can be mistaken for an upstream release (upstream's
last is `v0.3.1`, published under `ghcr.io/contaimlabs`). Each release is built from `develop` by
release-please and GoReleaser, and `main` is moved to it.

**How a record finds its zone.** `BUNNY_INCLUDE_DOMAINS` is a comma-separated list of the zones the
webhook may write to. Create, update and delete all choose a zone the same way:

- A name belongs to a zone only if it is the zone itself or ends with a dot followed by the zone, so
  `joe.apps.98dollarwebsite.com` never lands in a zone called `website.com`.
- If more than one zone fits, the longest one wins: `apps.98dollarwebsite.com` beats
  `98dollarwebsite.com`.
- Only zones on the list are considered. With the list unset, every zone in the Bunny account is.
- Upper case and a trailing dot make no difference.
- If no zone fits, or a listed zone is not in the Bunny account, the change fails with an error
  naming it and nothing is written; external-dns tries again on its next pass.
- A record for the zone's own name (the apex) is stored in Bunny with an empty name and read back
  under the zone's name without churn, but apex routes are not supported: external-dns files the
  apex's ownership record outside the zone (`a-apps.98dollarwebsite.com` for
  `apps.98dollarwebsite.com`), where it is refused unless a parent zone is a candidate.

**Refusing to start.** If `BUNNY_INCLUDE_DOMAINS` is set but names no zone (empty, or only commas,
spaces and entries that are just `.`), the webhook exits at start with an error instead of writing
to every zone. Leaving it unset keeps upstream's every-zone behaviour.

**Upstream.** The zone matching, the apex read-back and `AdjustEndpoints` filling in Bunny's record
defaults are offered back to upstream; the start refusal and the release pipeline are ours alone.
The settings keep upstream's names, so moving back to upstream's image is an image change only.

## Important

This provider is not officially supported by [Bunny.net](https://bunny.net), but is maintained by the team at Contaim Labs
for the community. If you encounter any issues, please open an issue on this repository. If you have any questions
about Bunny.net, please reach out to their support team.

## Deployment

You can deploy the provider using any Kubernetes deployment method, such as Helm or kubectl. Examples for the official
external-dns Helm chart are provided below.

### External DNS Helm Chart

The default configuration is designed to work seamlessly with the official ExternalDNS Helm chart. Be sure to create the
`external-dns-bunny-secret` secret with the `api-key` key containing your Bunny.net API key or modify the configuration
to use a different secret or method of providing the API key.

The values file should look similar to the following:

```yaml
namespace: external-dns
provider:
  name: webhook
  webhook:
    image:
      repository: ghcr.io/internetliquid/external-dns-bunny-webhook
      tag: v0.100.0
    env:
      - name: BUNNY_API_KEY
        valueFrom:
          secretKeyRef:
            name: external-dns-bunny-secret
            key: api-key
```

To deploy the provider using the Helm chart, add the repository. You can skip this step if you already have the
repository added.

```shell
helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
```

Once the repository is added, install the chart with the values file.

```shell
helm upgrade --install external-dns external-dns/external-dns --version 1.15.0
```

Additional configuration options are available below and may be set using environment variables.

## Configuration

The provider can be configured using the following environment variables:

| Environment Variable | Required | Description | Default |
|----------------------|----------|-------------|---------|
| `BUNNY_API_KEY` | Yes | The API key used to authenticate with the Bunny.net API. | |
| `BUNNY_INCLUDE_DOMAINS` | No | Comma-separated zones the webhook may write to; see [How a record finds its zone](#this-fork). | every zone |
| `BUNNY_DRY_RUN` | No | If set to `true`, the provider will not make any changes to the DNS records. | `false` |
| `WEBHOOK_HOST` | No | The host to use for the webhook endpoint. | `localhost` |
| `WEBHOOK_PORT` | No | The port to use for the webhook endpoint. | `8888` |
| `WEBHOOK_READ_TIMEOUT` | No | The read timeout for the webhook endpoint. | `60s` |
| `WEBHOOK_WRITE_TIMEOUT` | No | The write timeout for the webhook endpoint. | `60s` |
| `HEALTH_HOST` | No | The host to use for the health endpoint. | `0.0.0.0` |
| `HEALTH_PORT` | No | The port to use for the health endpoint. | `8080` |
| `HEALTH_READ_TIMEOUT` | No | The read timeout for the health endpoint. | `60s` |
| `HEALTH_WRITE_TIMEOUT` | No | The write timeout for the health endpoint. | `60s` |

## Provider-Specific Annotations

The following annotations may be added to sources to control behavior of the DNS records created by this provider:

### `external-dns.alpha.kubernetes.io/webhook-bunny-disabled`

If set to `true`, the DNS record will be managed but set to disabled in the Bunny API. This annotation is optional
and will default to `false` if not provided. Disabling a record will cause it to not respond to DNS queries,
but will still be managed by the provider and visible in the Bunny.net dashboard.

### `external-dns.alpha.kubernetes.io/webhook-bunny-monitor-type`

The monitor type to use for the DNS record. Valid values are `none` (default), `http`, and `ping`. This
annotation is optional and will default to `none` if not provided, which will create a standard DNS record
without any monitoring.

### `external-dns.alpha.kubernetes.io/webhook-bunny-weight`

The weight to use for the DNS record. Valid values are between 1 and 100. This annotation is optional and will
default to `100` if not provided. Any value outside of the valid range will be set to the nearest valid value,
and any non-integer value will result in the default value being used.

### Additional Annotations

The following additional annotations are being considered for future releases:

#### Smart DNS Records

Smart DNS records are a feature of Bunny.net that allow you to create DNS records that route traffic based on
latency or geographic location. These annotations are not yet implemented, but are planned for a future release.
We would like to hear from you if you are interested in this feature.

##### `external-dns.alpha.kubernetes.io/webhook-bunny-smart-type`

The type of smart DNS record to create. Valid values are `none`, `latency`, and `geo`. This annotation is optional
and will default to `none` if not provided.

##### `external-dns.alpha.kubernetes.io/webhook-bunny-smart-latency-zone`

The latency zone to use for the smart DNS record. This annotation is required if the `smart-type` is set to `latency`
and must be a valid Bunny.net latency zone.

##### `external-dns.alpha.kubernetes.io/webhook-bunny-smart-geo-lat`

The latitude to use for the smart DNS record. This annotation is required if the `smart-type` is set to `geo` and
must be a valid latitude value.

##### `external-dns.alpha.kubernetes.io/webhook-bunny-smart-geo-long`

The longitude to use for the smart DNS record. This annotation is required if the `smart-type` is set to `geo` and
must be a valid longitude value.

##### `external-dns.alpha.kubernetes.io/webhook-bunny-smart-geo-preset`

A list of preset lat/lng for common Cloud Providers and their regions will be maintained in the future. This annotation
will allow you to specify a preset to use for the smart DNS record. This annotation will be optional and will be mutually
exclusive with the `geo-lat` and `geo-long` annotations.

An example for this annotation might be:

```yaml
annotations:
  external-dns.alpha.kubernetes.io/webhook-bunny-smart-type: "geo"
  external-dns.alpha.kubernetes.io/webhook-bunny-smart-geo-preset: "aws:us-east-1"
```

## Development

A development environment can be set up using [Tilt](https://tilt.dev) by running the following command:

```shell
tilt up
```

This will start the development environment and open a browser window with the Tilt dashboard. The provider will
automatically reload when changes are made to the source code.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
