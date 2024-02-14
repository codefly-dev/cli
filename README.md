[![Go CLI](https://github.com/codefly-dev/cli/actions/workflows/go.yml/badge.svg)](https://github.com/codefly-dev/cli/actions/workflows/go.yml)
[![Release](https://github.com/codefly-dev/cli/actions/workflows/release.yml/badge.svg)](https://github.com/codefly-dev/cli/actions/workflows/release.yml)

# Welcome to the `codefly.ai` CLI 😇

> We are building the next generation of tools for developers.

![](docs/media/dragonfly.png)





# Development

## Requirements

### Automatic versioning of CLI and agents

`scripts/publish.sh` uses `semver` to automatically version the CLI.

- https://github.com/usvc/semver

### Debugging of Agents

It can be very handy to be able to "drop in" into the agent code and debug it.

To do so:

- run the CLI with a breakpoint
- get the PID of the agent from the log
- set a breakpoint in the agent code
- attach the process in the IDE with your agent code
- let the magic happens!

> Note: this process will be the same to allow debugging into the client code: CLI exposes an endpoint with the PID of the client process.

This is by far the best experience to advance debugging! I would say 99% of developers don't know about this is even possible.


### Generating client code
```shell
gm generate gRPC -d --service management/build --destination pkg/builder/clients/builder --language go
```


# OLD


TODO: golang ci lint

## Local Development

### Setup
We use `go.work` for local development so the structure of the projects should be this for now:

```shell
codefly.dev
├── cli
├── core
├── golor
├── agents
│   ├── libraries
│   └── services
└── sdk
    └── sdk-go
```

You can use `go.work` to use the local `core` package if you make changes to it. This is the most common developer setup -- it's not setup in the repo to allow CI to work.

Note: I don't include `go.work` anymore in the repo so you can order your directories any way you like as long as you use a proper `go.work` file.

For the previous layout, `go.work` looks like this:

```go
go 1.21.5

use (
.
../core
)
```


### Building the CLI

This also does the `zsh` completion. Bash or other shells are supported as well but not in this script.

```shell
./scripts/dev/install.sh && source ~/.zshrc
```

### Init

```shell
codefly init
```

### Building agents

Inside each agent you want to build, run:

```shell
./build.sh
```

To use private repositories, TODO docs based on
https://medium.com/@joeponzio/how-to-use-a-private-github-repo-as-a-go-module-442fbedc80c9


# TODO: Will be in the main docs
# HERE WE WANT README FOR CLI DEVELOPERS

## Tracks


## Getting started

Sometime, you want to run the CLI in the proper directory, for example, it will "pin" the project and application to wherever you are.

If you run outside, for example, running `go run main.go` in the `cli` directory, you can rely on the *current* application and project as defined in the `~/.codefly/codefly.yaml` file.

In other words, everywhere, in the CLI directory when developing, replace `codefly` with `go run main.go` in the commands.

To change context, you can run:

```shell
codefly context switch
```

This is the interactive version, code for the argument version would be useful too.


### Creating an application

```shell
codefly add application your-app
```

### Creating a service

```shell
 codefly add service my-service --agent=python
```

### Running things
I recommend running with debugging
```shell
codefly run application -d
```

Generate templates code
```shell
codefly agent generate -d --todo --service=../agents/services/go-grpc
```



TODO: Move this somewhere else

## Go

### Error handling

#### User error vs system error

- [x] User error: the user did something wrong, action can be taken and should be given

```go

TODO
```

- [x] System error: something went wrong, it is a bug

```go
importer, err := imports.NewApplicationImporter()
shared.UnexpectedExitOnError(err, "cannot create applications importer")
```

#### To wrap or not to wrap

- [x] Don't wrap when the context is clear

```go
func ImportApplication(imp ApplicationImporter) error {
    err := imp.Fetch()
    if err != nil {
        return err
    }
	//...
}
```

Here we are in the same domain: it should be clear from the import error what is going on

- [x] Wrap when the context is not clear

```go
TODO
```


### Tips for a Cool README:

1. **Engaging Visuals**: Include badges for build status, code quality, etc., and consider adding a project logo or screenshots/gifs of your project in action.

2. **Clear and Concise**: Make your README easy to read with clear headings and concise descriptions.

3. **Examples**: Include usage examples, as they are extremely helpful to new users.

4. **Contribution Guidelines**: Encourage community involvement with clear contribution guidelines.

5. **License Information**: Always specify the license to inform users about how they can use your project.

6. **Acknowledgments**: Give credit where it's due if you're building upon others' work.

7. **Keep it Updated**: Regularly update the README as your project evolves.

Remember, the README is often the first thing users or potential contributors see, so making it informative, welcoming, and visually appealing can greatly impact the success of your project.
