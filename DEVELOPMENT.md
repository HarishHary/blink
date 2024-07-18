- [Development](#development)
  - [Overview](#overview)
  - [Prerequisites / Dependencies](#prerequisites-dependencies)
    - [Developing locally](#developing-locally)
    - [Developing using Docker](#developing-using-docker)
  - [Components](#components)
  - [Relevant commands](#relevant-commands)
  - [IDEs](#ides)
    - [Dev Containers](#dev-containers)
  - [Formatting](#formatting)
  - [Anything else](#anything-else)

# Development

This guide contains relevant and useful information if you are developing on the Blink project.

## Overview

Blink is a monorepo codebase with code in several languages (Ansible, Go, Bash Script, Makefile, Terraform).

## Prerequisites / Dependencies

Since Blink is still pretty alpha phase, you need to ensure several dependencies are installed if you are developing on the source code:

- [Go](https://go.dev/doc/install) >= 1.22
- [Terraform CDK](https://developer.hashicorp.com/terraform/tutorials/cdktf/cdktf-install) >= 0.20.4
- [Node](https://nodejs.org/en/download/package-manager) >= v20.14.0
- [Docker](#developing-using-docker)

This is the complete set of requirements. Of course, if you don't want to install all those dependencies, you can use a [Dev Containers](#dev-containers) to have a developement environement with Visual Studio code.

### Developing locally

<!-- FIXME -->

### Developing using Docker

<!-- FIXME -->

## Components

The major components of the codebase are:

<!-- FIXME -->

## Relevant commands

<!-- FIXME -->

## IDEs

Of course, you can use any IDE you wish but using Visual Studio Code is used by the devlopement team and a dev container is given to ease up the process:

With the terminal, the text editor is a developer's most important tool. Everyone has their preferences, but if you're just getting started and looking for something simple that works, Visual Studio Code is a pretty good option.

Go ahead, download it and install it.

I also recommend install the following visual studio plugins: Gitlens, Terraform, Azure, Ansible, Python

### Dev Containers

Dev Containers have become our preferred deployment and developement method. In short, with Docker installed, open `Code` and install the [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)

See the following for more detail on Dev Containers, including Docker troubleshooting:
[Developing inside a Container using Visual Studio Code Remote Development](https://code.visualstudio.com/docs/devcontainers/containers#_installation)

## Formatting

Git hook scripts are useful for identifying simple issues before submission to code review. We run our hooks on every commit to automatically point out issues in code such as missing semicolons, trailing whitespace, and debug statements. By pointing these issues out before code review, this allows a code reviewer to focus on the architecture of a change while not wasting time with trivial style nitpicks.

Run pre-commit install to set up the git hook scripts:

``` bash
# pre-commit install
pre-commit installed at .git/hooks/pre-commit
```

If you are contributing, make sure to run `trunk fmt` before making a PR.

## Anything else

If you discover anything confusing or missing while developing, feel free to:

- Create an issue or PR to improve these docs so others can benefit too.
- You can directly ping me as well.
