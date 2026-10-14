package arn

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	a, err := Parse(" arn:aws:ec2:us-east-1:123456789012:instance/i-0abc ")
	if err != nil {
		t.Fatal(err)
	}
	if a.Partition != "aws" || a.Service != "ec2" || a.Region != "us-east-1" || a.Account != "123456789012" || a.Resource != "instance/i-0abc" {
		t.Errorf("parsed = %+v", a)
	}
	typ, id := a.Split()
	if typ != "instance" || id != "i-0abc" {
		t.Errorf("split = %q %q", typ, id)
	}

	a, _ = Parse("arn:aws:lambda:eu-west-1:123456789012:function:fn:3")
	if typ, id := a.Split(); typ != "function" || id != "fn:3" {
		t.Errorf("lambda split = %q %q", typ, id)
	}
	a, _ = Parse("arn:aws:s3:::my-bucket/some/key")
	if typ, id := a.Split(); typ != "my-bucket" || id != "some/key" || a.Region != "" {
		t.Errorf("s3 split = %q %q", typ, id)
	}
	a, _ = Parse("arn:aws:sqs:us-east-1:123456789012:queue")
	if typ, id := a.Split(); typ != "" || id != "queue" {
		t.Errorf("sqs split = %q %q", typ, id)
	}
	a, _ = Parse("arn:aws:rds:us-east-1:123456789012:db:name/with/slash")
	if typ, id := a.Split(); typ != "db" || id != "name/with/slash" {
		t.Errorf("rds split = %q %q", typ, id)
	}
	a, _ = Parse("arn:aws:iam::123456789012:role/path/Name")
	if typ, id := a.Split(); typ != "role" || id != "path/Name" {
		t.Errorf("iam split = %q %q", typ, id)
	}

	for _, bad := range []string{"", "i-0abc", "arn:aws:ec2", "arn::ec2:us-east-1:1:x", "arn:aws:ec2:us-east-1:1:", "nrn:aws:ec2:us-east-1:1:x"} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) err = %v", bad, err)
		}
	}
}
