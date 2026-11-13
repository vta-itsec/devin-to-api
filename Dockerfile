# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
COPY devinproto ./devinproto
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# The generated protobuf file is ~12MB of Go source; cap compiler memory so
# builds do not OOM on small hosts. Override with --build-arg GOMEMLIMIT=8GiB.
ARG GOMEMLIMIT=2GiB
RUN CGO_ENABLED=0 GOMEMLIMIT=${GOMEMLIMIT} GOGC=50 \
    go build -p 2 -trimpath -ldflags="-s -w" \
    -o /out/devin-to-api ./cmd/devin-to-api

# ---- runtime stage ----
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl bash xz-utils \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /bin/bash devin
USER devin
WORKDIR /home/devin

# Devin CLI inside the image → `docker exec -it <container> devin auth login`
# writes ~/.local/share/devin/credentials.toml, which the proxy picks up live.
# The install script ends by running interactive `devin setup`, which exits
# non-zero without a TTY — tolerate it and verify the binary directly.
RUN curl -fsSL https://cli.devin.ai/install.sh | bash; \
    /home/devin/.local/bin/devin version

ENV PATH="/home/devin/.local/bin:${PATH}" \
    DEVIN_CREDENTIALS_FILE="/home/devin/.local/share/devin/credentials.toml" \
    DEVIN_TO_API_LISTEN=":8080"

COPY --from=build /out/devin-to-api /usr/local/bin/devin-to-api
COPY docker/config.yaml /etc/devin-to-api/config.yaml

EXPOSE 8080
ENTRYPOINT ["devin-to-api"]
CMD ["-config", "/etc/devin-to-api/config.yaml"]
