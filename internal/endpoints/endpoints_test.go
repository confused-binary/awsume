package endpoints

import "testing"

func TestPartitions(t *testing.T) {
	if p := ForRegion("us-gov-east-1"); p.ID != "aws-us-gov" {
		t.Errorf("ForRegion gov = %s", p.ID)
	}
	if p := ForRegion("cn-northwest-1"); p.ID != "aws-cn" {
		t.Errorf("ForRegion cn = %s", p.ID)
	}
	if p := ForRegion(""); p.ID != "aws" {
		t.Errorf("ForRegion empty = %s", p.ID)
	}
	if p, ok := FromARN("arn:aws-cn:sts::123:assumed-role/x/y"); !ok || p.ID != "aws-cn" {
		t.Errorf("FromARN = %v %v", p, ok)
	}
	if _, ok := FromARN("not-an-arn"); ok {
		t.Error("FromARN accepted garbage")
	}
	if _, ok := ByName("aws-iso"); ok {
		t.Error("unknown partition accepted")
	}
	if len(Names()) != 3 || Default().ID != "aws" {
		t.Errorf("Names = %v", Names())
	}
}

func TestSTSPrecedence(t *testing.T) {
	env := map[string]string{}
	r := Resolver{Partition: Default(), Env: func(k string) string { return env[k] }}

	if ep, region := r.STS(); ep != "https://sts.amazonaws.com" || region != "us-east-1" {
		t.Errorf("global default = %s %s", ep, region)
	}
	r.Region = "eu-west-1"
	if ep, region := r.STS(); ep != "https://sts.eu-west-1.amazonaws.com" || region != "eu-west-1" {
		t.Errorf("regional default = %s %s", ep, region)
	}
	r.STSRegionalEndpoints = "legacy"
	if ep, region := r.STS(); ep != "https://sts.amazonaws.com" || region != "us-east-1" {
		t.Errorf("legacy = %s %s", ep, region)
	}
	r.STSRegionalEndpoints = ""
	r.ProfileEndpoint = "https://profile.test/"
	if ep, _ := r.STS(); ep != "https://profile.test" {
		t.Errorf("profile endpoint = %s", ep)
	}
	env["AWS_ENDPOINT_URL"] = "https://globalenv.test"
	if ep, _ := r.STS(); ep != "https://globalenv.test" {
		t.Errorf("global env = %s", ep)
	}
	r.ProfileSTSEndpoint = "https://services.test"
	if ep, _ := r.STS(); ep != "https://services.test" {
		t.Errorf("services section = %s", ep)
	}
	env["AWS_ENDPOINT_URL_STS"] = "https://stsenv.test"
	if ep, _ := r.STS(); ep != "https://stsenv.test" {
		t.Errorf("sts env = %s", ep)
	}
	r.Overrides.STS = "https://flag.test"
	if ep, region := r.STS(); ep != "https://flag.test" || region != "eu-west-1" {
		t.Errorf("flag override = %s %s", ep, region)
	}
}

func TestOtherPartitionsAndOverrides(t *testing.T) {
	gov, _ := ByName("aws-us-gov")
	r := Resolver{Partition: gov}
	if ep, region := r.STS(); ep != "https://sts.us-gov-west-1.amazonaws.com" || region != "us-gov-west-1" {
		t.Errorf("gov sts = %s %s", ep, region)
	}
	if r.Signin() != "https://signin.amazonaws-us-gov.com/federation" {
		t.Errorf("gov signin = %s", r.Signin())
	}
	if r.Console() != "https://console.amazonaws-us-gov.com" {
		t.Errorf("gov console = %s", r.Console())
	}
	cn, _ := ByName("aws-cn")
	r = Resolver{Partition: cn, Region: "cn-north-1"}
	if ep, _ := r.STS(); ep != "https://sts.cn-north-1.amazonaws.com.cn" {
		t.Errorf("cn sts = %s", ep)
	}
	if got := r.SSOPortal("cn-north-1"); got != "https://portal.sso.cn-north-1.amazonaws.com.cn" {
		t.Errorf("cn sso portal = %s", got)
	}
	r = Resolver{Partition: Default(), Overrides: Overrides{
		Signin: "https://signin.test/federation/", Console: "https://console.test/", SSOPortal: "https://portal.test", SSOOIDC: "https://oidc.test",
	}}
	if r.Signin() != "https://signin.test/federation" || r.Console() != "https://console.test" {
		t.Errorf("overrides = %s %s", r.Signin(), r.Console())
	}
	if r.SSOPortal("us-east-1") != "https://portal.test" || r.SSOOIDC("us-east-1") != "https://oidc.test" {
		t.Errorf("sso overrides = %s %s", r.SSOPortal("us-east-1"), r.SSOOIDC("us-east-1"))
	}
	r = Resolver{Partition: Default()}
	if r.SSOPortal("") != "https://portal.sso.us-east-1.amazonaws.com" || r.SSOOIDC("us-west-2") != "https://oidc.us-west-2.amazonaws.com" {
		t.Errorf("sso defaults = %s %s", r.SSOPortal(""), r.SSOOIDC("us-west-2"))
	}
}
