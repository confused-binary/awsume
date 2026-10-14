package console

import "testing"

func TestDestination(t *testing.T) {
	const base = "https://console.aws.amazon.com"
	cases := []struct {
		region, service, want string
	}{
		{"", "", base + "/console/home"},
		{"us-east-1", "", "https://us-east-1.console.aws.amazon.com/console/home?region=us-east-1"},
		{"eu-west-1", "ec2", "https://eu-west-1.console.aws.amazon.com/ec2/home?region=eu-west-1"},
		{"eu-west-1", "S3", "https://eu-west-1.console.aws.amazon.com/s3/home?region=eu-west-1"},
		{"", "iam", base + "/iam/home"},
		{"us-west-2", "unknownsvc", "https://us-west-2.console.aws.amazon.com/unknownsvc/home?region=us-west-2"},
		{"us-west-2", "/custom/path?x=1", "https://us-west-2.console.aws.amazon.com/custom/path?x=1&region=us-west-2"},
		{"us-west-2", "logs", "https://us-west-2.console.aws.amazon.com/cloudwatch/home?region=us-west-2#logsV2:log-groups"},
		{"ap-south-1", "apigateway", "https://ap-south-1.console.aws.amazon.com/apigateway/main/apis?region=ap-south-1"},
	}
	for _, tc := range cases {
		if got := Destination(base, true, tc.region, tc.service); got != tc.want {
			t.Errorf("Destination(%q, %q) = %q, want %q", tc.region, tc.service, got, tc.want)
		}
	}
}

func TestDestinationNonRegionalHost(t *testing.T) {
	got := Destination("https://console.amazonaws-us-gov.com/", false, "us-gov-west-1", "ec2")
	want := "https://console.amazonaws-us-gov.com/ec2/home?region=us-gov-west-1"
	if got != want {
		t.Errorf("gov = %q, want %q", got, want)
	}
	got = Destination("https://eu-west-1.console.aws.amazon.com", true, "eu-west-1", "")
	want = "https://eu-west-1.console.aws.amazon.com/console/home?region=eu-west-1"
	if got != want {
		t.Errorf("already regional host = %q, want %q", got, want)
	}
}

func TestKnownServices(t *testing.T) {
	names := KnownServices()
	if len(names) < 20 {
		t.Fatalf("too few services: %d", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("services not sorted at %d: %v", i, names)
		}
		if names[i] == "" {
			t.Fatal("empty service name listed")
		}
	}
}
