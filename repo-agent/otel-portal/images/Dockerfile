# Build Stage
FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the binary statically
RUN CGO_ENABLED=0 GOOS=linux go build -o otel-portal .

# Final Stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /app/otel-portal /otel-portal

# Expose ports
EXPOSE 4317 4318

# User nonroot
USER 65532:65532

ENTRYPOINT ["/otel-portal"]
CMD ["listen", "--output-file", "/data/otel-data.bin"]
