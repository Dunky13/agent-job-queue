package notaryapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/sys/unix"
)

const testID = "2efe2717-52ef-43a5-96dc-0797e4ca1041"
const testIssuer = "57246542-96fe-1a63-e053-0824d011072a"
const testKeyID = "2X9R4HXF34"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }
func response(r *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

type fixture struct {
	dir, keyPath, archive, record, hash string
	creds                               *credentials
	env                                 map[string]string
}

func makeFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir()}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	f.keyPath = filepath.Join(f.dir, "AuthKey.p8")
	writeFile(t, f.keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	f.archive = filepath.Join(f.dir, "payload.zip")
	writeFile(t, f.archive, []byte("synthetic archive, no provider writes"))
	digest := sha256.Sum256([]byte("synthetic archive, no provider writes"))
	f.hash = hex.EncodeToString(digest[:])
	f.record = filepath.Join(f.dir, "payload.json")
	f.env = map[string]string{"NOTARY_KEY_FILE": f.keyPath, "NOTARY_KEY_ID": testKeyID, "NOTARY_ISSUER_ID": testIssuer, "GITHUB_RUN_ATTEMPT": "2"}
	f.creds, err = loadCredentials(f.getenv)
	if err != nil {
		t.Fatal(err)
	}
	f.writeRecord(t, stageRecord{Stage: "payload", Phase: "prepared", PreparedAttempt: "2", ArchiveSHA256: f.hash})
	return f
}
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) getenv(key string) string { return f.env[key] }
func (f *fixture) writeRecord(t *testing.T, r stageRecord) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.record, data)
}
func (f *fixture) readRecord(t *testing.T) stageRecord {
	t.Helper()
	data, err := os.ReadFile(f.record)
	if err != nil {
		t.Fatal(err)
	}
	var r stageRecord
	if err = json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *fixture) client(fn roundTripFunc) *client {
	c := newClient(f.creds)
	c.transport = fn
	c.pollInterval = time.Millisecond
	c.requestTimeout = time.Second
	return c
}
func createResponse(attrs string) string {
	return `{"data":{"id":"` + testID + `","attributes":` + attrs + `}}`
}

const goodAttrs = `{"awsAccessKeyId":"synthetic-access","awsSecretAccessKey":"synthetic-secret","awsSessionToken":"synthetic-session","bucket":"notary-test-bucket","object":"synthetic-object"}`

func (f *fixture) command(ctx context.Context, args []string, c *client) (string, error) {
	var out bytes.Buffer
	err := run(ctx, args, &out, f.getenv, func(creds *credentials) *client { c.credentials = creds; return c })
	return out.String(), err
}
func (f *fixture) submitArgs() []string {
	return []string{"submit", "--archive", f.archive, "--record", f.record, "--sha256", f.hash}
}
func requireError(t *testing.T, err error, code string) {
	t.Helper()
	if err != failure(code) {
		t.Fatalf("error = %v; want %s", err, code)
	}
}

func TestJWTSignatureAndClaims(t *testing.T) {
	f := makeFixture(t)
	now := time.Unix(1800000000, 0)
	raw, err := f.creds.token(now)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Headers) != 1 || tok.Headers[0].KeyID != testKeyID || tok.Headers[0].Algorithm != "ES256" || tok.Headers[0].ExtraHeaders[jose.HeaderType] != "JWT" {
		t.Fatalf("unexpected headers: %+v", tok.Headers)
	}
	var claims jwt.Claims
	if err = tok.Claims(&f.creds.key.PublicKey, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != testIssuer || len(claims.Audience) != 1 || claims.Audience[0] != "appstoreconnect-v1" || !claims.IssuedAt.Time().Equal(now) || !claims.Expiry.Time().Equal(now.Add(5*time.Minute)) {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Claims(&other.PublicKey, &claims) == nil {
		t.Fatal("wrong key verified JWT")
	}
}

func TestMalformedKeysNeverContactProvider(t *testing.T) {
	for _, kind := range []string{"empty", "garbage", "encrypted", "sec1", "p384", "rsa", "trailing", "leading", "individual", "bad-key-id", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			data, err := os.ReadFile(f.keyPath)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "empty":
				data = nil
			case "garbage":
				data = []byte("private-key-material")
			case "encrypted":
				data = pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("secret")})
			case "sec1":
				der, e := x509.MarshalECPrivateKey(f.creds.key)
				if e != nil {
					t.Fatal(e)
				}
				data = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
			case "p384":
				key, e := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
				if e != nil {
					t.Fatal(e)
				}
				der, e := x509.MarshalPKCS8PrivateKey(key)
				if e != nil {
					t.Fatal(e)
				}
				data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			case "rsa":
				key, e := rsa.GenerateKey(rand.Reader, 2048)
				if e != nil {
					t.Fatal(e)
				}
				der, e := x509.MarshalPKCS8PrivateKey(key)
				if e != nil {
					t.Fatal(e)
				}
				data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			case "trailing":
				data = append(data, []byte("unexpected")...)
			case "leading":
				data = append([]byte("unexpected\n"), data...)
			case "individual":
				f.env["NOTARY_ISSUER_ID"] = ""
			case "bad-key-id":
				f.env["NOTARY_KEY_ID"] = "bad\nheader"
			case "symlink":
				link := filepath.Join(f.dir, "key-link")
				if err = os.Symlink(f.keyPath, link); err != nil {
					t.Fatal(err)
				}
				f.env["NOTARY_KEY_FILE"] = link
			}
			writeFile(t, f.keyPath, data)
			c := f.client(func(r *http.Request) (*http.Response, error) {
				t.Fatal("provider called with invalid key")
				return nil, errors.New("unexpected")
			})
			out, err := f.command(context.Background(), []string{"preflight"}, c)
			if err == nil || !strings.HasPrefix(out, `{"status":"error","error":"invalid_`) || strings.Contains(out, "private-key-material") {
				t.Fatalf("unsafe preflight result: %q %v", out, err)
			}
			_, err = f.command(context.Background(), f.submitArgs(), c)
			if err == nil {
				t.Fatal("submit accepted invalid key")
			}
			if f.readRecord(t).Phase != "prepared" {
				t.Fatal("changed record")
			}
		})
	}
}

func TestPreflightOnlyFixedOutputAndFixedEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		body, want string
		err        string
	}{
		{"ready", 200, `{"data":[{"privateAccount":"inventory"}],"links":{"next":"https://untrusted.invalid"}}`, "{\"status\":\"ready\"}\n", ""},
		{"denied", 403, `{"privateAccount":"inventory","token":"secret"}`, "{\"status\":\"error\",\"error\":\"authentication_failed\"}\n", "authentication_failed"},
		{"invalid", 200, `{"data":null}`, "{\"status\":\"error\",\"error\":\"invalid_response\"}\n", "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := makeFixture(t)
			f.env["NOTARY_ENDPOINT"] = "https://untrusted.invalid"
			calls := 0
			c := f.client(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.String() != productionEndpoint || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				return response(r, tc.code, tc.body), nil
			})
			out, err := f.command(context.Background(), []string{"preflight"}, c)
			if tc.err == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireError(t, err, tc.err)
			}
			if out != tc.want || calls != 1 {
				t.Fatalf("output=%q calls=%d", out, calls)
			}
		})
	}
}

func TestInvalidPreparedStateNeverContactsProvider(t *testing.T) {
	for _, kind := range []string{"hash", "archive-bytes", "attempt", "legacy-uploading", "submitted", "completed", "unknown-field", "missing-field", "duplicate-field", "null-field", "stage", "record-symlink", "archive-symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			r := f.readRecord(t)
			switch kind {
			case "hash":
				r.ArchiveSHA256 = strings.Repeat("0", 64)
			case "archive-bytes":
				writeFile(t, f.archive, []byte("changed"))
			case "attempt":
				r.PreparedAttempt = "1"
			case "legacy-uploading":
				r.Phase = "uploading"
			case "submitted":
				r.Phase = "submitted"
				r.SubmissionID = testID
			case "completed":
				r.CompletedSHA256 = f.hash
			case "stage":
				r.Stage = "unknown"
			}
			f.writeRecord(t, r)
			switch kind {
			case "unknown-field", "missing-field", "null-field":
				data, e := os.ReadFile(f.record)
				if e != nil {
					t.Fatal(e)
				}
				var fields map[string]json.RawMessage
				if e = json.Unmarshal(data, &fields); e != nil {
					t.Fatal(e)
				}
				if kind == "unknown-field" {
					fields["unknown"] = json.RawMessage(`"x"`)
				} else if kind == "missing-field" {
					delete(fields, "submission_id")
				} else {
					fields["submission_id"] = json.RawMessage(`null`)
				}
				data, e = json.Marshal(fields)
				if e != nil {
					t.Fatal(e)
				}
				writeFile(t, f.record, data)
			case "duplicate-field":
				data, e := os.ReadFile(f.record)
				if e != nil {
					t.Fatal(e)
				}
				data = append([]byte(`{"phase":"prepared",`), data[1:]...)
				writeFile(t, f.record, data)
			case "record-symlink":
				link := filepath.Join(f.dir, "record-link")
				if e := os.Symlink(f.record, link); e != nil {
					t.Fatal(e)
				}
				f.record = link
			case "archive-symlink":
				link := filepath.Join(f.dir, "archive-link")
				if e := os.Symlink(f.archive, link); e != nil {
					t.Fatal(e)
				}
				f.archive = link
			}
			c := f.client(func(r *http.Request) (*http.Response, error) {
				t.Fatal("provider called before validation")
				return nil, errors.New("unexpected")
			})
			_, err := f.command(context.Background(), f.submitArgs(), c)
			if err == nil {
				t.Fatal("accepted invalid state")
			}
		})
	}
}

func TestSubmitDurableOrderingAndNoRecreation(t *testing.T) {
	for _, kind := range []string{"success", "run-owned", "partial-upload", "malformed-attrs", "wrong-type-attrs", "persist-id-failure", "persist-intent-failure", "auth-failure"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			if kind == "run-owned" {
				record := f.readRecord(t)
				record.PreparedRunID = "333"
				f.env["GITHUB_RUN_ID"] = "333"
				f.writeRecord(t, record)
			}
			posts, gets, uploads, saves := 0, 0, 0, 0
			c := f.client(func(req *http.Request) (*http.Response, error) {
				if req.Method == "GET" {
					gets++
					if f.readRecord(t).Phase != "prepared" {
						t.Fatal("authentication followed state transition")
					}
					code := 200
					if kind == "auth-failure" {
						code = 403
					}
					return response(req, code, `{"data":[]}`), nil
				}
				posts++
				r := f.readRecord(t)
				if r.Phase != "uploading" || r.SubmissionID != "" || saves != 1 {
					t.Fatalf("create before durable intent: %+v saves=%d", r, saves)
				}
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				var body map[string]string
				if err = json.Unmarshal(data, &body); err != nil {
					t.Fatal(err)
				}
				if body["sha256"] != f.hash || body["submissionName"] != "payload.zip" || len(body) != 2 {
					t.Fatalf("bad body: %s", data)
				}
				attrs := goodAttrs
				if kind == "malformed-attrs" {
					attrs = `{"bucket":"x"}`
				} else if kind == "wrong-type-attrs" {
					attrs = `"not an object"`
				}
				return response(req, 201, createResponse(attrs)), nil
			})
			c.persist = func(dir *os.File, name string, data []byte) error {
				saves++
				if kind == "persist-id-failure" && saves == 2 || kind == "persist-intent-failure" && saves == 1 {
					return errors.New("secret persistence detail")
				}
				return atomicWrite(dir, name, data)
			}
			c.upload = func(ctx context.Context, a uploadAttributes, file *os.File, hash string) error {
				uploads++
				r := f.readRecord(t)
				if r.Phase != "submitted" || r.SubmissionID != testID || saves != 2 {
					t.Fatalf("upload before durable ID: %+v saves=%d", r, saves)
				}
				if hash != f.hash {
					t.Fatal("wrong upload hash")
				}
				data, e := io.ReadAll(file)
				if e != nil {
					t.Fatal(e)
				}
				if string(data) != "synthetic archive, no provider writes" {
					t.Fatal("wrong upload stream")
				}
				if kind == "partial-upload" {
					return errors.New("secret signed URL")
				}
				return nil
			}
			out, err := f.command(context.Background(), f.submitArgs(), c)
			switch kind {
			case "success", "run-owned":
				if err != nil {
					t.Fatal(err)
				}
			case "partial-upload":
				requireError(t, err, "upload_failed")
			case "malformed-attrs", "wrong-type-attrs":
				requireError(t, err, "invalid_upload_attributes")
			case "persist-id-failure", "persist-intent-failure":
				requireError(t, err, "record_persistence_failed")
			case "auth-failure":
				requireError(t, err, "authentication_failed")
			}
			wantPosts := 1
			if kind == "persist-intent-failure" || kind == "auth-failure" {
				wantPosts = 0
			}
			if posts != wantPosts || gets != 1 {
				t.Fatalf("posts=%d gets=%d", posts, gets)
			}
			wantUploads := 0
			if kind == "success" || kind == "run-owned" || kind == "partial-upload" {
				wantUploads = 1
			}
			if uploads != wantUploads {
				t.Fatalf("uploads=%d", uploads)
			}
			if wantPosts == 1 && out != "{\"id\":\""+testID+"\",\"status\":\"Submitted\"}\n" {
				t.Fatalf("lost public recovery ID: %q", out)
			}
			if wantPosts == 0 && out != "" {
				t.Fatalf("unexpected output: %q", out)
			}
			if kind == "persist-id-failure" {
				if r := f.readRecord(t); r.Phase != "uploading" || r.SubmissionID != "" {
					t.Fatalf("record: %+v", r)
				}
			}
			if wantPosts == 1 && kind != "persist-id-failure" {
				if r := f.readRecord(t); r.Phase != "submitted" || r.SubmissionID != testID {
					t.Fatalf("record: %+v", r)
				}
			}
			if kind == "run-owned" && f.readRecord(t).PreparedRunID != "333" {
				t.Fatal("submission state lost execution custody")
			}
			// Every created outcome, even a partial upload, must refuse resubmission before GET.
			if wantPosts == 1 {
				_, err = f.command(context.Background(), f.submitArgs(), c)
				requireError(t, err, "submission_refused")
				if posts != 1 || gets != 1 {
					t.Fatal("recreated existing submission")
				}
			}
		})
	}
}

func TestDroppedAndTimedOutCreateIsOnePOST(t *testing.T) {
	for _, kind := range []string{"drop", "timeout", "malformed", "invalid-id", "server-error", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			var posts atomic.Int32
			var timedOut atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					fmt.Fprint(w, `{"data":[]}`)
					return
				}
				posts.Add(1)
				switch kind {
				case "drop":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				case "malformed":
					fmt.Fprint(w, `{"data":`)
				case "invalid-id":
					fmt.Fprint(w, `{"data":{"id":"not-a-uuid"}}`)
				case "server-error":
					w.WriteHeader(503)
					fmt.Fprint(w, "private provider details")
				case "redirect":
					w.Header().Set("Location", "/other")
					w.WriteHeader(307)
				}
			}))
			defer server.Close()
			c := newClient(f.creds)
			c.endpoint = server.URL
			if kind == "timeout" {
				// Observe the POST before waiting for its real request deadline.
				// A short deadline around socket setup can expire before dispatch
				// on a busy host, leaving the ambiguous-create path untested.
				c.requestTimeout = 40 * time.Millisecond
				c.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet {
						return response(r, http.StatusOK, `{"data":[]}`), nil
					}
					posts.Add(1)
					<-r.Context().Done()
					timedOut.Store(errors.Is(r.Context().Err(), context.DeadlineExceeded))
					return nil, r.Context().Err()
				})
			}
			_, err := f.command(context.Background(), f.submitArgs(), c)
			if err == nil {
				t.Fatal("create unexpectedly succeeded")
			}
			if posts.Load() != 1 {
				t.Fatalf("POST count=%d", posts.Load())
			}
			if kind == "timeout" && !timedOut.Load() {
				t.Fatal("create did not reach its request deadline")
			}
			r := f.readRecord(t)
			if r.Phase != "uploading" || r.SubmissionID != "" {
				t.Fatalf("lost unknown outcome guard: %+v", r)
			}
			_, err = f.command(context.Background(), f.submitArgs(), c)
			requireError(t, err, "submission_refused")
			if posts.Load() != 1 {
				t.Fatal("create retried")
			}
		})
	}
}

func TestDirectoryLockPrecedesProvider(t *testing.T) {
	f := makeFixture(t)
	dir, err := openDirectory(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err = unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(dir.Fd()), unix.LOCK_UN)
	c := f.client(func(r *http.Request) (*http.Response, error) {
		t.Fatal("provider called while directory locked")
		return nil, errors.New("unexpected")
	})
	_, err = f.command(context.Background(), f.submitArgs(), c)
	requireError(t, err, "record_locked")
}

func TestOfficialS3PutObjectUsesScopedCredentialsAndNoRetry(t *testing.T) {
	for _, code := range []int{200, 503, 307} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := makeFixture(t)
			calls := 0
			c := f.client(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "PUT" || r.URL.Scheme != "https" || r.URL.Host != "notary-test-bucket.s3.us-west-2.amazonaws.com" || r.URL.Path != "/synthetic-object" {
					t.Fatalf("unexpected S3 request: %s %s", r.Method, r.URL)
				}
				auth := r.Header.Get("Authorization")
				if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "synthetic-access/") || !strings.Contains(auth, "/us-west-2/s3/aws4_request") || strings.Contains(auth, "Bearer") || r.Header.Get("X-Amz-Security-Token") != "synthetic-session" {
					t.Fatalf("wrong S3 auth: %s", auth)
				}
				digest, _ := hex.DecodeString(f.hash)
				if r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(digest) {
					t.Fatal("missing archive checksum")
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != "synthetic archive, no provider writes" {
					t.Fatalf("wrong upload stream: %q", data)
				}
				resp := response(r, code, "")
				if code == 307 {
					resp.Header.Set("Location", "https://untrusted.invalid")
				}
				return resp, nil
			})
			var a uploadAttributes
			if err := json.Unmarshal([]byte(goodAttrs), &a); err != nil {
				t.Fatal(err)
			}
			file, err := openRegular(f.archive)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			err = c.putObject(context.Background(), a, file, f.hash)
			if code == 200 && err != nil {
				t.Fatal(err)
			}
			if code != 200 && err == nil {
				t.Fatal("S3 failure accepted")
			}
			if calls != 1 {
				t.Fatalf("SDK retried: %d", calls)
			}
		})
	}
}

func TestWaitStatusesTimeoutCancellationAndPublicOutput(t *testing.T) {
	for _, kind := range []string{"accepted", "invalid", "rejected", "timeout", "cancelled", "cancel-during-get", "unknown", "wrong-id", "wrong-id-after-pending", "malformed-after-pending"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := f.client(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/notary/v2/submissions/"+testID {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if kind == "cancel-during-get" {
					cancel()
					return nil, errors.New("private cancellation error")
				}
				status := "In Progress"
				id := testID
				if calls == 2 && kind == "wrong-id-after-pending" {
					id = testIssuer
				}
				if calls == 2 && kind == "malformed-after-pending" {
					return response(r, 200, `{broken`), nil
				}
				switch kind {
				case "accepted":
					if calls == 2 {
						status = "Accepted"
					}
				case "invalid":
					status = "Invalid"
				case "rejected":
					status = "Rejected"
				case "unknown":
					status = "private provider text"
				case "wrong-id":
					status = "Accepted"
					id = testIssuer
				}
				return response(r, 200, `{"data":{"id":"`+id+`","attributes":{"status":"`+status+`"}}}`), nil
			})
			if kind == "cancelled" {
				cancel()
			}
			timeout := "1s"
			if kind == "timeout" {
				timeout = "15ms"
				c.pollInterval = time.Second
			}
			out, err := f.command(ctx, []string{"wait", "--id", testID, "--timeout", timeout}, c)
			wantStatus := "Unknown"
			switch kind {
			case "accepted":
				wantStatus = "Accepted"
				if err != nil || calls != 2 {
					t.Fatalf("err=%v calls=%d", err, calls)
				}
			case "invalid":
				wantStatus = "Invalid"
				requireError(t, err, "submission_rejected")
			case "rejected":
				wantStatus = "Rejected"
				requireError(t, err, "submission_rejected")
			case "timeout":
				wantStatus = "In Progress"
				requireError(t, err, "timeout")
				if calls != 1 {
					t.Fatalf("polls=%d", calls)
				}
			case "cancelled", "cancel-during-get":
				requireError(t, err, "cancelled")
			case "unknown", "wrong-id", "wrong-id-after-pending", "malformed-after-pending":
				requireError(t, err, "invalid_response")
			}
			wantError := ""
			switch kind {
			case "timeout":
				wantError = "timeout"
			case "invalid", "rejected":
				wantError = "submission_rejected"
			case "cancelled", "cancel-during-get":
				wantError = "cancelled"
			case "unknown", "wrong-id", "wrong-id-after-pending", "malformed-after-pending":
				wantError = "invalid_response"
			}
			encodedError := ""
			if wantError != "" {
				encodedError = ",\"error\":\"" + wantError + "\""
			}
			if out != "{\"id\":\""+testID+"\",\"status\":\""+wantStatus+"\""+encodedError+"}\n" {
				t.Fatalf("unsafe or incorrect output: %q", out)
			}
		})
	}
}

func TestAPIAndLogRedirectsNeverForwardJWT(t *testing.T) {
	for _, kind := range []string{"history-redirect", "log-redirect", "log-success"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			calls := 0
			outPath := filepath.Join(f.dir, "log.json")
			c := f.client(func(r *http.Request) (*http.Response, error) {
				calls++
				switch calls {
				case 1:
					if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
						t.Fatal("missing Apple JWT")
					}
					if kind == "history-redirect" {
						resp := response(r, 302, "")
						resp.Header.Set("Location", "https://credentials.invalid/steal")
						return resp, nil
					}
					return response(r, 200, `{"data":{"attributes":{"developerLogUrl":"https://logs.invalid/private?signature=secret-token"}}}`), nil
				case 2:
					if r.URL.Host != "logs.invalid" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
						t.Fatalf("credentials forwarded to signed URL: %v", r.Header)
					}
					if kind == "log-redirect" {
						resp := response(r, 307, "")
						resp.Header.Set("Location", "https://credentials.invalid/steal")
						return resp, nil
					}
					return response(r, 200, `{"jobId":"`+testID+`","status":"Accepted"}`), nil
				default:
					t.Fatal("redirect followed")
					return nil, errors.New("unexpected")
				}
			})
			args := []string{"log", "--id", testID, "--output", outPath}
			if kind == "history-redirect" {
				args = []string{"preflight"}
			}
			out, err := f.command(context.Background(), args, c)
			if kind == "log-success" {
				if err != nil {
					t.Fatal(err)
				}
				data, e := os.ReadFile(outPath)
				if e != nil || !json.Valid(data) {
					t.Fatalf("log not saved: %v", e)
				}
				info, e := os.Stat(outPath)
				if e != nil || info.Mode().Perm() != 0600 {
					t.Fatal("nonprivate log")
				}
			} else if err == nil {
				t.Fatal("redirect accepted")
			}
			if strings.Contains(out, "secret-token") || strings.Contains(out, "Bearer") || err != nil && strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("secret output: %s %v", out, err)
			}
			wantCalls := 2
			if kind == "history-redirect" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestResponseBoundsAndTimeouts(t *testing.T) {
	for _, kind := range []string{"history-bound", "create-bound", "log-url-bound", "log-bound", "log-invalid-json", "history-timeout"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			calls := 0
			c := f.client(func(r *http.Request) (*http.Response, error) {
				calls++
				if kind == "history-timeout" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				if kind == "history-bound" || kind == "create-bound" && r.Method == "POST" || kind == "log-url-bound" {
					return response(r, 200, strings.Repeat("x", maxResponse+1)), nil
				}
				if strings.HasPrefix(kind, "log-") {
					if calls == 1 {
						return response(r, 200, `{"data":{"attributes":{"developerLogUrl":"https://logs.invalid/object?token=secret"}}}`), nil
					}
					if kind == "log-invalid-json" {
						return response(r, 200, "not-json secret"), nil
					}
					return response(r, 200, strings.Repeat("x", maxLog+1)), nil
				}
				return response(r, 200, `{"data":[]}`), nil
			})
			c.requestTimeout = 10 * time.Millisecond
			args := []string{"preflight"}
			if kind == "create-bound" {
				args = f.submitArgs()
			} else if strings.HasPrefix(kind, "log-") {
				args = []string{"log", "--id", testID, "--output", filepath.Join(f.dir, "log.json")}
			}
			out, err := f.command(context.Background(), args, c)
			if err == nil {
				t.Fatal("unbounded/invalid response accepted")
			}
			if strings.Contains(out, "xxxx") || strings.Contains(out, "secret") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe error output: %q %v", out, err)
			}
			if kind == "create-bound" && f.readRecord(t).Phase != "uploading" {
				t.Fatal("lost ambiguous create guard")
			}
		})
	}
}

func TestLogRejectsUnsafeURLsAndOutputFiles(t *testing.T) {
	for _, kind := range []string{"http-url", "userinfo-url", "fragment-url", "missing-url", "symlink-output", "hardlink-output", "populated-output", "public-output", "directory-output"} {
		t.Run(kind, func(t *testing.T) {
			f := makeFixture(t)
			path := filepath.Join(f.dir, "log.json")
			calls := 0
			switch kind {
			case "symlink-output":
				if err := os.Symlink(f.archive, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink-output":
				writeFile(t, path, nil)
				if err := os.Link(path, path+"-link"); err != nil {
					t.Fatal(err)
				}
			case "populated-output":
				writeFile(t, path, []byte("keep previous log"))
			case "public-output":
				writeFile(t, path, nil)
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory-output":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			c := f.client(func(r *http.Request) (*http.Response, error) {
				calls++
				u := "https://logs.invalid/log"
				switch kind {
				case "http-url":
					u = "http://logs.invalid/log"
				case "userinfo-url":
					u = "https://user:secret@logs.invalid/log"
				case "fragment-url":
					u = "https://logs.invalid/log#secret"
				case "missing-url":
					u = ""
				}
				return response(r, 200, `{"data":{"attributes":{"developerLogUrl":"`+u+`"}}}`), nil
			})
			_, err := f.command(context.Background(), []string{"log", "--id", testID, "--output", path}, c)
			if err == nil {
				t.Fatal("unsafe log accepted")
			}
			if strings.HasSuffix(kind, "output") {
				if calls != 0 {
					t.Fatal("provider called with invalid output")
				}
			} else if calls != 1 {
				t.Fatal("unsafe URL fetched")
			}
			if kind == "populated-output" {
				data, e := os.ReadFile(path)
				if e != nil || string(data) != "keep previous log" {
					t.Fatal("overwrote previous log")
				}
			}
		})
	}
}

func TestCLIPhysicalParentsAndPrivateMktempOutput(t *testing.T) {
	f := makeFixture(t)
	// Exercise the same parser, credential loader, file opening and log writing
	// used by queue-tool with a synthetic parent alias like macOS /var -> /private/var.
	alias := filepath.Join(t.TempDir(), "temp-alias")
	if err := os.Symlink(f.dir, alias); err != nil {
		t.Fatal(err)
	}
	f.keyPath = filepath.Join(alias, "AuthKey.p8")
	f.env["NOTARY_KEY_FILE"] = f.keyPath
	f.archive = filepath.Join(alias, "payload.zip")
	f.record = filepath.Join(alias, "payload.json")
	calls := 0
	c := f.client(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == "POST" {
			return response(r, 201, createResponse(goodAttrs)), nil
		}
		return response(r, 200, `{"data":[]}`), nil
	})
	c.upload = func(context.Context, uploadAttributes, *os.File, string) error { return nil }
	if _, err := f.command(context.Background(), f.submitArgs(), c); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	file, err := os.CreateTemp(alias, "provider-log-")
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	c = f.client(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "logs.invalid" {
			if r.Header.Get("Authorization") != "" {
				t.Fatal("JWT forwarded")
			}
			return response(r, 200, `{"jobId":"`+testID+`","status":"Accepted"}`), nil
		}
		return response(r, 200, `{"data":{"attributes":{"developerLogUrl":"https://logs.invalid/object"}}}`), nil
	})
	out, err := f.command(context.Background(), []string{"log", "--id", testID, "--output", file.Name()}, c)
	if err != nil || out != "" {
		t.Fatalf("mktemp CLI result: %q %v", out, err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || !json.Valid(data) {
		t.Fatal("mktemp output not written")
	}
	// A final key symlink through this same supported parent alias remains forbidden.
	link := filepath.Join(alias, "key-link.p8")
	if err = os.Symlink(f.keyPath, link); err != nil {
		t.Fatal(err)
	}
	f.env["NOTARY_KEY_FILE"] = link
	_, err = f.command(context.Background(), []string{"preflight"}, c)
	requireError(t, err, "invalid_key")
}

func TestCLIRejectsUnexpectedAndUnsafeArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"preflight", "--endpoint", "https://untrusted.invalid"}, {"preflight", "trailing"}, {"wait", "--id", "../secret"}, {"log", "--id", testID, "--output", "x", "--key", "secret"}} {
		f := makeFixture(t)
		c := f.client(func(r *http.Request) (*http.Response, error) {
			t.Fatal("provider called for invalid arguments")
			return nil, errors.New("unexpected")
		})
		_, err := f.command(context.Background(), args, c)
		requireError(t, err, "invalid_arguments")
	}
}

func TestPartialOfficialS3SendRetainsIDAndNeverRetries(t *testing.T) {
	f := makeFixture(t)
	posts, puts := 0, 0
	c := f.client(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case "GET":
			return response(r, 200, `{"data":[]}`), nil
		case "POST":
			posts++
			return response(r, 201, createResponse(goodAttrs)), nil
		case "PUT":
			puts++
			if f.readRecord(t).SubmissionID != testID {
				t.Fatal("S3 began without durable ID")
			}
			var firstBytes [8]byte
			if _, err := io.ReadFull(r.Body, firstBytes[:]); err != nil {
				t.Fatal(err)
			}
			return nil, io.ErrUnexpectedEOF
		default:
			t.Fatalf("unexpected method %s", r.Method)
			return nil, errors.New("unexpected")
		}
	})
	out, err := f.command(context.Background(), f.submitArgs(), c)
	requireError(t, err, "upload_failed")
	if !strings.Contains(out, testID) || f.readRecord(t).SubmissionID != testID || posts != 1 || puts != 1 {
		t.Fatalf("lost partial-upload recovery: %q posts=%d puts=%d", out, posts, puts)
	}
	_, err = f.command(context.Background(), f.submitArgs(), c)
	requireError(t, err, "submission_refused")
	if posts != 1 || puts != 1 {
		t.Fatal("retried a partial upload")
	}
}

func TestSubmitExecutionCustodyPrecedesProviderCalls(t *testing.T) {
	for _, variant := range []string{"matching run", "wrong run", "missing record run", "missing execution run"} {
		t.Run(variant, func(t *testing.T) {
			f := makeFixture(t)
			record := f.readRecord(t)
			record.PreparedRunID = "333"
			f.env["GITHUB_RUN_ID"] = "333"
			switch variant {
			case "wrong run":
				f.env["GITHUB_RUN_ID"] = "444"
			case "missing record run":
				record.PreparedRunID = ""
			case "missing execution run":
				delete(f.env, "GITHUB_RUN_ID")
			}
			f.writeRecord(t, record)
			calls := 0
			c := f.client(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != "GET" {
					t.Fatal("custody test must not create an Apple submission")
				}
				return response(req, http.StatusForbidden, `{"errors":[]}`), nil
			})
			_, err := f.command(context.Background(), f.submitArgs(), c)
			if err == nil {
				t.Fatal("expected preflight denial or custody refusal")
			}
			if variant == "matching run" {
				if calls != 1 {
					t.Fatalf("valid custody never reached preflight: %d", calls)
				}
			} else if calls != 0 {
				t.Fatalf("invalid execution custody contacted provider %d times", calls)
			}
		})
	}
}
