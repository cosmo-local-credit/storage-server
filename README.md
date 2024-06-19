# storage-server

![GitHub Tag](https://img.shields.io/github/v/tag/grassrootseconomics/storage-server)

## Getting Started

### Prerequisites

* Git
* Docker

See [docker-compose.yaml](dev/docker-compose.yaml) for an example on how to run and deploy a single instance.

### 1. Build the Docker image

We provide pre-built images for `linux/amd64`. See the packages tab on Github.

If you are on any other platform:

```bash
git clone https://github.com/grassrootseconomics/storage-server.git
cd storage-server
docker buildx build --build-arg BUILD=$(git rev-parse --short HEAD) --tag storage-server:$(git rev-parse --short HEAD) --tag storage-server:latest .
docker images
```

### 2. Run the server

```bash
cd dev
docker compose up
```

## Sample Uploader

```bash
cd uploader
python3 -m http.server -b 127.0.0.1 3000
```

## License

[AGPL-3.0](LICENSE).