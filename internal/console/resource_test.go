package console

import (
	"testing"

	"awsume/internal/arn"
)

func TestResourceDestination(t *testing.T) {
	const base = "https://console.aws.amazon.com"
	cases := []struct {
		arn, fallback, want string
	}{
		{"arn:aws:ec2:us-east-1:123456789012:instance/i-0abc", "", "https://us-east-1.console.aws.amazon.com/ec2/home?region=us-east-1#InstanceDetails:instanceId=i-0abc"},
		{"arn:aws:ec2:eu-west-1:123456789012:security-group/sg-1", "", "https://eu-west-1.console.aws.amazon.com/ec2/home?region=eu-west-1#SecurityGroup:groupId=sg-1"},
		{"arn:aws:ec2:eu-west-1:123456789012:vpc/vpc-1", "", "https://eu-west-1.console.aws.amazon.com/vpcconsole/home?region=eu-west-1#VpcDetails:VpcId=vpc-1"},
		{"arn:aws:ec2:eu-west-1:123456789012:something-new/x", "", "https://eu-west-1.console.aws.amazon.com/ec2/home?region=eu-west-1"},
		{"arn:aws:s3:::my-bucket", "us-west-2", "https://us-west-2.console.aws.amazon.com/s3/buckets/my-bucket?tab=objects&region=us-west-2"},
		{"arn:aws:s3:::my-bucket", "", "https://console.aws.amazon.com/s3/buckets/my-bucket?tab=objects"},
		{"arn:aws:s3:::my-bucket/dir/file.txt", "", "https://console.aws.amazon.com/s3/object/my-bucket?prefix=dir%2Ffile.txt"},
		{"arn:aws:s3:::my-bucket/dir/", "", "https://console.aws.amazon.com/s3/buckets/my-bucket?prefix=dir%2F"},
		{"arn:aws:iam::123456789012:user/alice", "us-east-1", base + "/iam/home#/users/details/alice"},
		{"arn:aws:iam::123456789012:role/service-role/MyRole", "", base + "/iam/home#/roles/details/MyRole"},
		{"arn:aws:iam::123456789012:policy/MyPolicy", "", base + "/iam/home#/policies/details/arn:aws:iam::123456789012:policy%2FMyPolicy"},
		{"arn:aws:iam::123456789012:group/admins", "", base + "/iam/home#/groups/details/admins"},
		{"arn:aws:sts::123456789012:assumed-role/MyRole/session", "", base + "/iam/home#/roles/details/MyRole"},
		{"arn:aws:lambda:us-east-2:123456789012:function:my-fn", "", "https://us-east-2.console.aws.amazon.com/lambda/home?region=us-east-2#/functions/my-fn"},
		{"arn:aws:lambda:us-east-2:123456789012:function:my-fn:7", "", "https://us-east-2.console.aws.amazon.com/lambda/home?region=us-east-2#/functions/my-fn/versions/7"},
		{"arn:aws:sqs:us-east-1:123456789012:my-queue", "", "https://us-east-1.console.aws.amazon.com/sqs/v3/home?region=us-east-1#/queues/https%3A%2F%2Fsqs.us-east-1.amazonaws.com%2F123456789012%2Fmy-queue"},
		{"arn:aws:sns:us-east-1:123456789012:my-topic", "", "https://us-east-1.console.aws.amazon.com/sns/v3/home?region=us-east-1#/topic/arn:aws:sns:us-east-1:123456789012:my-topic"},
		{"arn:aws:dynamodb:us-east-1:123456789012:table/Orders", "", "https://us-east-1.console.aws.amazon.com/dynamodbv2/home?region=us-east-1#table?name=Orders"},
		{"arn:aws:rds:us-east-1:123456789012:db:prod-db", "", "https://us-east-1.console.aws.amazon.com/rds/home?region=us-east-1#database:id=prod-db;is-cluster=false"},
		{"arn:aws:rds:us-east-1:123456789012:cluster:prod", "", "https://us-east-1.console.aws.amazon.com/rds/home?region=us-east-1#database:id=prod;is-cluster=true"},
		{"arn:aws:cloudformation:us-east-1:123456789012:stack/my-stack/abc-123", "", "https://us-east-1.console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/stackinfo?stackId=arn%3Aaws%3Acloudformation%3Aus-east-1%3A123456789012%3Astack%2Fmy-stack%2Fabc-123"},
		{"arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/fn:*", "", "https://us-east-1.console.aws.amazon.com/cloudwatch/home?region=us-east-1#logsV2:log-groups/log-group/$252Faws$252Flambda$252Ffn"},
		{"arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/db-AbC123", "", "https://us-east-1.console.aws.amazon.com/secretsmanager/secret?name=prod%2Fdb&region=us-east-1"},
		{"arn:aws:ssm:us-east-1:123456789012:parameter/my/param", "", "https://us-east-1.console.aws.amazon.com/systems-manager/parameters%2Fmy%2Fparam/description?region=us-east-1"},
		{"arn:aws:kms:us-east-1:123456789012:key/1234-abcd", "", "https://us-east-1.console.aws.amazon.com/kms/home?region=us-east-1#/kms/keys/1234-abcd"},
		{"arn:aws:ecs:us-east-1:123456789012:service/my-cluster/my-svc", "", "https://us-east-1.console.aws.amazon.com/ecs/v2/clusters/my-cluster/services/my-svc?region=us-east-1"},
		{"arn:aws:ecs:us-east-1:123456789012:task-definition/web:12", "", "https://us-east-1.console.aws.amazon.com/ecs/v2/task-definitions/web/12?region=us-east-1"},
		{"arn:aws:eks:us-east-1:123456789012:cluster/prod", "", "https://us-east-1.console.aws.amazon.com/eks/home?region=us-east-1#/clusters/prod"},
		{"arn:aws:ecr:us-east-1:123456789012:repository/team/app", "", "https://us-east-1.console.aws.amazon.com/ecr/repositories/private/123456789012/team/app?region=us-east-1"},
		{"arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/50dc6c", "", "https://us-east-1.console.aws.amazon.com/ec2/home?region=us-east-1#LoadBalancer:loadBalancerArn=arn%3Aaws%3Aelasticloadbalancing%3Aus-east-1%3A123456789012%3Aloadbalancer%2Fapp%2Fmy-alb%2F50dc6c"},
		{"arn:aws:autoscaling:us-east-1:123456789012:autoScalingGroup:uuid:autoScalingGroupName/my-asg", "", "https://us-east-1.console.aws.amazon.com/ec2/home?region=us-east-1#AutoScalingGroupDetails:id=my-asg"},
		{"arn:aws:states:us-east-1:123456789012:stateMachine:Flow", "", "https://us-east-1.console.aws.amazon.com/states/home?region=us-east-1#/statemachines/view/arn:aws:states:us-east-1:123456789012:stateMachine:Flow"},
		{"arn:aws:events:us-east-1:123456789012:rule/my-rule", "", "https://us-east-1.console.aws.amazon.com/events/home?region=us-east-1#/eventbus/default/rules/my-rule"},
		{"arn:aws:route53:::hostedzone/Z123", "us-east-1", base + "/route53/v2/hostedzones#ListRecordSets/Z123"},
		{"arn:aws:cloudfront::123456789012:distribution/E123", "", base + "/cloudfront/v4/home#/distributions/E123"},
		{"arn:aws:organizations::123456789012:account/o-abc/111111111111", "", base + "/organizations/v2/home/accounts/111111111111"},
		{"arn:aws:cloudtrail:us-east-1:123456789012:trail/main", "", "https://us-east-1.console.aws.amazon.com/cloudtrailv2/home?region=us-east-1#/trails/arn:aws:cloudtrail:us-east-1:123456789012:trail/main"},
		{"arn:aws:codepipeline:us-east-1:123456789012:my-pipeline", "", "https://us-east-1.console.aws.amazon.com/codesuite/codepipeline/pipelines/my-pipeline/view?region=us-east-1"},
		{"arn:aws:apigateway:us-east-1::/restapis/abc123/stages/prod", "", "https://us-east-1.console.aws.amazon.com/apigateway/main/apis/abc123/resources?api=abc123&region=us-east-1"},
		{"arn:aws:glue:us-east-1:123456789012:table/mydb/mytable", "", "https://us-east-1.console.aws.amazon.com/glue/home?region=us-east-1#/v2/data-catalog/tables/view/mytable?database=mydb"},
		// Unknown service: service console home in the ARN's region.
		{"arn:aws:frobnicate:ap-south-1:123456789012:thing/x", "", "https://ap-south-1.console.aws.amazon.com/frobnicate/home?region=ap-south-1"},
	}
	for _, tc := range cases {
		a, err := arn.Parse(tc.arn)
		if err != nil {
			t.Fatalf("%s: %v", tc.arn, err)
		}
		if got := ResourceDestination(base, true, a, tc.fallback); got != tc.want {
			t.Errorf("%s\n got %s\nwant %s", tc.arn, got, tc.want)
		}
	}
}

func TestResourceDestinationOtherPartitions(t *testing.T) {
	a, _ := arn.Parse("arn:aws-us-gov:ec2:us-gov-west-1:123456789012:instance/i-1")
	got := ResourceDestination("https://console.amazonaws-us-gov.com", false, a, "")
	if got != "https://console.amazonaws-us-gov.com/ec2/home?region=us-gov-west-1#InstanceDetails:instanceId=i-1" {
		t.Errorf("gov = %s", got)
	}
	a, _ = arn.Parse("arn:aws-cn:sqs:cn-north-1:123456789012:q")
	got = ResourceDestination("https://console.amazonaws.cn", false, a, "")
	if got != "https://console.amazonaws.cn/sqs/v3/home?region=cn-north-1#/queues/https%3A%2F%2Fsqs.cn-north-1.amazonaws.com.cn%2F123456789012%2Fq" {
		t.Errorf("cn = %s", got)
	}
}
