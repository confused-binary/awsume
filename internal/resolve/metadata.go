package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/confused-binary/awsume/internal/awscreds"
)

const (
	defaultIMDSEndpoint      = "http://169.254.169.254"
	defaultContainerEndpoint = "http://169.254.170.2"
	metadataTimeout          = 3 * time.Second
)

// metadataCredentials is the JSON document served by IMDS and by the
// container credential endpoint.
type metadataCredentials struct {
	Code            string `json:"Code"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func (m metadataCredentials) toCredentials(source string) (awscreds.Credentials, error) {
	creds := awscreds.Credentials{
		AccessKeyID:     m.AccessKeyID,
		SecretAccessKey: m.SecretAccessKey,
		SessionToken:    m.Token,
		Source:          source,
	}
	if err := creds.Validate(); err != nil {
		return creds, err
	}
	if m.Expiration != "" {
		if t, err := time.Parse(time.RFC3339, m.Expiration); err == nil {
			creds.Expiration = t
		}
	}
	return creds, nil
}

func (r *Resolver) metadataRequest(ctx context.Context, method, url string, headers map[string]string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// fromInstanceMetadata fetches credentials from the EC2 instance metadata
// service using IMDSv2.
func (r *Resolver) fromInstanceMetadata(ctx context.Context) (*Result, error) {
	if strings.EqualFold(r.env("AWS_EC2_METADATA_DISABLED"), "true") {
		return nil, errors.New("instance metadata disabled by AWS_EC2_METADATA_DISABLED")
	}
	base := strings.TrimRight(firstNonEmpty(r.env("AWS_EC2_METADATA_SERVICE_ENDPOINT"), defaultIMDSEndpoint), "/")
	r.logf("fetching credentials from instance metadata at %s", base)

	token, status, err := r.metadataRequest(ctx, http.MethodPut, base+"/latest/api/token", map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "21600"})
	if err != nil {
		return nil, fmt.Errorf("imds token: %w", err)
	}
	headers := map[string]string{}
	if status == http.StatusOK {
		headers["X-aws-ec2-metadata-token"] = string(token)
	}
	roleData, status, err := r.metadataRequest(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/", headers)
	if err != nil {
		return nil, fmt.Errorf("imds role name: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("imds role name: HTTP %d", status)
	}
	role := strings.TrimSpace(strings.SplitN(string(roleData), "\n", 2)[0])
	if role == "" {
		return nil, errors.New("imds: no IAM role attached to the instance")
	}
	data, status, err := r.metadataRequest(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/"+role, headers)
	if err != nil {
		return nil, fmt.Errorf("imds credentials: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("imds credentials: HTTP %d", status)
	}
	var m metadataCredentials
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("imds credentials: decode: %w", err)
	}
	if m.Code != "" && m.Code != "Success" {
		return nil, fmt.Errorf("imds credentials: %s", m.Code)
	}
	creds, err := m.toCredentials("instance-metadata")
	if err != nil {
		return nil, fmt.Errorf("imds credentials: %w", err)
	}
	region := r.envRegion()
	if region == "" {
		if az, status, err := r.metadataRequest(ctx, http.MethodGet, base+"/latest/meta-data/placement/region", headers); err == nil && status == http.StatusOK {
			region = strings.TrimSpace(string(az))
		}
	}
	return &Result{
		Credentials: creds,
		Region:      region,
		Partition:   r.partitionFor(region),
		Chain:       []string{"instance metadata role " + role},
	}, nil
}

// fromContainer fetches credentials from the ECS/EKS container credential
// endpoint.
func (r *Resolver) fromContainer(ctx context.Context) (*Result, error) {
	var endpoint string
	switch {
	case r.env("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "":
		endpoint = r.env("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	case r.env("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "":
		endpoint = defaultContainerEndpoint + r.env("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	default:
		return nil, errors.New("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI or AWS_CONTAINER_CREDENTIALS_FULL_URI is not set")
	}
	headers := map[string]string{}
	if tokenFile := r.env("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); tokenFile != "" {
		tok, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read container authorization token: %w", err)
		}
		headers["Authorization"] = strings.TrimSpace(string(tok))
	} else if tok := r.env("AWS_CONTAINER_AUTHORIZATION_TOKEN"); tok != "" {
		headers["Authorization"] = tok
	}
	r.logf("fetching credentials from container endpoint %s", endpoint)
	data, status, err := r.metadataRequest(ctx, http.MethodGet, endpoint, headers)
	if err != nil {
		return nil, fmt.Errorf("container credentials: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("container credentials: HTTP %d", status)
	}
	var m metadataCredentials
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("container credentials: decode: %w", err)
	}
	creds, err := m.toCredentials("container")
	if err != nil {
		return nil, fmt.Errorf("container credentials: %w", err)
	}
	region := r.envRegion()
	return &Result{
		Credentials: creds,
		Region:      region,
		Partition:   r.partitionFor(region),
		Chain:       []string{"container credentials"},
	}, nil
}
