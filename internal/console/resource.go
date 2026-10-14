package console

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/confused-binary/awsume/internal/arn"
)

// ResourceDestination builds a console deep link to the resource named by
// a. The region comes from the ARN; fallbackRegion is used for resources
// whose ARN carries no region but whose console is regional (for example
// S3 buckets). Global services (IAM, Route 53, CloudFront, Organizations)
// get a region-less URL. Services without a known deep link land on the
// service's console home.
func ResourceDestination(base string, regionalHost bool, a arn.ARN, fallbackRegion string) string {
	region := a.Region
	if region == "" {
		region = fallbackRegion
	}
	typ, id := a.Split()
	esc := url.PathEscape

	// Global services: no region on the host or in the query.
	switch a.Service {
	case "iam":
		return globalURL(base, iamPath(a, typ, id))
	case "sts":
		if typ == "assumed-role" {
			role := strings.SplitN(id, "/", 2)[0]
			return globalURL(base, "/iam/home#/roles/details/"+esc(role))
		}
		return globalURL(base, "/iam/home")
	case "route53":
		if typ == "hostedzone" {
			return globalURL(base, "/route53/v2/hostedzones#ListRecordSets/"+esc(id))
		}
		return globalURL(base, "/route53/v2/home")
	case "cloudfront":
		if typ == "distribution" {
			return globalURL(base, "/cloudfront/v4/home#/distributions/"+esc(id))
		}
		return globalURL(base, "/cloudfront/v4/home")
	case "organizations":
		if typ == "account" {
			parts := strings.Split(id, "/")
			return globalURL(base, "/organizations/v2/home/accounts/"+esc(parts[len(parts)-1]))
		}
		return globalURL(base, "/organizations/v2/home")
	case "s3":
		bucket, key := typ, id
		if bucket == "" {
			bucket = id
			key = ""
		}
		switch {
		case key == "":
			return regionalURL(base, regionalHost, region, "/s3/buckets/"+esc(bucket), "tab=objects")
		case strings.HasSuffix(key, "/"):
			return regionalURL(base, regionalHost, region, "/s3/buckets/"+esc(bucket), "prefix="+url.QueryEscape(key))
		default:
			return regionalURL(base, regionalHost, region, "/s3/object/"+esc(bucket), "prefix="+url.QueryEscape(key))
		}
	}

	path := resourcePath(a, typ, id, esc)
	if path == "" {
		return Destination(base, regionalHost, region, a.Service)
	}
	return regionalURL(base, regionalHost, region, path, "")
}

var secretSuffix = regexp.MustCompile(`-[A-Za-z0-9]{6}$`)

func iamPath(a arn.ARN, typ, id string) string {
	typ = strings.ToLower(typ)
	name := id
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		name = id[i+1:]
	}
	switch typ {
	case "user":
		return "/iam/home#/users/details/" + url.PathEscape(name)
	case "role":
		return "/iam/home#/roles/details/" + url.PathEscape(name)
	case "group":
		return "/iam/home#/groups/details/" + url.PathEscape(name)
	case "policy":
		return "/iam/home#/policies/details/" + url.PathEscape(a.String)
	case "mfa", "sms-mfa", "u2f":
		return "/iam/home#/security_credentials"
	case "instance-profile":
		return "/iam/home#/roles"
	case "saml-provider", "oidc-provider":
		return "/iam/home#/identity_providers"
	}
	return "/iam/home"
}

// resourcePath returns the region-relative console path for regional
// services, or "" when no deep link is known.
func resourcePath(a arn.ARN, typ, id string, esc func(string) string) string {
	typ = strings.ToLower(typ)
	switch a.Service {
	case "ec2":
		switch typ {
		case "instance":
			return "/ec2/home#InstanceDetails:instanceId=" + id
		case "security-group":
			return "/ec2/home#SecurityGroup:groupId=" + id
		case "volume":
			return "/ec2/home#VolumeDetails:volumeId=" + id
		case "snapshot":
			return "/ec2/home#SnapshotDetails:snapshotId=" + id
		case "image":
			return "/ec2/home#ImageDetails:imageId=" + id
		case "key-pair":
			return "/ec2/home#KeyPairs:keyName=" + id
		case "launch-template":
			return "/ec2/home#LaunchTemplateDetails:launchTemplateId=" + id
		case "network-interface":
			return "/ec2/home#NetworkInterface:networkInterfaceId=" + id
		case "elastic-ip":
			return "/ec2/home#ElasticIpDetails:AllocationId=" + id
		case "vpc":
			return "/vpcconsole/home#VpcDetails:VpcId=" + id
		case "subnet":
			return "/vpcconsole/home#SubnetDetails:subnetId=" + id
		case "route-table":
			return "/vpcconsole/home#RouteTableDetails:RouteTableId=" + id
		case "internet-gateway":
			return "/vpcconsole/home#InternetGateway:internetGatewayId=" + id
		case "natgateway":
			return "/vpcconsole/home#NatGatewayDetails:natGatewayId=" + id
		case "vpc-endpoint":
			return "/vpcconsole/home#Endpoints:vpcEndpointId=" + id
		case "transit-gateway":
			return "/vpcconsole/home#TransitGatewayDetails:transitGatewayId=" + id
		}
		return "/ec2/home"
	case "autoscaling":
		if i := strings.Index(a.Resource, "autoScalingGroupName/"); i >= 0 {
			return "/ec2/home#AutoScalingGroupDetails:id=" + esc(a.Resource[i+len("autoScalingGroupName/"):])
		}
		return "/ec2/home#AutoScalingGroups:"
	case "elasticloadbalancing":
		switch typ {
		case "loadbalancer":
			return "/ec2/home#LoadBalancer:loadBalancerArn=" + url.QueryEscape(a.String)
		case "targetgroup":
			return "/ec2/home#TargetGroup:targetGroupArn=" + url.QueryEscape(a.String)
		}
		return "/ec2/home#LoadBalancers:"
	case "lambda":
		if typ == "function" {
			name, version, hasVersion := strings.Cut(id, ":")
			if hasVersion && version != "" {
				return "/lambda/home#/functions/" + esc(name) + "/versions/" + esc(version)
			}
			return "/lambda/home#/functions/" + esc(name)
		}
		if typ == "layer" {
			return "/lambda/home#/layers"
		}
		return "/lambda/home"
	case "sqs":
		queueURL := "https://sqs." + a.Region + "." + dnsSuffix(a.Partition) + "/" + a.Account + "/" + a.Resource
		return "/sqs/v3/home#/queues/" + url.QueryEscape(queueURL)
	case "sns":
		return "/sns/v3/home#/topic/" + a.String
	case "dynamodb":
		if typ == "table" {
			name := strings.SplitN(id, "/", 2)[0]
			return "/dynamodbv2/home#table?name=" + esc(name)
		}
		return "/dynamodbv2/home"
	case "rds":
		switch typ {
		case "db":
			return "/rds/home#database:id=" + esc(id) + ";is-cluster=false"
		case "cluster":
			return "/rds/home#database:id=" + esc(id) + ";is-cluster=true"
		case "snapshot":
			return "/rds/home#db-snapshot:id=" + esc(id)
		case "cluster-snapshot":
			return "/rds/home#db-snapshot:engine=aurora;id=" + esc(id)
		}
		return "/rds/home"
	case "cloudformation":
		if typ == "stack" {
			return "/cloudformation/home#/stacks/stackinfo?stackId=" + url.QueryEscape(a.String)
		}
		return "/cloudformation/home"
	case "logs":
		if typ == "log-group" {
			name := strings.TrimSuffix(id, ":*")
			return "/cloudwatch/home#logsV2:log-groups/log-group/" + cloudwatchEscape(name)
		}
		return "/cloudwatch/home#logsV2:log-groups"
	case "cloudwatch":
		if typ == "alarm" {
			return "/cloudwatch/home#alarmsV2:alarm/" + esc(id)
		}
		return "/cloudwatch/home"
	case "secretsmanager":
		if typ == "secret" {
			name := secretSuffix.ReplaceAllString(id, "")
			return "/secretsmanager/secret?name=" + url.QueryEscape(name)
		}
		return "/secretsmanager/home"
	case "ssm":
		if typ == "parameter" {
			name := id
			if !strings.HasPrefix(name, "/") {
				name = "/" + name
			}
			return "/systems-manager/parameters" + esc(name) + "/description"
		}
		if typ == "document" {
			return "/systems-manager/documents/" + esc(id) + "/description"
		}
		return "/systems-manager/home"
	case "kms":
		if typ == "key" {
			return "/kms/home#/kms/keys/" + esc(id)
		}
		if typ == "alias" {
			return "/kms/home#/kms/defaultKeys"
		}
		return "/kms/home"
	case "ecs":
		parts := strings.Split(id, "/")
		switch {
		case typ == "cluster":
			return "/ecs/v2/clusters/" + esc(parts[0])
		case typ == "service" && len(parts) >= 2:
			return "/ecs/v2/clusters/" + esc(parts[0]) + "/services/" + esc(parts[1])
		case typ == "task" && len(parts) >= 2:
			return "/ecs/v2/clusters/" + esc(parts[0]) + "/tasks/" + esc(parts[1])
		case typ == "task-definition":
			family, rev, _ := strings.Cut(parts[0], ":")
			if rev != "" {
				return "/ecs/v2/task-definitions/" + esc(family) + "/" + esc(rev)
			}
			return "/ecs/v2/task-definitions/" + esc(family)
		}
		return "/ecs/v2/home"
	case "eks":
		if typ == "cluster" {
			return "/eks/home#/clusters/" + esc(id)
		}
		return "/eks/home"
	case "ecr":
		if typ == "repository" {
			return "/ecr/repositories/private/" + esc(a.Account) + "/" + id
		}
		return "/ecr/repositories"
	case "cloudtrail":
		if typ == "trail" {
			return "/cloudtrailv2/home#/trails/" + a.String
		}
		return "/cloudtrailv2/home"
	case "states":
		switch typ {
		case "statemachine":
			return "/states/home#/statemachines/view/" + a.String
		case "execution":
			return "/states/home#/executions/details/" + a.String
		}
		return "/states/home"
	case "events":
		if typ == "rule" {
			bus, rule, hasBus := strings.Cut(id, "/")
			if !hasBus {
				bus, rule = "default", id
			}
			return "/events/home#/eventbus/" + esc(bus) + "/rules/" + esc(rule)
		}
		return "/events/home"
	case "codepipeline":
		return "/codesuite/codepipeline/pipelines/" + esc(a.Resource) + "/view"
	case "codecommit":
		return "/codesuite/codecommit/repositories/" + esc(a.Resource) + "/browse"
	case "codebuild":
		if typ == "project" {
			return "/codesuite/codebuild/" + esc(a.Account) + "/projects/" + esc(id)
		}
		return "/codesuite/codebuild/projects"
	case "cognito-idp":
		if typ == "userpool" {
			return "/cognito/v2/idp/user-pools/" + esc(id) + "/users"
		}
		return "/cognito/v2/idp/user-pools"
	case "apigateway":
		if parts := strings.Split(strings.TrimPrefix(a.Resource, "/"), "/"); len(parts) >= 2 && parts[0] == "restapis" {
			return "/apigateway/main/apis/" + esc(parts[1]) + "/resources?api=" + esc(parts[1])
		}
		return "/apigateway/main/apis"
	case "acm":
		if typ == "certificate" {
			return "/acm/home#/certificates/" + esc(id)
		}
		return "/acm/home"
	case "glue":
		switch typ {
		case "database":
			return "/glue/home#/v2/data-catalog/databases/view/" + esc(id)
		case "table":
			db, table, _ := strings.Cut(id, "/")
			return "/glue/home#/v2/data-catalog/tables/view/" + esc(table) + "?database=" + esc(db)
		case "job":
			return "/gluestudio/home#/editor/job/" + esc(id) + "/details"
		}
		return "/glue/home"
	case "elasticache":
		return "/elasticache/home"
	case "redshift":
		if typ == "cluster" {
			return "/redshiftv2/home#cluster-details?cluster=" + esc(id)
		}
		return "/redshiftv2/home"
	case "backup":
		return "/backup/home"
	case "guardduty":
		if typ == "detector" {
			return "/guardduty/home#/findings"
		}
		return "/guardduty/home"
	case "sagemaker":
		if typ == "notebook-instance" {
			return "/sagemaker/home#/notebook-instances/" + esc(id)
		}
		return "/sagemaker/home"
	case "elasticfilesystem":
		if typ == "file-system" {
			return "/efs/home#/file-systems/" + esc(id)
		}
		return "/efs/home"
	case "kinesis":
		if typ == "stream" {
			return "/kinesis/home#/streams/details/" + esc(id) + "/monitoring"
		}
		return "/kinesis/home"
	case "es", "opensearch":
		if typ == "domain" {
			return "/aos/home#opensearch/domains/" + esc(id)
		}
		return "/aos/home"
	}
	return ""
}

func dnsSuffix(partition string) string {
	if partition == "aws-cn" {
		return "amazonaws.com.cn"
	}
	return "amazonaws.com"
}

// cloudwatchEscape applies the double encoding the CloudWatch console uses
// in log group URLs ("/" becomes "$252F").
func cloudwatchEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "%", "$25")
}

func globalURL(base, path string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + path
}

// regionalURL joins base, path and region, inserting the region into the
// host for the commercial partition and placing "region=" (and any extra
// query) before a fragment.
func regionalURL(base string, regionalHost bool, region, path, extraQuery string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if region != "" && regionalHost {
		if u, err := url.Parse(base); err == nil && u.Host != "" && !strings.HasPrefix(u.Host, region+".") {
			u.Host = region + "." + u.Host
			base = strings.TrimRight(u.String(), "/")
		}
	}
	query := extraQuery
	if region != "" {
		if query != "" {
			query += "&"
		}
		query += "region=" + url.QueryEscape(region)
	}
	if query == "" {
		return base + path
	}
	if i := strings.Index(path, "#"); i >= 0 {
		return base + joinQuery(path[:i], query) + path[i:]
	}
	return base + joinQuery(path, query)
}
