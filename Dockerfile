FROM golang:1.27.0-trixie AS builder

WORKDIR /workspace

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary
RUN CGO_ENABLED=0 go build -trimpath -o /bin/sooraa .

# ---- Runtime stage ----
FROM scratch

COPY --from=builder /bin/sooraa /sooraa

ENTRYPOINT ["/sooraa"]
