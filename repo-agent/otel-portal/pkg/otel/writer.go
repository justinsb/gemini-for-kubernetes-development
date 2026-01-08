package otel

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"sync"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"

	storagepb "github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/otel-portal/api/private/storage"
)

type FileWriter struct {
	w *writer
}

type writer struct {
	fileMutex sync.Mutex
	f         *os.File

	typeCodesMutex sync.Mutex
	nextTypeCode   TypeCode
	typeCodes      map[string]TypeCode
}

type TypeCode uint32

func NewFileWriter(path string) (*FileWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	w := &writer{f: f}

	w.nextTypeCode = 32
	w.typeCodes = make(map[string]TypeCode)
	w.recordWellKnownType(storagepb.WellKnownTypeCode_WellKnownTypeCode_ObjectType, &storagepb.ObjectType{})

	return &FileWriter{w: w}, nil
}

// codeForType returns the integer code value for objects of obj.
// If this is the first time we've seen the type, this method will assign a code value, write it to the file and return it.
func (w *writer) codeForType(ctx context.Context, obj proto.Message) (TypeCode, error) {
	typeName := string(obj.ProtoReflect().Descriptor().FullName())

	w.typeCodesMutex.Lock()
	defer w.typeCodesMutex.Unlock()

	typeCode, found := w.typeCodes[typeName]
	if found {
		return typeCode, nil
	}

	typeCode = w.nextTypeCode
	w.nextTypeCode++

	record := &storagepb.ObjectType{
		TypeCode: uint32(typeCode),
		TypeName: typeName,
	}
	if err := w.writeObjectWithTypeCode(ctx, TypeCode(storagepb.WellKnownTypeCode_WellKnownTypeCode_ObjectType), record); err != nil {
		return 0, err
	}

	w.typeCodes[typeName] = typeCode
	return typeCode, nil
}

// recordWellKnownType is used to insert a "system" type into the table of type codes.
// This is used for the types that are needed to e.g. record the type code table itself.
func (w *writer) recordWellKnownType(typeCode storagepb.WellKnownTypeCode, obj proto.Message) {
	typeName := string(obj.ProtoReflect().Descriptor().FullName())

	w.typeCodesMutex.Lock()
	defer w.typeCodesMutex.Unlock()

	w.typeCodes[typeName] = TypeCode(typeCode)
}

func (w *FileWriter) Write(ctx context.Context, msg proto.Message) error {
	log := klog.FromContext(ctx)

	log.Info("writing data to file")
	log.Info("message:\n" + prototext.Format(msg))

	return w.w.writeObject(ctx, msg)
}

// writeObject appends an object to the file
func (w *writer) writeObject(ctx context.Context, obj proto.Message) error {
	typeCode, err := w.codeForType(ctx, obj)
	if err != nil {
		return err
	}

	return w.writeObjectWithTypeCode(ctx, typeCode, obj)
}

// writeObjectWithTypeCode is the key function here.  We encode and write the object.
// We include a header that identifies the object using the provided typeCode.
func (w *writer) writeObjectWithTypeCode(ctx context.Context, typeCode TypeCode, obj proto.Message) error {
	buf, err := proto.Marshal(obj)
	if err != nil {
		return fmt.Errorf("converting to proto: %w", err)
	}

	crc32q := crc32.MakeTable(crc32.Castagnoli)
	checksum := crc32.Checksum(buf, crc32q)

	flags := uint32(0)

	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()

	if w.f == nil {
		return fmt.Errorf("already closed")
	}

	// write the object with a header.
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[0:4], uint32(len(buf)))
	binary.BigEndian.PutUint32(header[4:8], checksum)
	binary.BigEndian.PutUint32(header[8:12], flags)
	binary.BigEndian.PutUint32(header[12:16], uint32(typeCode))

	if _, err := w.f.Write(header); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}
	if _, err := w.f.Write(buf); err != nil {
		// TODO: Rotate file?
		return fmt.Errorf("writing body: %w", err)
	}

	return nil
}

// Close closes the output file.
func (w *writer) Close() error {
	w.fileMutex.Lock()
	defer w.fileMutex.Unlock()

	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f = nil
	}

	return nil
}

func (w *FileWriter) Close() error {
	return w.w.Close()
}
