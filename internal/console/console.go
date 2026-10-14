// Package console builds AWS Management Console destination URLs for a
// region and/or service, so the federated sign-in lands the user where they
// asked to go.
package console

import (
	"sort"
	"strings"
)

// servicePaths maps friendly service names to console paths. Services that
// are not listed fall back to "/<name>/home", which is correct for most
// consoles.
var servicePaths = map[string]string{
	"":                "/console/home",
	"home":            "/console/home",
	"console":         "/console/home",
	"acm":             "/acm/home",
	"apigateway":      "/apigateway/main/apis",
	"athena":          "/athena/home",
	"backup":          "/backup/home",
	"batch":           "/batch/home",
	"bedrock":         "/bedrock/home",
	"billing":         "/billing/home",
	"cloudformation":  "/cloudformation/home",
	"cloudfront":      "/cloudfront/v4/home",
	"cloudshell":      "/cloudshell/home",
	"cloudtrail":      "/cloudtrailv2/home",
	"cloudwatch":      "/cloudwatch/home",
	"codebuild":       "/codesuite/codebuild/projects",
	"codepipeline":    "/codesuite/codepipeline/pipelines",
	"cognito":         "/cognito/v2/idp/user-pools",
	"config":          "/config/home",
	"detective":       "/detective/home",
	"dynamodb":        "/dynamodbv2/home",
	"ec2":             "/ec2/home",
	"ecr":             "/ecr/repositories",
	"ecs":             "/ecs/v2/home",
	"efs":             "/efs/home",
	"eks":             "/eks/home",
	"elasticache":     "/elasticache/home",
	"elb":             "/ec2/home#LoadBalancers:",
	"eventbridge":     "/events/home",
	"glue":            "/glue/home",
	"guardduty":       "/guardduty/home",
	"iam":             "/iam/home",
	"identitycenter":  "/singlesignon/home",
	"inspector":       "/inspector/v2/home",
	"kinesis":         "/kinesis/home",
	"kms":             "/kms/home",
	"lambda":          "/lambda/home",
	"lightsail":       "/lightsail/home",
	"logs":            "/cloudwatch/home#logsV2:log-groups",
	"macie":           "/macie/home",
	"organizations":   "/organizations/v2/home",
	"rds":             "/rds/home",
	"redshift":        "/redshiftv2/home",
	"route53":         "/route53/v2/home",
	"s3":              "/s3/home",
	"sagemaker":       "/sagemaker/home",
	"secretsmanager":  "/secretsmanager/home",
	"securityhub":     "/securityhub/home",
	"ses":             "/ses/home",
	"sns":             "/sns/v3/home",
	"sqs":             "/sqs/v3/home",
	"ssm":             "/systems-manager/home",
	"systems-manager": "/systems-manager/home",
	"stepfunctions":   "/states/home",
	"sts":             "/iam/home",
	"support":         "/support/home",
	"vpc":             "/vpcconsole/home",
	"waf":             "/wafv2/homev2",
}

// KnownServices lists the friendly service names with explicit mappings.
func KnownServices() []string {
	names := make([]string, 0, len(servicePaths))
	for n := range servicePaths {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// Destination builds the console URL for the given base (for example
// "https://console.aws.amazon.com"), region and service. When regionalHost
// is true and a region is set, the region is inserted into the host name as
// the commercial console does. A service value that starts with "/" is used
// as a literal path.
func Destination(base string, regionalHost bool, region, service string) string {
	region = strings.TrimSpace(region)
	service = strings.ToLower(strings.TrimSpace(service))

	path := service
	if !strings.HasPrefix(service, "/") {
		var ok bool
		if path, ok = servicePaths[service]; !ok {
			path = "/" + service + "/home"
		}
	}
	return regionalURL(base, regionalHost, region, path, "")
}

func joinQuery(path, query string) string {
	if strings.Contains(path, "?") {
		return path + "&" + query
	}
	return path + "?" + query
}
