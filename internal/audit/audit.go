// Package audit logs each outgoing connection through the forward proxy as
// structured JSON lines. The SPIFFE identity of the client pod is extracted
// from the mTLS connection state forwarded by ztunnel (HBONE tunnel).
package audit

import (
	"log/slog"
	"net"
	"time"
)

type Event struct {
	Timestamp     time.Time `json:"ts"`
	ClientAddr    string    `json:"client_addr"`
	SPIFFE        string    `json:"spiffe,omitempty"`
	Method        string    `json:"method"` // HTTP-FORWARD or CONNECT
	TargetHost    string    `json:"target_host"`
	TargetPort    uint32    `json:"target_port"`
	UpstreamProxy string    `json:"upstream_proxy"`
	Decision      string    `json:"decision"` // allow or deny
	DenyReason    string    `json:"deny_reason,omitempty"`
	Status        int       `json:"status,omitempty"`
	BytesIn       int64     `json:"bytes_in,omitempty"`
	BytesOut      int64     `json:"bytes_out,omitempty"`
	DurationMS    int64     `json:"duration_ms,omitempty"`
}

type Logger struct {
	log *slog.Logger
}

func New(log *slog.Logger) *Logger {
	return &Logger{log: log.With("component", "audit")}
}

// Log emits a structured audit event for log aggregators (Loki, Splunk, Elastic).
func (l *Logger) Log(e Event) {
	l.log.Info("egress",
		"ts", e.Timestamp.Format(time.RFC3339Nano),
		"client_addr", e.ClientAddr,
		"spiffe", e.SPIFFE,
		"method", e.Method,
		"target_host", e.TargetHost,
		"target_port", e.TargetPort,
		"upstream_proxy", e.UpstreamProxy,
		"decision", e.Decision,
		"deny_reason", e.DenyReason,
		"status", e.Status,
		"bytes_in", e.BytesIn,
		"bytes_out", e.BytesOut,
		"duration_ms", e.DurationMS,
	)
}

// HostPortFromAddr splits a net.Addr into host and port (best effort).
func HostPortFromAddr(addr net.Addr) (string, string) {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String(), ""
	}
	return host, port
}
