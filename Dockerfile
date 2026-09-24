FROM golang:1.27.0-trixie AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sooraa .

FROM debian:bookworm-slim

ARG SOORAA_UID=10001
ARG SOORAA_GID=10001

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid "${SOORAA_GID}" sooraa \
    && useradd --uid "${SOORAA_UID}" --gid "${SOORAA_GID}" --home-dir /data/sooraa --create-home --shell /usr/sbin/nologin sooraa \
    && mkdir -p /workspace /data/sooraa /data/sooraa/workspace \
    && chown -R sooraa:sooraa /workspace /data/sooraa

COPY --from=builder /out/sooraa /usr/local/bin/sooraa

ENV HOME=/data/sooraa \
    XDG_CONFIG_HOME=/data/sooraa/.config \
    SOORAA_SESSION_FILE=/data/sooraa/sessions \
    SOORAA_WORKSPACE_ROOT=/data/sooraa/workspace \
    SOORAA_MEMORY_FILE=/data/sooraa/memory \
    SOORAA_SKILL_FILE=/data/sooraa/skills \
    SOORAA_EVALUATION_FILE=/data/sooraa/evaluations.json \
    SOORAA_SCHEDULER_FILE=/data/sooraa/scheduler.json \
    SOORAA_MODEL_CATALOG_CACHE=/data/sooraa/model-catalog.json

WORKDIR /workspace
VOLUME ["/data/sooraa", "/workspace"]
USER sooraa:sooraa
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/sooraa"]
CMD ["start"]
