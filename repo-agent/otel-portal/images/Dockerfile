# Build Stage
FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the binary statically
RUN CGO_ENABLED=0 GOOS=linux go build -o otel-portal .
RUN CGO_ENABLED=0 GOOS=linux go build -o otel-portal-ctl ./cmd/otel-portal-ctl

# Final Stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /app/otel-portal /otel-portal
COPY --from=builder /app/otel-portal-ctl /otel-portal-ctl

# Expose ports
EXPOSE 4317 4318

# User nonroot
USER 65532:65532

ENTRYPOINT ["/otel-portal"]
CMD ["listen", "--output-dir", "/data/otel/", "--rotate-interval", "5m"]
