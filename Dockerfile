FROM golang:1.27.0-trixie AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ceraa .

FROM debian:bookworm-slim

ARG CERAA_UID=10001
ARG CERAA_GID=10001

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid "${CERAA_GID}" ceraa \
    && useradd --uid "${CERAA_UID}" --gid "${CERAA_GID}" --home-dir /data/ceraa --create-home --shell /usr/sbin/nologin ceraa \
    && mkdir -p /workspace /data/ceraa /data/ceraa/workspace \
    && chown -R ceraa:ceraa /workspace /data/ceraa

COPY --from=builder /out/ceraa /usr/local/bin/ceraa

ENV HOME=/data/ceraa \
    XDG_CONFIG_HOME=/data/ceraa/.config \
    CERAA_SESSION_FILE=/data/ceraa/sessions \
    CERAA_WORKSPACE_ROOT=/data/ceraa/workspace \
    CERAA_MEMORY_FILE=/data/ceraa/memory \
    CERAA_SKILL_FILE=/data/ceraa/skills \
    CERAA_EVALUATION_FILE=/data/ceraa/evaluations.json \
    CERAA_SCHEDULER_FILE=/data/ceraa/scheduler.json \
    CERAA_MODEL_CATALOG_CACHE=/data/ceraa/model-catalog.json

WORKDIR /workspace
VOLUME ["/data/ceraa", "/workspace"]
USER ceraa:ceraa
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/ceraa"]
CMD ["start"]
