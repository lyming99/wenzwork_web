package objectstore

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func newS3Client(cfg S3Config, httpClient aws.HTTPClient) (*s3.Client, S3Config, error) {
	validated, err := validateS3Config(cfg)
	if err != nil {
		return nil, S3Config{}, err
	}

	awsCfg := aws.Config{
		Region: validated.Region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			validated.AccessKeyID,
			validated.SecretAccessKey,
			validated.SessionToken,
		)),
		HTTPClient: httpClient,
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(options *retry.StandardOptions) {
				options.MaxAttempts = 2
			})
		},
	}

	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(validated.Endpoint)
		options.UsePathStyle = usePathStyle(validated)
	})
	return client, validated, nil
}
