variable "commit_sha" {
  type = string
}

resource "golambda_zip_archive" "handler" {
  target = "github.com/acme/lambdas/cmd/handler@${var.commit_sha}"
  goarch = "arm64"
}

# A binary that doubles as a CLI tool, invoked with arguments.
resource "golambda_zip_archive" "worker" {
  target = "github.com/acme/tools/cmd/acme@${var.commit_sha}"
  args   = ["worker", "--queue=jobs"]
}

# A package in the same repository as this configuration. It's rebuilt on
# every plan and only changes when the archive bytes do.
resource "golambda_zip_archive" "local" {
  source_dir = "${path.module}/../cmd/handler"
}

resource "aws_lambda_function" "handler" {
  function_name    = "handler"
  role             = aws_iam_role.lambda.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = golambda_zip_archive.handler.output_path
  source_code_hash = golambda_zip_archive.handler.output_base64sha256
}
