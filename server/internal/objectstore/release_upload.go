package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

const MaxReleaseAssetBytes = int64(5 * 1024 * 1024 * 1024)

var (
	ErrReleaseUploadInvalid          = errors.New("release asset upload input is invalid")
	ErrReleaseUploadTooLarge         = errors.New("release asset upload is too large")
	ErrReleaseUploadSizeMismatch     = errors.New("release asset upload size does not match")
	ErrReleaseUploadChecksumMismatch = errors.New("release asset upload checksum does not match")
	releaseSHA256Pattern             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ReleaseAssetUploadInput struct {
	Version       string
	Platform      string
	Architecture  string
	FileName      string
	FileSizeBytes int64
	SHA256        string
	ContentType   string
}

type ReleaseAssetUpload struct {
	ObjectKey     string `json:"objectKey"`
	DownloadURL   string `json:"downloadUrl"`
	FileSizeBytes int64  `json:"fileSizeBytes"`
	SHA256        string `json:"sha256"`
}

type ReleaseAssetUploader struct {
	client          *s3.Client
	uploader        *transfermanager.Client
	bucket          string
	downloadBaseURL string
}

func NewReleaseAssetUploader(cfg S3Config, downloadBaseURL string) (*ReleaseAssetUploader, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client, validated, err := newS3Client(cfg, &http.Client{Transport: transport})
	if err != nil {
		return nil, fmt.Errorf("configure release asset storage: %w", err)
	}
	baseURL, err := resolveDownloadBaseURL(validated, downloadBaseURL)
	if err != nil {
		return nil, err
	}
	return &ReleaseAssetUploader{
		client: client,
		uploader: transfermanager.New(client, func(options *transfermanager.Options) {
			options.PartSizeBytes = 16 * 1024 * 1024
			options.MultipartUploadThreshold = 16 * 1024 * 1024
			options.Concurrency = 1
			options.FailTimeout = 30 * time.Second
			options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}),
		bucket:          validated.Bucket,
		downloadBaseURL: baseURL,
	}, nil
}

func (u *ReleaseAssetUploader) Upload(ctx context.Context, input ReleaseAssetUploadInput, body io.Reader) (ReleaseAssetUpload, error) {
	input, err := validateReleaseUploadInput(input)
	if err != nil {
		return ReleaseAssetUpload{}, err
	}
	if body == nil {
		return ReleaseAssetUpload{}, ErrReleaseUploadInvalid
	}

	objectKey := strings.Join([]string{
		"releases",
		releaseVersionSegment(input.Version),
		input.Platform,
		input.Architecture,
		uuid.NewString(),
		input.FileName,
	}, "/")
	hasher := sha256.New()
	counter := &byteCounter{}
	limited := &io.LimitedReader{R: body, N: MaxReleaseAssetBytes + 1}
	stream := io.TeeReader(limited, io.MultiWriter(hasher, counter))
	_, err = u.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:      aws.String(u.bucket),
		Key:         aws.String(objectKey),
		ContentType: aws.String(input.ContentType),
		Body:        stream,
		Metadata:    map[string]string{"sha256": input.SHA256},
	})
	if err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("upload release asset to object storage: %w", err)
	}

	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if counter.value > MaxReleaseAssetBytes {
		u.deleteUploadedObject(objectKey)
		return ReleaseAssetUpload{}, ErrReleaseUploadTooLarge
	}
	if counter.value < 1 || input.FileSizeBytes > 0 && counter.value != input.FileSizeBytes {
		u.deleteUploadedObject(objectKey)
		return ReleaseAssetUpload{}, fmt.Errorf("%w: expected %d bytes, received %d", ErrReleaseUploadSizeMismatch, input.FileSizeBytes, counter.value)
	}
	if input.SHA256 != "" && actualSHA256 != input.SHA256 {
		u.deleteUploadedObject(objectKey)
		return ReleaseAssetUpload{}, fmt.Errorf("%w: expected %s, received %s", ErrReleaseUploadChecksumMismatch, input.SHA256, actualSHA256)
	}

	return ReleaseAssetUpload{
		ObjectKey: objectKey, DownloadURL: u.downloadBaseURL + "/" + escapeObjectKey(objectKey),
		FileSizeBytes: counter.value, SHA256: actualSHA256,
	}, nil
}

func (u *ReleaseAssetUploader) deleteUploadedObject(objectKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = u.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(u.bucket), Key: aws.String(objectKey),
	})
}

type byteCounter struct {
	value int64
}

func (c *byteCounter) Write(payload []byte) (int, error) {
	c.value += int64(len(payload))
	return len(payload), nil
}

func validateReleaseUploadInput(input ReleaseAssetUploadInput) (ReleaseAssetUploadInput, error) {
	input.Version = strings.TrimSpace(input.Version)
	input.Platform = strings.TrimSpace(input.Platform)
	input.Architecture = strings.TrimSpace(input.Architecture)
	input.FileName = strings.TrimSpace(input.FileName)
	input.SHA256 = strings.ToLower(strings.TrimSpace(input.SHA256))
	input.ContentType = strings.TrimSpace(input.ContentType)
	if input.ContentType == "" {
		input.ContentType = "application/octet-stream"
	}

	if !validUploadText(input.Version, 50) || !validUploadFileName(input.FileName) ||
		input.FileSizeBytes < 0 || input.FileSizeBytes > MaxReleaseAssetBytes ||
		input.SHA256 != "" && !releaseSHA256Pattern.MatchString(input.SHA256) {
		return ReleaseAssetUploadInput{}, ErrReleaseUploadInvalid
	}
	if input.Platform != "web" && input.Platform != "windows" && input.Platform != "macos" && input.Platform != "linux" &&
		input.Platform != "android" && input.Platform != "ios" {
		return ReleaseAssetUploadInput{}, ErrReleaseUploadInvalid
	}
	if input.Architecture != "x64" && input.Architecture != "arm64" && input.Architecture != "universal" {
		return ReleaseAssetUploadInput{}, ErrReleaseUploadInvalid
	}
	if len(input.ContentType) > 255 || strings.ContainsAny(input.ContentType, "\r\n") {
		return ReleaseAssetUploadInput{}, ErrReleaseUploadInvalid
	}
	if _, _, err := mime.ParseMediaType(input.ContentType); err != nil {
		return ReleaseAssetUploadInput{}, ErrReleaseUploadInvalid
	}
	return input, nil
}

func validUploadText(value string, maxRunes int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validUploadFileName(value string) bool {
	return validUploadText(value, 255) && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\`)
}

func releaseVersionSegment(version string) string {
	var builder strings.Builder
	lastWasDash := false
	for _, r := range strings.ToLower(version) {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if allowed {
			builder.WriteRune(r)
			lastWasDash = r == '-'
			continue
		}
		if !lastWasDash {
			builder.WriteByte('-')
			lastWasDash = true
		}
	}
	result := strings.Trim(builder.String(), ".-_")
	if result == "" {
		return "release"
	}
	return result
}

func resolveDownloadBaseURL(cfg S3Config, configured string) (string, error) {
	configured = strings.TrimRight(strings.TrimSpace(configured), "/")
	if configured != "" {
		parsed, err := url.Parse(configured)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", errors.New("DOWNLOAD_CDN_BASE_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		return configured, nil
	}

	endpoint, _ := url.Parse(cfg.Endpoint)
	if usePathStyle(cfg) {
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + url.PathEscape(cfg.Bucket)
	} else {
		host := endpoint.Host
		hostname := strings.ToLower(endpoint.Hostname())
		if strings.HasPrefix(hostname, "s3.oss-") && hostMatchesDomain(hostname, "aliyuncs.com") {
			host = strings.TrimPrefix(hostname, "s3.")
			if endpoint.Port() != "" {
				host = net.JoinHostPort(host, endpoint.Port())
			}
		}
		endpoint.Host = cfg.Bucket + "." + host
	}
	return strings.TrimRight(endpoint.String(), "/"), nil
}

func escapeObjectKey(objectKey string) string {
	segments := strings.Split(path.Clean(objectKey), "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return strings.Join(segments, "/")
}
