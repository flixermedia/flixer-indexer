package tasklog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var spanSequence atomic.Uint64

func taskTraceID(queue, id string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("flixer-indexer-task:%d:%s:%s", len(queue), queue, id)))
	idBytes := digest[:16]
	if bytes.Equal(idBytes, make([]byte, 16)) {
		idBytes[15] = 1
	}
	return hex.EncodeToString(idBytes)
}

// Traceparent returns a W3C trace header for an Indexer task request.
func Traceparent(ctx context.Context) string {
	ref, ok := ctx.Value(ctxKey{}).(taskRef)
	if !ok {
		return ""
	}
	var nonce [16]byte
	binary.BigEndian.PutUint64(nonce[:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(nonce[8:], spanSequence.Add(1))
	digest := sha256.Sum256(append([]byte(ref.trace), nonce[:]...))
	span := digest[:8]
	if bytes.Equal(span, make([]byte, 8)) {
		span[7] = 1
	}
	return "00-" + ref.trace + "-" + hex.EncodeToString(span) + "-01"
}
