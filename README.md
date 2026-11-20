# terraform-provider-golambda

An experimental OpenTofu provider that builds Go AWS Lambda functions into
reproducible zip archives, the way `archive_file` works for JavaScript and Python.

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

Or build a package from the same repository as the configuration:

```hcl
resource "golambda_zip_archive" "handler" {
  source_dir = "${path.module}/../cmd/handler"
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
- **Local sources.** `source_dir` builds a main package from a directory
  inside a local module, with the same flags. There's no version to compare,
  so it's rebuilt on every plan (a cache hit when nothing changed). The
  resource only shows a diff when the archive bytes change, which is better
  than keying on a commit ID: a commit that doesn't affect the binary causes
  no diff, and uncommitted changes aren't missed. The same code builds to the
  same bytes from any checkout path, and from `target` too. The module's
  `go.mod` and `go.sum` must be complete, and a `vendor` directory is used
  if present. `go.work` files are ignored.
- **Arguments.** If `args` is set, `bootstrap` is a `/bin/sh` script that runs
  `exec "$LAMBDA_TASK_ROOT/<binary>" <args...>`. This is useful for binaries
  that also work as CLI tools.
- **Missing files.** If `output_path` disappears (for example, on a fresh
  CI worker), refresh rebuilds it. The default `output_path` is named
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
- `replace` directives in a `target` module's `go.mod` that point at local
  paths won't work, because the module is built from the read-only module
  cache. They work with `source_dir`.
- A `source_dir` build runs on every plan. With a cold cache (a fresh
  worker), that's a full compile, the same as a `target` build after its
  archive file goes missing.
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
    "registry.opentofu.org/mattmoyer/golambda" = "/path/to/terraform-provider-golambda"
  }
  direct {}
}
```

## License

This project’s source code is licensed under the [Mozilla Public License 2.0](LICENSE).
