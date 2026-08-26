package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

var (
	s3RegionPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	s3BucketPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{1,61}[A-Za-z0-9]$`)
)

const (
	S3AddressingStyleAuto    = "auto"
	S3AddressingStylePath    = "path"
	S3AddressingStyleVirtual = "virtual"
)

// S3Config contains the settings needed to check an S3-compatible object store.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	AddressingStyle string
}

// CheckS3 uploads, downloads, verifies, and deletes a unique temporary object.
// It also makes a best-effort cleanup attempt when a later operation fails.
func CheckS3(ctx context.Context, cfg S3Config) error {
	return checkS3(ctx, cfg, &http.Client{Timeout: 30 * time.Second})
}

// ResolveS3AddressingStyle returns the effective path or virtual-hosted style.
func ResolveS3AddressingStyle(cfg S3Config) (string, error) {
	cfg, err := validateS3Config(cfg)
	if err != nil {
		return "", err
	}
	if usePathStyle(cfg) {
		return S3AddressingStylePath, nil
	}
	return S3AddressingStyleVirtual, nil
}

func checkS3(ctx context.Context, cfg S3Config, httpClient aws.HTTPClient) (err error) {
	client, cfg, err := newS3Client(cfg, httpClient)
	if err != nil {
		return err
	}
	key := fmt.Sprintf(
		".wenzwork-init/probe-%s-%s.txt",
		time.Now().UTC().Format("20060102T150405Z"),
		uuid.NewString(),
	)
	payload := []byte(fmt.Sprintf("WenzWork S3 initialization probe: %s\n", key))
	uploaded := false

	defer func() {
		if !uploaded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, cleanupErr := client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{
			Bucket: aws.String(cfg.Bucket),
			Key:    aws.String(key),
		}); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean up S3 probe object %q: %w", key, cleanupErr))
		}
	}()

	if _, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(cfg.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(payload),
		ContentType: aws.String("text/plain; charset=utf-8"),
	}); err != nil {
		return fmt.Errorf("upload S3 probe object: %w", err)
	}
	uploaded = true

	output, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("download S3 probe object: %w", err)
	}
	downloaded, readErr := io.ReadAll(io.LimitReader(output.Body, int64(len(payload)+1)))
	closeErr := output.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read S3 probe object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close S3 probe response: %w", closeErr)
	}
	if !bytes.Equal(downloaded, payload) {
		return errors.New("downloaded S3 probe content does not match the uploaded content")
	}

	if _, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete S3 probe object: %w", err)
	}
	uploaded = false
	return nil
}

func validateS3Config(cfg S3Config) (S3Config, error) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.AccessKeyID = strings.TrimSpace(cfg.AccessKeyID)
	cfg.AddressingStyle = strings.ToLower(strings.TrimSpace(cfg.AddressingStyle))
	if cfg.AddressingStyle == "" {
		cfg.AddressingStyle = S3AddressingStyleAuto
	}

	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return S3Config{}, errors.New("S3_ENDPOINT must be an absolute HTTP(S) URL")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return S3Config{}, errors.New("S3_ENDPOINT must not contain credentials, a query, or a fragment")
	}
	if !s3RegionPattern.MatchString(cfg.Region) {
		return S3Config{}, errors.New("S3_REGION contains unsupported characters")
	}
	if !s3BucketPattern.MatchString(cfg.Bucket) {
		return S3Config{}, errors.New("S3_BUCKET must be a valid 3-63 character bucket name")
	}
	if cfg.AccessKeyID == "" {
		return S3Config{}, errors.New("S3_ACCESS_KEY_ID is required")
	}
	if strings.TrimSpace(cfg.SecretAccessKey) == "" {
		return S3Config{}, errors.New("S3_SECRET_ACCESS_KEY is required")
	}
	switch cfg.AddressingStyle {
	case S3AddressingStyleAuto, S3AddressingStylePath, S3AddressingStyleVirtual:
	default:
		return S3Config{}, errors.New("S3_ADDRESSING_STYLE must be auto, path, or virtual")
	}
	return cfg, nil
}

func usePathStyle(cfg S3Config) bool {
	switch cfg.AddressingStyle {
	case S3AddressingStylePath:
		return true
	case S3AddressingStyleVirtual:
		return false
	}

	endpoint, _ := url.Parse(cfg.Endpoint)
	hostname := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	return !hostMatchesDomain(hostname, "aliyuncs.com") &&
		!hostMatchesDomain(hostname, "amazonaws.com") &&
		!hostMatchesDomain(hostname, "amazonaws.com.cn")
}

func hostMatchesDomain(hostname, domain string) bool {
	return hostname == domain || strings.HasSuffix(hostname, "."+domain)
}
