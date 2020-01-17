package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"

	"mime/multipart"
	"net/textproto"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ssm"
)

const (
	boothook = `#!/bin/sh
umask 007
FILE=/etc/ssm-cloud-config.yaml
if [ ! -f "${FILE}" ]; then
	%s | jq .Parameter.Value -r > /etc/ssm-cloud-config.yaml
	%s
	systemctl restart cloud-init
fi
`

	include = `file:///etc/ssm-cloud-config.yaml
`

	cloudConfig = `
#cloud-config
write_files:
-   content: |
        It works!
    path: /etc/ssm-test
`
	multipartHeader = `MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="%s"

`
)

var (
	BootHookType = textproto.MIMEHeader{
		"content-type": {"text/cloud-boothook"},
	}
	IncludeType = textproto.MIMEHeader{
		"content-type": {"text/x-include-url"},
	}
)

func requestToCurl(req *request.Request) (string, error) {
	err := req.Build()
	if err != nil {
		return "", err
	}
	s := "curl"
	err = req.Sign()
	if err != nil {
		return "", err
	}

	url := req.HTTPRequest.URL

	// Create headers
	for h, v := range req.HTTPRequest.Header {
		s = s + " -H \"" + h + ": " + strings.Join(v, ",") + "\""
	}

	body, err := ioutil.ReadAll(req.GetBody())

	if err != nil {
		return "", err
	}
	// Add the body and finally the URL
	return s + " -d '" + string(body) + "' \"" + url.String() + "\"", nil
}

func userdata(svc *ssm.SSM, parameter string) (string, error) {
	buf := bytes.NewBuffer([]byte{})
	mpWriter := multipart.NewWriter(buf)
	bootHookWriter, _ := mpWriter.CreatePart(BootHookType)

	getRequest, _ := svc.GetParameterRequest(
		&ssm.GetParameterInput{
			Name:           aws.String(parameter),
			WithDecryption: aws.Bool(true),
		},
	)
	deleteRequest, _ := svc.DeleteParameterRequest(
		&ssm.DeleteParameterInput{
			Name: aws.String(parameter),
		},
	)
	getCommand, err := requestToCurl(getRequest)
	if err != nil {
		return "", err
	}
	deleteCommand, err := requestToCurl(deleteRequest)
	if err != nil {
		return "", err
	}

	_, err = bootHookWriter.Write([]byte(fmt.Sprintf(boothook, getCommand, deleteCommand)))
	if err != nil {
		return "", err
	}

	includeWriter, _ := mpWriter.CreatePart(IncludeType)
	_, err = includeWriter.Write([]byte(include))
	if err != nil {
		return "", err
	}
	err = mpWriter.Close()
	if err != nil {
		return "", err
	}
	bytes, err := ioutil.ReadAll(buf)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(multipartHeader, mpWriter.Boundary()) + string(bytes), nil
}

func main() {
	sess, _ := session.NewSession(&aws.Config{
		Region: aws.String("eu-west-1")},
	)

	ec2Svc := ec2.New(sess)
	ssmSvc := ssm.New(sess)
	_, err := ssmSvc.PutParameter(&ssm.PutParameterInput{
		Name:      aws.String("presign-test"),
		Value:     aws.String(cloudConfig),
		Type:      aws.String(ssm.ParameterTypeSecureString),
		Overwrite: aws.Bool(true),
	})

	if err != nil {
		panic(err)
	}

	data, err := userdata(ssmSvc, "presign-test")
	if err != nil {
		panic(err)
	}

	fmt.Println("Userdata looks like:")
	fmt.Println(data)

	encodedData := base64.StdEncoding.EncodeToString([]byte(data))

	// Specify the details of the instance that you want to create.
	runResult, err := ec2Svc.RunInstances(&ec2.RunInstancesInput{
		// A CAPI AMI in eu-west-1
		ImageId:      aws.String("ami-0295e27735f1e45f4"),
		InstanceType: aws.String("t3.medium"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		UserData:     aws.String(encodedData),
		KeyName:      aws.String("default"),
	})

	if err != nil {
		fmt.Println("Could not create instance", err)
		return
	}

	fmt.Println("Created instance", *runResult.Instances[0].InstanceId)

	// Add tags to the created instance
	_, errtag := ec2Svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{runResult.Instances[0].InstanceId},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String("ssm-parameter-test"),
			},
		},
	})
	if errtag != nil {
		fmt.Println("Could not create tags for instance", runResult.Instances[0].InstanceId, errtag)
		return
	}

	fmt.Println("Successfully tagged instance")
	fmt.Println("instance id is: " + aws.StringValue(runResult.Instances[0].InstanceId))
}
