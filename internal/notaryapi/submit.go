package notaryapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sys/unix"
)

var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var attemptPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type stageRecord struct {
	Stage           string `json:"stage"`
	Phase           string `json:"phase"`
	PreparedAttempt string `json:"prepared_attempt"`
	PreparedRunID   string `json:"prepared_run_id,omitempty"`
	ArchiveSHA256   string `json:"archive_sha256"`
	SubmissionID    string `json:"submission_id"`
	CompletedSHA256 string `json:"completed_sha256"`
}

func readRecord(dir *os.File, name string) (stageRecord, error) {
	var record stageRecord
	f, err := openRegularAt(dir, name)
	if err != nil {
		return record, failure("invalid_record")
	}
	defer f.Close()
	data, err := readBounded(f, 16<<10)
	if err != nil {
		return record, failure("invalid_record")
	}
	fields, err := recordFields(data)
	if err != nil || (len(fields) != 6 && len(fields) != 7) {
		return record, failure("invalid_record")
	}
	if raw, ok := fields["prepared_run_id"]; ok {
		if json.Unmarshal(raw, &record.PreparedRunID) != nil || !attemptPattern.MatchString(record.PreparedRunID) {
			return record, failure("invalid_record")
		}
	} else if len(fields) != 6 {
		return record, failure("invalid_record")
	}
	for _, key := range []string{"stage", "phase", "prepared_attempt", "archive_sha256", "submission_id", "completed_sha256"} {
		var value string
		if raw, ok := fields[key]; !ok || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
			return record, failure("invalid_record")
		}
	}
	if json.Unmarshal(data, &record) != nil {
		return record, failure("invalid_record")
	}
	if record.Stage != "app" && record.Stage != "dmg" {
		return record, failure("invalid_record")
	}
	return record, nil
}

type uploadAttributes struct {
	AccessKeyID     string `json:"awsAccessKeyId"`
	SecretAccessKey string `json:"awsSecretAccessKey"`
	SessionToken    string `json:"awsSessionToken"`
	Bucket          string `json:"bucket"`
	Object          string `json:"object"`
}

func (a uploadAttributes) valid() bool {
	return a.AccessKeyID != "" && a.SecretAccessKey != "" && a.SessionToken != "" && bucketPattern.MatchString(a.Bucket) && a.Object != ""
}

func (c *client) submit(ctx context.Context, archive, recordPath, hash, attempt, run string) (result, error) {
	r := result{}
	if archive == "" || recordPath == "" || !hashPattern.MatchString(hash) || !attemptPattern.MatchString(attempt) {
		return r, failure("invalid_arguments")
	}
	dir, err := openDirectory(filepath.Dir(recordPath))
	if err != nil {
		return r, failure("invalid_record")
	}
	defer dir.Close()
	// Lock the directory inode, not the atomically replaced record inode.
	if err = unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return r, failure("record_locked")
	}
	defer unix.Flock(int(dir.Fd()), unix.LOCK_UN)
	name := filepath.Base(recordPath)
	record, err := readRecord(dir, name)
	if err != nil {
		return r, err
	}
	// This check precedes all provider calls, including the authentication GET.
	// In particular, legacy uploading+empty-ID records must never be resubmitted.
	if record.Phase != "prepared" || record.SubmissionID != "" || record.CompletedSHA256 != "" || record.PreparedAttempt != attempt || record.ArchiveSHA256 != hash || (record.PreparedRunID != "" && record.PreparedRunID != run) || (run != "" && record.PreparedRunID == "") {
		return r, failure("submission_refused")
	}
	f, err := openRegular(archive)
	if err != nil {
		return r, failure("invalid_archive")
	}
	defer f.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, f); err != nil || hex.EncodeToString(digest.Sum(nil)) != hash {
		return r, failure("archive_hash_mismatch")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return r, failure("invalid_archive")
	}
	if err = c.preflight(ctx); err != nil {
		return r, err
	}
	if ctx.Err() != nil {
		return r, contextFailure(ctx)
	}
	body, err := json.Marshal(struct {
		Name string `json:"submissionName"`
		Hash string `json:"sha256"`
	}{filepath.Base(archive), hash})
	if err != nil {
		return r, failure("invalid_archive")
	}
	record.Phase = "uploading"
	if err = c.save(dir, name, record); err != nil {
		return r, err
	}
	// One POST only. A dropped or malformed response leaves uploading+empty-ID;
	// no code path retries, recreates, or guesses whether Apple accepted it.
	data, err := c.request(ctx, http.MethodPost, "", body)
	if err != nil {
		return r, err
	}
	var response struct {
		Data struct {
			ID         string          `json:"id"`
			Attributes json.RawMessage `json:"attributes"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &response) != nil || !uuidPattern.MatchString(response.Data.ID) {
		return r, failure("invalid_response")
	}
	r = result{ID: response.Data.ID, Status: "Submitted"}
	record.Phase, record.SubmissionID = "submitted", r.ID
	// Persist the known ID before interpreting upload credentials. Even malformed
	// attributes cannot erase the only recovery evidence for this submission.
	if err = c.save(dir, name, record); err != nil {
		return r, err
	}
	var attrs uploadAttributes
	if json.Unmarshal(response.Data.Attributes, &attrs) != nil || !attrs.valid() {
		return r, failure("invalid_upload_attributes")
	}
	upload := c.upload
	if upload == nil {
		upload = c.putObject
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err = upload(ctx, attrs, f, hash); err != nil {
		return r, failure("upload_failed")
	}
	return r, nil
}

func (c *client) save(dir *os.File, name string, record stageRecord) error {
	data, err := json.Marshal(record)
	if err == nil {
		err = c.persist(dir, name, append(data, '\n'))
	}
	if err != nil {
		return failure("record_persistence_failed")
	}
	return nil
}

func (c *client) putObject(ctx context.Context, a uploadAttributes, archive *os.File, hash string) error {
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	digest, err := hex.DecodeString(hash)
	if err != nil {
		return err
	}
	// Construct the SDK directly: do not load ambient AWS credentials, profiles,
	// endpoints, or retry settings. Apple supplies credentials scoped to this upload.
	svc := s3.New(s3.Options{Region: "us-west-2",
		Credentials: awscredentials.NewStaticCredentialsProvider(a.AccessKeyID, a.SecretAccessKey, a.SessionToken),
		HTTPClient:  c.httpClient(30 * time.Minute), RetryMaxAttempts: 1,
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 1 }),
	})
	_, err = svc.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(a.Bucket), Key: aws.String(a.Object),
		Body: archive, ContentLength: aws.Int64(info.Size()), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(digest))})
	return err
}
