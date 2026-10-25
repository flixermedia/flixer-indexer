// Package auth signs and verifies requests between flixer and the indexer.
//
// A request carries X-Indexer-Timestamp (unix seconds) and X-Indexer-Signature,
// the hex HMAC-SHA256 of
//
//	flixer-indexer:v1:<timestamp>:<METHOD>:<path>:<hex sha256 of body>
//
// keyed with the shared APP_SECRET. The same scheme is used in both
// directions, so the secret never travels and a captured request cannot be
// replayed against another path or with another body.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	HeaderTimestamp = "X-Indexer-Timestamp"
	HeaderSignature = "X-Indexer-Signature"
	// MaxSkew is how far a timestamp may be from the verifier's clock.
	MaxSkew = 5 * time.Minute
)

var ErrUnauthorized = errors.New("invalid or missing request signature")

// Signature computes the signature for a request.
func Signature(secret []byte, timestamp, method, path string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("flixer-indexer:v1:" + timestamp + ":" + method + ":" + path + ":" + hex.EncodeToString(sum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

// Sign adds the authentication headers to req.
func Sign(secret []byte, req *http.Request, body []byte, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, Signature(secret, timestamp, req.Method, req.URL.Path, body))
}

// Verify checks the authentication headers of req against body.
func Verify(secret []byte, req *http.Request, body []byte, now time.Time) error {
	timestamp := req.Header.Get(HeaderTimestamp)
	signature := req.Header.Get(HeaderSignature)
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || signature == "" {
		return ErrUnauthorized
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew < -MaxSkew || skew > MaxSkew {
		return ErrUnauthorized
	}
	expected := Signature(secret, timestamp, req.Method, req.URL.Path, body)
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return ErrUnauthorized
	}
	return nil
}
