// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package main

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-lambda-go/lambda"
	"google.golang.org/protobuf/encoding/protojson"

	"istio.io/istio/pkg/test/echo/proto"
	"istio.io/istio/pkg/test/echo/server/forwarder"
)

type (
	Request  = json.RawMessage
	Response = json.RawMessage
)

func handler(ctx context.Context, event Request) (Response, error) {
	f := forwarder.New()
	defer func() {
		_ = f.Close()
	}()
	request := &proto.ForwardEchoRequest{}
	if err := protojson.Unmarshal(event, request); err != nil {
		return nil, err
	}
	res, err := f.ForwardEcho(ctx, &forwarder.Config{
		Request: request,
	})
	if err != nil {
		return nil, err
	}
	return protojson.Marshal(res)
}

func main() {
	lambda.StartHandlerFunc(handler)
}
