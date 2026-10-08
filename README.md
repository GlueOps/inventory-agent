# inventory-agent

A small, read-only Kubernetes collector for GlueOps captains. Each run (a
CronJob every 15 minutes, rendered by the `glueops-platform` chart) collects
three datasets and POSTs them as one gzipped JSON snapshot to an ingest
endpoint, logs one structured summary line, and exits 0 no matter what.

| Dataset         | Source                                                        | Contents                                                                 |
|-----------------|---------------------------------------------------------------|--------------------------------------------------------------------------|
| `cluster`       | `GET /version`, `list nodes`                                  | Kubernetes version/platform; per node `status.nodeInfo` version fields   |
| `helm_releases` | Helm release Secrets in `HELM_NAMESPACE` (label `owner=helm`) | Nine metadata fields per release, latest revision only                   |
| `pod_images`    | `list pods` in each `POD_NAMESPACES` entry                    | One row per init/app container: owner, derived workload, image, digest   |

It never collects values, manifests, hooks, descriptions, env vars, args,
volumes or any other pod spec field. The payload types are closed Go structs
(`internal/schema`) mirrored by [`schema/payload.schema.json`](schema/payload.schema.json)
with `additionalProperties: false` at every level; tests keep both in sync.

## Configuration

All configuration is via environment variables.

| Variable                         | Default                    | Description                                                                                   |
|----------------------------------|----------------------------|-----------------------------------------------------------------------------------------------|
| `CAPTAIN_DOMAIN`                 | (required)                 | Cluster identity and join key. Rejected if it contains `placeholder`.                        |
| `INGEST_URL`                     | `""`                       | Receiver URL. Must be `https://`. Empty: log `no_endpoint_configured`, exit 0.               |
| `POD_NAMESPACES`                 | `kube-system,glueops-core` | Comma-separated namespaces to list pods in. The chart supplies the full list.                |
| `HELM_NAMESPACE`                 | `glueops-core`             | Namespace whose Helm release Secrets are read.                                               |
| `GLUEOPS_PLATFORM_CHART_VERSION` | `""`                       | Declared chart version, sent as `platform_chart_version`.                                    |
| `DEV_MODE`                       | `false`                    | Allows `http://` for `INGEST_URL` (local/k3d only).                                          |
| `LOG_LEVEL`                      | `info`                     | `debug`, `info`, `warn` or `error`.                                                          |
| `MAX_POD_ROWS`                   | `5000`                     | Row cap for `pod_images`; beyond it the section is cut and `truncated: true`.                |
| `MAX_GZIP_BYTES`                 | `2097152`                  | Size cap for the gzipped snapshot; `pod_images` rows are dropped until it fits.              |
| `HTTP_TIMEOUT`                   | `10s`                      | Per-attempt timeout for the POST.                                                            |
| `RETRIES`                        | `2`                        | Extra attempts after the first POST (network errors, 429 and 5xx only).                      |
| `KUBECONFIG`                     | `~/.kube/config`           | Used only when not running in-cluster.                                                       |

There is no authentication in v1. `send.Options.BearerToken` is the reserved
slot for a later token; nothing sets it today and a test asserts no
`Authorization` header is sent.

## Payload

One envelope per run. Run-level fields (`captain_domain`, `cluster_uid`,
`run_id`, `collected_at`, `collector_version`, `platform_chart_version`) sit
at the top; each dataset under `datasets` carries its own `schema_version`,
`status` (`ok`|`error`), optional `error` code (`rbac_denied`,
`api_unavailable`, `decode_failed`, `internal_error`) and `data`, which is
`null` on error and `[]` when there is simply nothing there. One dataset
failing never drops the others. All timestamps are RFC 3339 UTC with
millisecond precision (`2026-10-01T05:00:00.123Z`).

The full schema is [`schema/payload.schema.json`](schema/payload.schema.json).
A sample envelope lives in `internal/schema/schema_test.go` (`SampleEnvelope`).

Headers sent: `Content-Type: application/json`, `Content-Encoding: gzip`,
`User-Agent: inventory-agent/<version>`.

## Logging

JSON lines on stdout. Failures carry a fixed `reason` code
(`send_failed`, `no_endpoint_configured`, `invalid_ingest_url`,
`invalid_config`, `payload_truncated`, `cluster_uid_unavailable`,
`kube_client_unavailable`, `internal_error`, plus the section codes above).
Every run ends with exactly one `run summary` line carrying `run_id`,
`status`, `bytes_sent`, `http_status`, `attempts` and the per-section status.

## Running locally

Against the current kubeconfig context, with a throwaway local receiver:

```sh
# terminal 1: a receiver that just prints what it gets
python3 -c 'import http.server,gzip,sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body=self.rfile.read(int(self.headers["Content-Length"]))
        print(gzip.decompress(body).decode()); self.send_response(200); self.end_headers()
http.server.HTTPServer(("127.0.0.1",8080),H).serve_forever()'

# terminal 2
export CAPTAIN_DOMAIN=nonprod.local.onglueops.com
export INGEST_URL=http://127.0.0.1:8080/ingest
export DEV_MODE=true
export POD_NAMESPACES=kube-system,glueops-core
go run .
```

The run exits 0 even without a cluster or receiver; check the summary line.

## Building

```sh
go build -trimpath -ldflags="-s -w -X main.version=v0.1.0" -o inventory-agent .
docker build --build-arg VERSION=v0.1.0 -t inventory-agent:dev .
```

Tests need no cluster (they use the client-go fake clientset):

```sh
gofmt -l . && go vet ./... && go test ./...
```

Releases are cut by release-please; a `v*` tag publishes the image to GHCR.
