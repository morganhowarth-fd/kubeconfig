# kubeconfig

A CLI tool that discovers EKS clusters across multiple AWS profiles and regions, then generates a unified `~/.kube/config` file with the appropriate `aws eks get-token` exec credentials.

## Prerequisites

- AWS CLI configured with SSO profiles in `~/.aws/config`
- Active AWS SSO session (`aws sso login`)

## Install

### Download a binary (recommended)

Download the latest release for your platform from the [GitHub releases page](https://github.com/morganhowarth-fd/kubeconfig/releases).

```sh
# macOS (Apple Silicon)
curl -L https://github.com/morgan-howarth/kubeconfig/releases/latest/download/kubeconfig_darwin_arm64.tar.gz | tar xz
sudo mv kubeconfig /usr/local/bin/

# macOS (Intel)
curl -L https://github.com/morgan-howarth/kubeconfig/releases/latest/download/kubeconfig_darwin_x86_64.tar.gz | tar xz
sudo mv kubeconfig /usr/local/bin/

# Linux (amd64)
curl -L https://github.com/morgan-howarth/kubeconfig/releases/latest/download/kubeconfig_linux_x86_64.tar.gz | tar xz
sudo mv kubeconfig /usr/local/bin/
```

### Build from source:

#### Prerequisites
- Go 1.24+ (if building from source)

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
| `--dry-run` | Write the generated kubeconfig to a temp file instead of `~/.kube/config` |
| `--account-prefix <prefix>` | Only include AWS profiles whose name starts with the given prefix |
| `--role <role>` | Only include AWS profiles with this `sso_role_name` (exact match) |

### Examples

Discover all clusters across all profiles:

```sh
kubeconfig
```

Only include profiles starting with `prod`:

```sh
kubeconfig --account-prefix prod
```

Only include profiles with a specific SSO role:

```sh
kubeconfig --role AdministratorAccess
```

Combine filters:

```sh
kubeconfig --account-prefix prod --role ReadOnly
```

Preview the output without modifying your kubeconfig:

```sh
kubeconfig --dry-run
```

## How it works

1. Reads all `[profile ...]` entries from `~/.aws/config`
2. Concurrently queries the EKS API in each region (`us-east-1`, `us-east-2`, `us-west-2`, `ca-west-1`) for every profile
3. Merges discovered clusters into `~/.kube/config`, configuring each context to authenticate via `aws eks get-token` with the correct profile and region
