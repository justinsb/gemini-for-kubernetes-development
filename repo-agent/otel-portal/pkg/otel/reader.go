package otel

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	storagepb "github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/otel-portal/api/private/storage"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	v1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"
)

type TraceFile struct {
	// traces  []*coltracepb.ExportTraceServiceRequest
	// metrics []*colmetricspb.ExportMetricsServiceRequest
	// logs    []*collogspb.ExportLogsServiceRequest
	messages []proto.Message
}

type FilterOptions struct {
	ServiceName string
}

func (f *TraceFile) VisitSpans(ctx context.Context, opt FilterOptions, callback func(string, *v1.ResourceSpans) error) error {
	for _, msg := range f.messages {
		data, ok := msg.(*coltracepb.ExportTraceServiceRequest)
		if !ok {
			continue
		}

		// klog.Infof("data %v", prototext.Format(data))
		for _, span := range data.ResourceSpans {
			serviceName := ""
			for _, attr := range span.GetResource().GetAttributes() {
				if attr.GetKey() == "service.name" {
					serviceName = attr.GetValue().GetStringValue()
					break
				}
			}

			if opt.ServiceName != "" && serviceName != opt.ServiceName {
				continue
			}

			if err := callback(serviceName, span); err != nil {
				return err
			}

		}
	}
	return nil
}

func (f *TraceFile) VisitAll(ctx context.Context, callback func(proto.Message) error) error {
	for _, msg := range f.messages {
		if err := callback(msg); err != nil {
			return err
		}
	}
	return nil
}

func NewTraceFileReader(ctx context.Context, p string) (*TraceFile, error) {
	out := &TraceFile{}

	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", p, err)
	}

	var filePaths []string
	if info.IsDir() {
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, fmt.Errorf("read dir %s: %w", p, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				filePaths = append(filePaths, filepath.Join(p, e.Name()))
			}
		}
	} else {
		filePaths = []string{p}
	}

	for _, fp := range filePaths {
		if err := out.readFile(ctx, fp); err != nil {
			return nil, fmt.Errorf("reading file %s: %w", fp, err)
		}
	}

	return out, nil
}

func (out *TraceFile) readFile(ctx context.Context, p string) error {
	log := klog.FromContext(ctx)

	log.Info("reading file", "path", p)
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("reading %v: %w", p, err)
	}

	r := bytes.NewReader(b)

	typeCodes := make(map[uint32]*storagepb.ObjectType)

	crc32q := crc32.MakeTable(crc32.Castagnoli)

	for {
		header := make([]byte, 16)
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("reading header: %w", err)
		}

		payloadLength := binary.BigEndian.Uint32(header[0:4])
		checksum := binary.BigEndian.Uint32(header[4:8])
		flags := binary.BigEndian.Uint32(header[8:12])
		typeCode := binary.BigEndian.Uint32(header[12:16])

		if flags != 0 {
			return fmt.Errorf("unexpected flags value %v", flags)
		}

		// TODO: Sanity-check payloadLength

		payload := make([]byte, payloadLength)
		if _, err := io.ReadFull(r, payload); err != nil {
			return fmt.Errorf("reading payload: %w", err)
		}

		// Verify checksum
		actualChecksum := crc32.Checksum(payload, crc32q)
		if actualChecksum != checksum {
			return fmt.Errorf("checksum mismatch: expected %x, got %x", checksum, actualChecksum)
		}

		// TODO: Better typeCode parsing
		if typeCode == 1 {
			// TODO: process type definitions
			defs := &storagepb.ObjectType{}
			if err := proto.Unmarshal(payload, defs); err != nil {
				return fmt.Errorf("parsing ObjectTypeDefinitions: %w", err)
			}
			log.V(0).Info("read type definitions", "defs", defs)
			typeCodes[defs.TypeCode] = defs
		} else {
			typeInfo := typeCodes[typeCode]
			if typeInfo == nil {
				return fmt.Errorf("unknown type code %d", typeCode)
			}

			typeName := typeInfo.TypeName
			switch typeName {

			case "opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest":
				// handled below
				obj := &coltracepb.ExportTraceServiceRequest{}
				if err := proto.Unmarshal(payload, obj); err != nil {
					return fmt.Errorf("parsing ExportTraceServiceRequest: %w", err)
				}
				out.messages = append(out.messages, obj)

			case "opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest":
				// handled below
				obj := &colmetricspb.ExportMetricsServiceRequest{}
				if err := proto.Unmarshal(payload, obj); err != nil {
					return fmt.Errorf("parsing ExportMetricsServiceRequest: %w", err)
				}
				out.messages = append(out.messages, obj)

			case "opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest":
				// handled below
				obj := &collogspb.ExportLogsServiceRequest{}
				if err := proto.Unmarshal(payload, obj); err != nil {
					return fmt.Errorf("parsing ExportLogsServiceRequest: %w", err)
				}
				out.messages = append(out.messages, obj)

			default:
				log.Info("unknown type name", "name", typeName)
			}
		}
	}

	return nil
}

func (f *TraceFile) Close() error {
	// No resources to clean up currently
	return nil
}
