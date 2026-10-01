// Package storage lưu file xuất (export_bot) lên object storage tương thích S3 (M2, S2.4.1 DYN-74).
// Local dùng SeaweedFS (deploy/docker-compose.yml, S3 API cổng 8333); không có credentials → anonymous
// (SeaweedFS mặc định). URL tải là đường dẫn công khai path-style: <endpoint>/<bucket>/<key>.
// Production sau này chuyển bucket riêng tư + link presigned khi cần.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 là client upload tối giản cho một bucket.
type S3 struct {
	bucket    string
	client    *s3.Client
	publicURL string // cơ sở để dựng URL tải: "<endpoint>/<bucket>"
}

// New dựng client. endpoint vd "http://localhost:8333" (nội bộ); publicURL là cơ sở cho link tải
// (BIVA_S3_PUBLIC_URL, vd "http://localhost:8333" khi host truy cập qua cổng map) — trống thì dùng endpoint.
// accessKey/secret trống = anonymous.
func New(endpoint, publicURL, bucket, accessKey, secret string) (*S3, error) {
	if endpoint == "" || bucket == "" {
		return nil, errors.New("thiếu endpoint hoặc bucket của S3")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("endpoint S3 không hợp lệ: %q", endpoint)
	}
	if publicURL == "" {
		publicURL = endpoint
	}
	var creds aws.CredentialsProvider
	if accessKey != "" && secret != "" {
		creds = credentials.NewStaticCredentialsProvider(accessKey, secret, "")
	} else {
		creds = aws.AnonymousCredentials{}
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"), // SeaweedFS/MinIO bỏ qua region nhưng SDK bắt buộc có
		awsconfig.WithCredentialsProvider(creds))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true // SeaweedFS/MinIO dùng path-style
		// endpoint tự host (http) + stream không seekable: bỏ checksum linh hoạt như AWS SDK khuyến nghị
		// cho dịch vụ tương thích S3 (https://github.com/aws/aws-sdk-go-v2 — SetRequestChecksumCalculation)
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3{bucket: bucket, client: client,
		publicURL: strings.TrimRight(publicURL, "/") + "/" + bucket}, nil
}

// EnsureBucket tạo bucket nếu chưa có (gọi một lần lúc khởi động).
func (s *S3) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	var nf *types.NotFound
	if err == nil || !errors.As(err, &nf) {
		return err
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

// Upload ghi body vào key, trả URL tải công khai.
func (s *S3) Upload(ctx context.Context, key, contentType string, body []byte) (string, error) {
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
		Body:        bytes.NewReader(body),
	}); err != nil {
		return "", fmt.Errorf("upload %s: %w", key, err)
	}
	return s.publicURL + "/" + key, nil
}

// ContentType của các định dạng export_bot xuất.
func ContentType(format string) string {
	switch format {
	case "json":
		return "application/json"
	case "markdown":
		return "text/markdown; charset=utf-8"
	case "faq_csv":
		return "text/csv; charset=utf-8"
	}
	return "application/octet-stream"
}
