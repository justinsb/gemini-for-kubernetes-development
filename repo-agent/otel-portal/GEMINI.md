This is otel-portal, an in-cluster proxy for OpenTelemetry (OTEL) data over http or grpc.

Currently it listens for opentelemetry HTTP or GRPC traffic, and appends it to a local file.  Over time we will periodically upload this file to GCS (or S3 etc).
