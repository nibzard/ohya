package hubsync

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

const (
	DiagnosticsFormatVersion = 1

	MaxDiagnosticsResultBytes = 256 << 10

	DefaultMaxDiagnostics = 2

	DefaultDiagnosticsTimeout = 5 * time.Second

	DefaultDiagnosticsInterval = 10 * time.Second
)

var ErrRemoteStatusUnsupported = errors.New("hubsync: hub does not support remote status aggregates")

type DiagnosticsProvider interface {
	RemoteStatus(ctx context.Context) (RemoteStatus, error)
}

type DiagnosticsRequest struct {
	FormatVersion uint8
}

type RemoteIdentity struct {
	DeviceID             string         `json:"device_id,omitempty"`
	LocalIngestDeviceIDs []string       `json:"local_ingest_device_ids"`
	Version              string         `json:"version"`
	Revision             string         `json:"revision"`
	Dirty                string         `json:"dirty"`
	Go                   string         `json:"go"`
	OS                   string         `json:"os"`
	Arch                 string         `json:"arch"`
	Schema               int            `json:"schema"`
	Parsers              map[string]int `json:"parsers"`
}

type RemoteLimits struct {
	MaxConns       int   `json:"max_conns"`
	MaxBatchBytes  int64 `json:"max_batch_bytes"`
	MaxRecordBytes int64 `json:"max_record_bytes"`
	QueryTimeoutMS int64 `json:"query_timeout_ms"`
	QueryMaxRows   int   `json:"query_max_rows"`
	QueryMaxBytes  int   `json:"query_max_bytes"`
}

type RemoteStatus struct {
	FormatVersion         int                        `json:"format_version"`
	Hub                   RemoteIdentity             `json:"hub"`
	GeneratedWallMS       int64                      `json:"generated_wall_ms"`
	UptimeMS              *int64                     `json:"uptime_ms,omitempty"`
	SampledWallMS         *int64                     `json:"sampled_wall_ms,omitempty"`
	Roles                 []string                   `json:"roles,omitempty"`
	StorageFiles          map[string]int64           `json:"storage_files,omitempty"`
	StorageObservedWallMS int64                      `json:"storage_observed_wall_ms,omitempty"`
	Resources             map[string]observe.Reading `json:"resources,omitempty"`
	Limits                RemoteLimits               `json:"limits"`
}

func WriteDiagnosticsRequest(w io.Writer, formatVersion uint8) error {
	if formatVersion == 0 {
		formatVersion = DiagnosticsFormatVersion
	}
	if _, err := w.Write([]byte{markerDiagnostics, ProtocolVersion, formatVersion}); err != nil {
		return fmt.Errorf("hubsync: write diagnostics request: %w", err)
	}
	return nil
}

func WriteDiagnosticsReply(w io.Writer, status RemoteStatus) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("hubsync: encode diagnostics status: %w", err)
	}
	if len(payload) > MaxDiagnosticsResultBytes {
		return fmt.Errorf("%w: diagnostics reply is %d bytes, want at most %d",
			ErrTooLarge, len(payload), MaxDiagnosticsResultBytes)
	}
	head := []byte{markerDiagnosticsRes, ProtocolVersion}
	head = binary.BigEndian.AppendUint32(head, uint32(len(payload)))
	head = append(head, payload...)
	if _, err := w.Write(head); err != nil {
		return fmt.Errorf("hubsync: write diagnostics reply: %w", err)
	}
	return nil
}

func ReadDiagnosticsReply(r io.Reader) (RemoteStatus, *Error, error) {
	fr := &frameReader{r: r}
	marker, err := replyHead(fr)
	if err != nil {
		return RemoteStatus{}, nil, err
	}
	switch marker {
	case markerDiagnosticsRes:
		n, err := fr.u32()
		if err != nil {
			return RemoteStatus{}, nil, err
		}
		payload, err := fr.bytes(n, MaxDiagnosticsResultBytes, "diagnostics result")
		if err != nil {
			return RemoteStatus{}, nil, err
		}
		var status RemoteStatus
		if err := json.Unmarshal(payload, &status); err != nil {
			return RemoteStatus{}, nil, fmt.Errorf("hubsync: decode diagnostics result: %w", err)
		}
		return status, nil, nil
	case markerError:
		perr, err := readErrorFrame(fr)
		if err != nil {
			return RemoteStatus{}, nil, err
		}
		return RemoteStatus{}, perr, nil
	default:
		return RemoteStatus{}, nil, fmt.Errorf("hubsync: bad diagnostics reply marker %q", marker)
	}
}

func readDiagnosticsRequest(fr *frameReader) (*DiagnosticsRequest, error) {
	v, err := fr.u8()
	if err != nil {
		return nil, err
	}
	return &DiagnosticsRequest{FormatVersion: v}, nil
}
