// Package sigv4 implements AWS Signature Version 4 request signing using only
// the standard library. It covers what the tool needs: header based signing
// of HTTP requests with an in-memory payload.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"awsume/internal/awscreds"
)

const (
	algorithm      = "AWS4-HMAC-SHA256"
	amzDateFormat  = "20060102T150405Z"
	dateStampFmt   = "20060102"
	headerAmzDate  = "X-Amz-Date"
	headerAmzToken = "X-Amz-Security-Token"
)

// Sign adds X-Amz-Date, X-Amz-Security-Token (when a session token is
// present) and Authorization headers to req. payload must be the exact body
// that will be sent. The signed headers are host, content-type (if set) and
// every x-amz-* header present on the request.
func Sign(req *http.Request, payload []byte, c awscreds.Credentials, region, service string, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if region == "" || service == "" {
		return fmt.Errorf("sigv4: region and service are required")
	}
	t := now.UTC()
	amzDate := t.Format(amzDateFormat)
	dateStamp := t.Format(dateStampFmt)

	req.Header.Set(headerAmzDate, amzDate)
	if c.SessionToken != "" {
		req.Header.Set(headerAmzToken, c.SessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	canonicalHeaders, signedHeaders := canonicalHeaders(req.Header, host)
	payloadHash := hashHex(payload)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	key := SigningKey(c.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, c.AccessKeyID, scope, signedHeaders, signature))
	return nil
}

// SigningKey derives the per-day signing key.
func SigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func canonicalHeaders(h http.Header, host string) (canonical, signed string) {
	values := map[string]string{"host": strings.TrimSpace(host)}
	for name, vals := range h {
		lower := strings.ToLower(name)
		if lower != "content-type" && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		trimmed := make([]string, 0, len(vals))
		for _, v := range vals {
			trimmed = append(trimmed, collapseSpaces(v))
		}
		values[lower] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(values[n])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		unescaped, err := url.PathUnescape(seg)
		if err != nil {
			unescaped = seg
		}
		segments[i] = escape(unescaped)
	}
	return strings.Join(segments, "/")
}

func canonicalQuery(q url.Values) string {
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(q))
	for k, vals := range q {
		for _, v := range vals {
			pairs = append(pairs, pair{escape(k), escape(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.k+"="+p.v)
	}
	return strings.Join(parts, "&")
}

// escape percent-encodes s according to RFC 3986 with only the unreserved
// characters left alone, which is what SigV4 requires.
func escape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}
