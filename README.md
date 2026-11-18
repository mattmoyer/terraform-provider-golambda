# terraform-provider-golambda

An OpenTofu provider that builds Go AWS Lambda functions into reproducible zip
archives, the way `archive_file` works for JavaScript and Python.

```hcl
resource "golambda_zip_archive" "handler" {
  target = "github.com/acme/lambdas/cmd/handler@${var.commit_sha}"
}

resource "aws_lambda_function" "handler" {
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = golambda_zip_archive.handler.output_path
  source_code_hash = golambda_zip_archive.handler.output_base64sha256
  # ...
}
```

See [docs/](docs/) for every attribute.

## How it works

- **Toolchain.** The requested Go version (`go_version`) is downloaded from
  proxy.golang.org as the `golang.org/toolchain` module, verified against
  sum.golang.org, and kept in `cache_dir`. Nothing needs to be installed on the
  worker except `git`, which is used for private modules.
- **Build at plan time.** The build happens in `ModifyPlan`, so a bad commit,
  a compile error or a missing toolchain fails `tofu plan`. A plan that
  succeeds means the build worked, and the archive hash is a known value in
  the plan. Apply rebuilds (a fast re-link with a warm Go cache) and checks
  the hash matches.
- **Reproducible output.** The provider resolves the module that contains the
  package, then builds from that module's directory as the main module with
  `CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags="-s -w -buildid="`. Unlike
  `go install pkg@version`, this doesn't embed the commit's version in the
  binary. A new commit that doesn't change the Lambda's code or dependencies
  produces a byte-identical archive. The plan then shows only a
  `resolved_version` change, and the Lambda shows no diff. Zip entries have
  fixed timestamps and modes and no extra fields.
- **Arguments.** If `args` is set, `bootstrap` is a `/bin/sh` script that runs
  `exec "$LAMBDA_TASK_ROOT/<binary>" <args...>`. This is useful for binaries
  that also work as CLI tools.
- **Missing files.** If `output_path` disappears (for example, on a fresh
  Spacelift worker), refresh rebuilds it. The default `output_path` is named
  after the archive's SHA-256, so it only changes when the contents do.
- **Caching.** The provider relies on Go's own module and build caches in
  `cache_dir` and has no cache of its own.

## Private modules

Set `GITHUB_TOKEN` (or the provider's `github_token`) in the environment where
plans run, not only where applies run. The token is passed to `git` through
environment-only config and is redacted from error messages. By default,
`GOPRIVATE` is set to `github.com/<owner>` for each target. Set the provider's
`goprivate` to override it.

## Limitations

- Builds need network access during plan. A cold cache adds the toolchain
  download, module fetch and compile time to the plan.
- `replace` directives in the target module's `go.mod` that point at local
  paths won't work, because the module is built from the read-only module
  cache.
- If an input depends on a value that is only known after apply, the hash is
  unknown at plan time and the build happens during apply.
- Zip bytes are only guaranteed stable for a given provider release, because
  `compress/flate` output can change between Go versions. The default
  `go_version` can also change between provider releases.
- cgo is not supported.

## Development

```sh
make test      # unit tests
make testacc   # real downloads and builds, acceptance tests with OpenTofu
make docs      # regenerate docs/
```

To use a local build, add this to `~/.tofurc`:

```hcl
provider_installation {
  dev_overrides {
    "spacelift.io/moyer/golambda" = "/path/to/terraform-provider-golambda"
  }
  direct {}
}
```

## Releasing to Spacelift

One-time setup:

1. Create the `golambda` provider in the Spacelift Terraform registry.
2. Register a signing key with `spacectl provider add-gpg-key --generate`.
3. Store the key and a Spacelift API key as repository secrets. The
   [release workflow](.github/workflows/release.yml) lists the names.

After that, push a `v*` tag. The workflow runs goreleaser, then
`spacectl provider create-version --protocols 6.0`, uploads the docs, and
publishes the version.
