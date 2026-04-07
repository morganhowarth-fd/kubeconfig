# kubeconfig

A CLI tool that discovers EKS clusters across multiple AWS profiles and regions, then generates a unified `~/.kube/config` file with the appropriate `aws eks get-token` exec credentials.

## Prerequisites

- Go 1.25+
- AWS CLI configured with SSO profiles in `~/.aws/config`
- Active AWS SSO session (`aws sso login`)

## Install

Download one of the pre-compiled binaries in the GitHub releases page and add it to your PATH. Or follow the build steps:

## Build

```sh
go build -o kubeconfig ./main.go
```

Optionally, move the binary to your PATH:

```sh
mv kubeconfig /usr/local/bin/
```

## Usage

```sh
kubeconfig [flags]
```

### Flags

| Flag | Description |
|------|-------------|
| `-dry-run` | Write the generated kubeconfig to a temp file instead of `~/.kube/config` |
| `-account-prefix <prefix>` | Only include AWS profiles whose name starts with the given prefix |

### Examples

Discover all clusters across all profiles:

```sh
kubeconfig
```

Only include profiles starting with `prod`:

```sh
kubeconfig -account-prefix prod
```

Preview the output without modifying your kubeconfig:

```sh
kubeconfig -dry-run
```

## How it works

1. Reads all `[profile ...]` entries from `~/.aws/config`
2. Concurrently queries the EKS API in each region (`us-east-1`, `us-east-2`, `us-west-2`, `ca-west-1`) for every profile
3. Merges discovered clusters into `~/.kube/config`, configuring each context to authenticate via `aws eks get-token` with the correct profile and region
