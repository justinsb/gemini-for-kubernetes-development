package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/otel-portal/pkg/otel"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

type OtelDumpOptions struct {
	SourceFile string
}

func (o *OtelDumpOptions) InitDefaults() {
	if o.SourceFile == "" {
		o.SourceFile = "otel-data.bin"
	}
}

func RunOtelDump(ctx context.Context, opt OtelDumpOptions) error {
	w := os.Stdout

	r, err := otel.NewTraceFileReader(ctx, opt.SourceFile)
	if err != nil {
		return fmt.Errorf("failed to create file reader: %w", err)
	}
	defer r.Close()

	if err := r.VisitAll(ctx, func(msg proto.Message) error {
		typeName := msg.ProtoReflect().Type().Descriptor().FullName()
		switch typeName {
		case "opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest":
			fmt.Fprintf(w, "----- ExportTraceServiceRequest -----\n")
			fmt.Fprintf(w, "MESSAGE: %v\n", prototext.Format(msg))
		case "opentelemetry.proto.collector.logs.v1.ExportLogsServiceRequest":
			fmt.Fprintf(w, "----- ExportLogsServiceRequest -----\n")
			fmt.Fprintf(w, "MESSAGE: %v\n", prototext.Format(msg))
		case "opentelemetry.proto.collector.metrics.v1.ExportMetricsServiceRequest":
		// 	fmt.Fprintf(w, "----- ExportMetricsServiceRequest -----\n")
		// 	fmt.Fprintf(w, "MESSAGE: %v\n", prototext.Format(msg))
		default:
			fmt.Fprintf(w, "Unknown message type: %s\n", typeName)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to read spans: %w", err)
	}

	return nil
}

func BuildOtelDumpCommand() *cobra.Command {
	opt := OtelDumpOptions{}
	cmd := &cobra.Command{
		Use:   "dump",
		Short: "Dump OpenTelemetry data from a file",
		RunE: func(cmd *cobra.Command, args []string) error {
			// InitDefaults should have been called before binding flags if we wanted default values in help,
			// but here we can just ensure defaults are set if 0/empty.
			opt.InitDefaults()
			return RunOtelDump(cmd.Context(), opt)
		},
	}

	cmd.Flags().StringVar(&opt.SourceFile, "source-file", "otel-data.bin", "Path to source file")

	return cmd
}
