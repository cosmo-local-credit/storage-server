# storage-server

![GitHub Tag](https://img.shields.io/github/v/tag/cosmo-local-credit/storage-server)

## Getting Started

### Prerequisites

* Git
* Docker

See [docker-compose.yaml](dev/docker-compose.yaml) for an example on how to run and deploy a single instance.

### 1. Build the Docker image

We provide pre-built images for `linux/amd64`. See the packages tab on Github.

If you are on any other platform:

```bash
git clone https://github.com/cosmo-local-credit/storage-server.git
cd storage-server
docker buildx build --build-arg BUILD=$(git rev-parse --short HEAD) --tag storage-server:$(git rev-parse --short HEAD) --tag storage-server:latest .
docker images
```

### 2. Run the server

```bash
cd dev
docker compose up
```

## API

### Authentication

Every `/v1` route requires a `clc-core` JWT:

```http
Authorization: Bearer <token>
```

Requirements:

| Requirement | Value |
|---|---|
| Algorithm | `EdDSA` (Ed25519) |
| Signature | Must verify against `auth.public_key` |
| `exp` | Required, must be in the future |
| `iat` | Required |
| `nbf` | Optional, enforced when present |
| Clock skew | `auth.clock_skew`, default `30s` |
| Claims | Either (`userId`, `ethereumAddress`, `role`) or (`service`, `publicKey`) |

The scheme is case insensitive, so `bearer` also works. `role` and `service` only identify which claim shape a token uses. They do not affect the result: every valid token gets the same access.

Anything wrong with the token returns `401 UNAUTHORIZED` with no further detail. The reason is recorded in the `storage_auth_total{result,reason}` metric.

### CORS

`api.origin` is matched with one `*` standing for exactly one DNS label, so
`https://*.clc-admin.pages.dev` admits every branch preview of that project while
`https://a.b.clc-admin.pages.dev` and the bare apex do not. List the apex
separately when it needs to be allowed. Echo's own matcher is a literal compare;
this behaviour comes from `newOriginMatcher` and mirrors clc-core's
`util.OriginAllowed`.

### Upload

```
POST /v1/upload
Content-Type: multipart/form-data
```

| Field | Type | Rule |
|---|---|---|
| `file` | binary | JPEG, PNG or WebP |
| `folder` | text | One of `api.allowed_folders` |
| `name` | text | `^[A-Za-z0-9_-]{1,64}$` |
| `width` | text | One of `image.allowed_widths`, exactly one value |

```bash
curl -X POST https://storage.sarafu.africa/v1/upload \
  -H "Authorization: Bearer $TOKEN" \
  -F "file=@photo.jpg" \
  -F "folder=voucher" \
  -F "name=bd10fd365101425f8bafeb6adfe8007c" \
  -F "width=800"
```

`200 OK`:

```json
{
  "ok": true,
  "payload": {
    "s3": "https://content.sarafu.network/voucher/bd10fd365101425f8bafeb6adfe8007c_800_a3f91c2e.webp",
    "width": 800,
    "height": 533,
    "byteSize": 82972,
    "contentType": "image/webp"
  }
}
```

Everything but `s3` describes the object as stored, not as requested: `width` is the
actual output width, which is the source's width when that is narrower than the
requested one. A caller can size a layout from this without decoding the result.

Any error:

```json
{
  "ok": false,
  "code": "INVALID_WIDTH"
}
```

### Object keys

```
<cdn_base_url>/<folder>/<name>_<width>_<hash>.<extension>
```

* `<width>` is the actual output width, which is `min(sourceWidth, requestedWidth)`. Sources are never upscaled, so a 200px source requested at 400 returns `_200`.
* `<hash>` is the first 8 hex characters of the SHA-256 of the stored bytes.
* `<extension>` is `webp` in most cases. See the next section.

Use the returned URL as is. Do not build it yourself, because both the width and the extension depend on the source.

Because the key contains a content hash, re-uploading the same bytes under the same name returns the same URL, and uploading different bytes under the same name returns a new URL instead of replacing the old object. Objects are stored with `Cache-Control: public, max-age=31536000, immutable`.

### Output format

One object is stored per upload. There is no variant ladder.

| Input | Output |
|---|---|
| Any image with transparency | Lossless WebP |
| Resized photo | Lossy WebP at `image.quality` |
| Logo, text, flat graphic | Lossless WebP, when it is smaller than the lossy candidate |
| WebP needing no resize | Returned unchanged |
| JPEG or PNG needing no resize | Kept as is unless WebP saves at least `1 - image.materially_smaller_ratio` |

EXIF orientation is applied to the pixels and the metadata is dropped, so portrait photos are stored upright.

### Limits

| Limit | Config key | Default |
|---|---|---|
| Request body | `api.max_body_size` | 8 MiB |
| Source pixels | `image.max_pixels` | 12,500,000 (fits 4032x3024) |
| Allowed widths | `image.allowed_widths` | 400, 800, 1280 |
| Allowed folders | `api.allowed_folders` | `voucher`, `profile` |
| Concurrent encodes | `image.normalize_concurrency` | 4 |

Requests over the body limit are rejected before the file is read. Requests over the pixel limit are rejected after reading the header but before decoding.

### Error codes

| Status | Code | Cause |
|---|---|---|
| 400 | `MISSING_FOLDER` | `folder` absent or empty |
| 400 | `MISSING_NAME` | `name` absent or empty |
| 400 | `INVALID_FOLDER` | `folder` not in `api.allowed_folders` |
| 400 | `INVALID_NAME` | `name` fails the pattern |
| 400 | `INVALID_WIDTH` | `width` absent, not a number, not allowed, or supplied more than once |
| 400 | `MISSING_FILE` | No `file` part |
| 400 | `NOT_MULTIPART` | Body is not `multipart/form-data` |
| 400 | `UNSUPPORTED_FILE_EXTENSION` | Not a readable JPEG, PNG or WebP |
| 400 | `IMAGE_TOO_LARGE` | Over `image.max_pixels` |
| 400 | `EOF` | Body ended early |
| 400 | `BAD_REQUEST` | Malformed request |
| 401 | `UNAUTHORIZED` | Missing, malformed, expired or badly signed token |
| 404 | `NOT_FOUND` | Unknown route, or a method the route does not accept |
| 405 | `METHOD_NOT_ALLOWED` | Method not routed |
| 408 | `DEADLINE_EXCEEDED` | Read timeout, see `api.upload_timeout` |
| 408 | `REQUEST_CANCELED` | Client disconnected |
| 413 | `FILE_SIZE_LIMIT_EXCEEDED` | Over `api.max_body_size` |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | Unsupported content type |
| 500 | `INTERNAL_SERVER_ERROR` | Everything else |

### CORS

Browser uploads need the origin listed in `api.origin`. Preflight allows `POST`, `GET`, `HEAD` and the `Origin`, `Content-Type`, `Accept`, `Authorization` headers.

### Metrics

```
GET /metrics
```

Prometheus text format, no authentication, served only when `metrics.enable` is true. Keep it off the public listener.

| Metric | Labels |
|---|---|
| `storage_uploads_total` | |
| `storage_upload_bytes_in_total` | |
| `storage_upload_bytes_out_total` | |
| `storage_normalize_duration_seconds` | |
| `storage_upload_width_total` | `requested`, `actual` |
| `storage_upload_format_total` | `source`, `output`, `lossless` |
| `storage_auth_total` | `result`, plus `reason` on rejections |

### Configuration

See [config.toml](config.toml). Any key can be overridden by an environment variable prefixed with `STORAGE_`, using `__` for nesting:

```bash
STORAGE_API__MAX_BODY_SIZE=16
STORAGE_IMAGE__ALLOWED_WIDTHS=400,800,1280
STORAGE_S3__SECRET_ACCESS_KEY=...
```

`auth.public_key` is the Ed25519 **public** key matching the `clc-core` signing key, in PEM form. The server refuses to start if it is missing, malformed, or a private key.

## Sample Uploader

```bash
cd uploader
python3 -m http.server -b 127.0.0.1 3000
```

## License

[AGPL-3.0](LICENSE).
