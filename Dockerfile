FROM golang:1.27.0-trixie AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ceraa .

FROM debian:bookworm-slim

ARG CERAA_UID=10001
ARG CERAA_GID=10001

# ripgrep is here because the model reaches for it by prior, not because it was
# asked to. A real session spent two of its calls on `rg` before falling back to
# grep, and this image is what it was running in: debian-slim with only
# ca-certificates, so the binary simply was not present.
#
# The install is deliberately allowed to fail. It is a separate step from
# ca-certificates, which is required, because a package with no build for the
# target architecture would otherwise abort the whole RUN and the image would not
# build at all - a hard break for anyone on that architecture, in exchange for a
# faster search. This bot runs for several users whose machines are not this
# machine, so the image has to build everywhere.
#
# A missing ripgrep is not a broken deployment: the prompt resolves its search
# instruction at startup from what is actually installed, and names grep when
# ripgrep is absent.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && (apt-get install -y --no-install-recommends ripgrep \
        || echo "ceraa: ripgrep unavailable for this architecture; the prompt will name grep instead") \
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
