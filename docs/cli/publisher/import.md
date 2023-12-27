# Turning working code into an Agent

```shell
codefly agent generate  --service=../agents/services/go-grpc
```

## Requirements

- working code needs to be in `./base` of the path provided

## Optional

- a `service.generation.codefly.yaml` file can be provided to override the default generation

## What it does

## Example: go-grpc

In the `base`, we specify in the `service.generation` file what we can replace to the standard codefly template:
```yaml
base:
  name: Web
  domain: github.com/codefly-dev/go-grpc/base
```

This means, that `Web` will be replace by the templatization of `{{.Service.Name}}` and `github.com/codefly-dev/go-grpc/base` will be replaced by `{{.Service.Domain}}`.
