package scalers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	kedav1alpha1 "github.com/kedacore/keda/pkg/apis/keda/v1alpha1"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	v2beta1 "k8s.io/api/autoscaling/v2beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	awsSqsQueueMetricName    = "ApproximateNumberOfMessages"
	awsAccessKeyIDEnvVar     = "AWS_ACCESS_KEY_ID"
	awsSecretAccessKeyEnvVar = "AWS_SECRET_ACCESS_KEY"
	targetQueueLengthDefault = 5
)

type awsSqsQueueScaler struct {
	metadata *awsSqsQueueMetadata
}

type awsSqsQueueMetadata struct {
	targetQueueLength  int
	queueURL           string
	awsRegion          string
	awsAccessKeyID     string
	awsSecretAccessKey string
	awsSessionToken    string
	awsRoleArn         string
}

var sqsQueueLog = logf.Log.WithName("aws_sqs_queue_scaler")

// NewAwsSqsQueueScaler creates a new awsSqsQueueScaler
func NewAwsSqsQueueScaler(resolvedEnv, resolvedAnnotations, metadata, authParams map[string]string, podIdentity string) (Scaler, error) {
	meta, err := parseAwsSqsQueueMetadata(metadata, resolvedAnnotations, authParams, podIdentity)
	if err != nil {
		return nil, fmt.Errorf("Error parsing SQS queue metadata: %s", err)
	}

	sess := session.Must(session.NewSession())
	if sess != nil {
		s, err := session.NewSession(&aws.Config{
			Credentials: credentials.NewStaticCredentials(meta.awsAccessKeyID, meta.awsSecretAccessKey, meta.awsSessionToken),
			Region:      aws.String(meta.awsRegion),
		})

		if err != nil {
			return nil, errors.New("unable to get an AWS session with the default provider chain or provided credentials")
		}

		sess = s
	}

	return &awsSqsQueueScaler{
		metadata: meta,
	}, nil
}

func parseAwsSqsQueueMetadata(metadata, resolvedAnnotations, authParams map[string]string, podIdentity string) (*awsSqsQueueMetadata, error) {
	meta := awsSqsQueueMetadata{}
	meta.targetQueueLength = defaultTargetQueueLength

	if val, ok := metadata["queueLength"]; ok && val != "" {
		queueLength, err := strconv.Atoi(val)
		if err != nil {
			meta.targetQueueLength = targetQueueLengthDefault
			sqsQueueLog.Error(err, "Error parsing SQS queue metadata queueLength, using default %n", targetQueueLengthDefault)
		} else {
			meta.targetQueueLength = queueLength
		}
	}

	if val, ok := metadata["queueURL"]; ok && val != "" {
		meta.queueURL = val
	} else {
		return nil, fmt.Errorf("no queueURL given")
	}

	if val, ok := metadata["awsRegion"]; ok && val != "" {
		meta.awsRegion = val
	} else {
		return nil, fmt.Errorf("no awsRegion given")
	}

	if podIdentity == "kiam" {
		meta.awsRoleArn = resolvedAnnotations[kedav1alpha1.PodIdentiyAnnotationKiam]
	} else {
		if authParams["awsRoleArn"] != "" {
			meta.awsRoleArn = authParams["awsRoleArn"]
		} else if authParams["awsAccessKeyId"] != "" && authParams["awsSecretAccessKey"] != "" {
			meta.awsAccessKeyID = authParams["awsAccessKeyId"]
			meta.awsSecretAccessKey = authParams["awsSecretAccessKey"]
		} else {
			return nil, fmt.Errorf("No authentication was found")
		}
	}

	return &meta, nil
}

// IsActive determines if we need to scale from zero
func (s *awsSqsQueueScaler) IsActive(ctx context.Context) (bool, error) {
	length, err := s.GetAwsSqsQueueLength()

	if err != nil {
		return false, err
	}

	return length > 0, nil
}

func (s *awsSqsQueueScaler) Close() error {
	return nil
}

func (s *awsSqsQueueScaler) GetMetricSpecForScaling() []v2beta1.MetricSpec {
	targetQueueLengthQty := resource.NewQuantity(int64(s.metadata.targetQueueLength), resource.DecimalSI)
	externalMetric := &v2beta1.ExternalMetricSource{MetricName: awsSqsQueueMetricName, TargetAverageValue: targetQueueLengthQty}
	metricSpec := v2beta1.MetricSpec{External: externalMetric, Type: externalMetricType}
	return []v2beta1.MetricSpec{metricSpec}
}

//GetMetrics returns value for a supported metric and an error if there is a problem getting the metric
func (s *awsSqsQueueScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	queuelen, err := s.GetAwsSqsQueueLength()

	if err != nil {
		sqsQueueLog.Error(err, "Error getting queue length")
		return []external_metrics.ExternalMetricValue{}, err
	}

	metric := external_metrics.ExternalMetricValue{
		MetricName: metricName,
		Value:      *resource.NewQuantity(int64(queuelen), resource.DecimalSI),
		Timestamp:  metav1.Now(),
	}

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

// Get SQS Queue Length
func (s *awsSqsQueueScaler) GetAwsSqsQueueLength() (int32, error) {
	input := &sqs.GetQueueAttributesInput{
		AttributeNames: aws.StringSlice([]string{"ApproximateNumberOfMessages"}),
		QueueUrl:       aws.String(s.metadata.queueURL),
	}

	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(s.metadata.awsRegion),
	}))
	creds := credentials.NewStaticCredentials(s.metadata.awsAccessKeyID, s.metadata.awsSecretAccessKey, "")

	if s.metadata.awsRoleArn != "" {
		creds = stscreds.NewCredentials(sess, s.metadata.awsRoleArn)
	}

	sqsClient := sqs.New(sess, &aws.Config{
		Region:      aws.String(s.metadata.awsRegion),
		Credentials: creds,
	})

	output, err := sqsClient.GetQueueAttributes(input)
	if err != nil {
		return -1, err
	}

	approximateNumberOfMessages, err := strconv.Atoi(*output.Attributes["ApproximateNumberOfMessages"])
	if err != nil {
		return -1, err
	}

	return int32(approximateNumberOfMessages), nil
}
