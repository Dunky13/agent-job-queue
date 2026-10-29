package notaryapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// Use real TLS, ALPN, HTTP/2 frames and the standard http.Transport. An HTTP/1
// disconnect or a mock RoundTripper cannot exercise Go's HTTP/2 replay loop.
func resetServer(t *testing.T, resetMethod string, resetCode http2.ErrCode, goAway bool, hash string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var gets, posts atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP/%d fallback", r.ProtoMajor)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	server.EnableHTTP2 = true
	server.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) {
			if conn.ConnectionState().NegotiatedProtocol != "h2" {
				t.Error("TLS did not negotiate h2")
				return
			}
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Error(err)
				return
			}
			preface := make([]byte, len(http2.ClientPreface))
			if _, err := io.ReadFull(conn, preface); err != nil {
				return
			}
			if string(preface) != http2.ClientPreface {
				t.Error("invalid HTTP/2 preface")
				return
			}
			framer := http2.NewFramer(conn, conn)
			framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
			if err := framer.WriteSettings(); err != nil {
				return
			}
			methods := map[uint32]string{}
			bodies := map[uint32][]byte{}
			respond := func(stream uint32, status, body string) error {
				var block bytes.Buffer
				encoder := hpack.NewEncoder(&block)
				if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: status}); err != nil {
					return err
				}
				if err := encoder.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/json"}); err != nil {
					return err
				}
				if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: stream, BlockFragment: block.Bytes(), EndHeaders: true}); err != nil {
					return err
				}
				return framer.WriteData(stream, true, []byte(body))
			}
			complete := func(stream uint32) error {
				method := methods[stream]
				var ordinal int32
				switch method {
				case http.MethodGet:
					ordinal = gets.Add(1)
				case http.MethodPost:
					ordinal = posts.Add(1)
					var request struct {
						Name string `json:"submissionName"`
						Hash string `json:"sha256"`
					}
					if err := json.Unmarshal(bodies[stream], &request); err != nil || request.Name != "Agent-Queue-0.1.0-universal.zip" || request.Hash != hash {
						t.Errorf("invalid or incomplete create body: %q (%v)", bodies[stream], err)
						return fmt.Errorf("invalid create body")
					}
				default:
					t.Errorf("unexpected HTTP/2 method %q", method)
					return fmt.Errorf("unexpected method")
				}
				delete(methods, stream)
				delete(bodies, stream)
				if method == resetMethod && ordinal == 1 {
					// Reset only after receiving the whole create body. The simulated provider
					// has observed the creation; replaying it can therefore create another ID.
					if goAway {
						return framer.WriteGoAway(stream-2, http2.ErrCodeNo, nil)
					}
					return framer.WriteRSTStream(stream, resetCode)
				}
				if method == http.MethodGet {
					return respond(stream, "200", `{"data":[]}`)
				}
				return respond(stream, "201", createResponse(goodAttrs))
			}
			for {
				frame, err := framer.ReadFrame()
				if err != nil {
					return
				}
				switch frame := frame.(type) {
				case *http2.SettingsFrame:
					if !frame.IsAck() {
						if err = framer.WriteSettingsAck(); err != nil {
							return
						}
					}
				case *http2.PingFrame:
					if !frame.IsAck() {
						if err = framer.WritePing(true, frame.Data); err != nil {
							return
						}
					}
				case *http2.MetaHeadersFrame:
					methods[frame.StreamID] = frame.PseudoValue("method")
					if frame.StreamEnded() {
						if err = complete(frame.StreamID); err != nil {
							return
						}
					}
				case *http2.DataFrame:
					bodies[frame.StreamID] = append(bodies[frame.StreamID], frame.Data()...)
					if frame.StreamEnded() {
						if err = complete(frame.StreamID); err != nil {
							return
						}
					}
				}
			}
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &gets, &posts
}

func TestHTTP2CreateResetNeverReplays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   http2.ErrCode
		goAway bool
	}{
		{"protocol-error", http2.ErrCodeProtocol, false},
		{"refused-stream", http2.ErrCodeRefusedStream, false},
		{"goaway", http2.ErrCodeNo, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := makeFixture(t)
			server, gets, posts := resetServer(t, http.MethodPost, tc.code, tc.goAway, f.hash)
			c := newClient(f.creds)
			c.endpoint = server.URL
			c.transport = server.Client().Transport // trusts only this server's synthetic TLS certificate
			c.requestTimeout = 2 * time.Second
			uploads := 0
			c.upload = func(context.Context, uploadAttributes, *os.File, string) error { uploads++; return nil }
			out, err := f.command(context.Background(), f.submitArgs(), c)
			record := f.readRecord(t)
			t.Logf("observed TLS h2 GETs=%d create POSTs=%d uploads=%d phase=%s id=%q error=%v", gets.Load(), posts.Load(), uploads, record.Phase, record.SubmissionID, err)
			if gets.Load() != 1 || posts.Load() != 1 {
				t.Errorf("create replayed over HTTP/2: GETs=%d POSTs=%d; want 1 each", gets.Load(), posts.Load())
			}
			requireError(t, err, "request_failed")
			if uploads != 0 || out != "" || record.Phase != "uploading" || record.SubmissionID != "" {
				t.Fatalf("create outcome failed open: uploads=%d output=%q record=%+v", uploads, out, record)
			}
			// The unknown outcome remains a hard refusal before any new provider GET/POST.
			_, err = f.command(context.Background(), f.submitArgs(), c)
			requireError(t, err, "submission_refused")
			if gets.Load() != 1 || posts.Load() != 1 {
				t.Fatal("retried ambiguous create on another submit")
			}
		})
	}
}

func TestHTTP2SafeGETCanRetryReset(t *testing.T) {
	f := makeFixture(t)
	server, gets, posts := resetServer(t, http.MethodGet, http2.ErrCodeRefusedStream, false, f.hash)
	c := newClient(f.creds)
	c.endpoint = server.URL
	c.transport = server.Client().Transport
	c.requestTimeout = 2 * time.Second
	out, err := f.command(context.Background(), []string{"preflight"}, c)
	if err != nil || strings.TrimSpace(out) != `{"status":"ready"}` || gets.Load() != 2 || posts.Load() != 0 {
		t.Fatalf("safe GET failed: output=%q error=%v GETs=%d POSTs=%d", out, err, gets.Load(), posts.Load())
	}
}
