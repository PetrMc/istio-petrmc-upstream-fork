# Lambda support

Like other platforms, Lambda mostly just involves running Ztunnel with `BOOTSTRAP_TOKEN` set.
However, Lambda doesn't support sidecar containers, so you will need to package Ztunnel with the app.

The provided Dockerfile shows how to do this.

Lambda support is outbound-only.

## Deploying

Build the image, then create a Lambda function with the image.
Under configuration, you'll need to set the `BOOTSTRAP_TOKEN` env var

## Testing

The provided app is a basically a forwarder:

```shell
$ aws lambda invoke --function-name <function name> /dev/stdout --payload '{"url":"http://echo.default.svc.cluster.local"}' --cli-binary-format raw-in-base64-out
```

Local testing can also be done:

```shell
$ docker run --init -it -v ~/.aws-lambda-rie:/aws-lambda -p 9000:8080 --entrypoint /aws-lambda/aws-lambda-rie \
  <IMAGE_NAME> \
  env BOOTSTRAP_TOKEN="..." /usr/local/bin/ztunnel lambda /usr/bin/echo
$ curl "http://localhost:9000/2015-03-31/functions/function/invocations" -d '{"url":"http://echo.default"}' -v
```
