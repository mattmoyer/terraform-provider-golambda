package provider

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Acceptance tests run with OpenTofu:
//
//	TF_ACC=1 TF_ACC_TERRAFORM_PATH=$(which tofu) TF_ACC_PROVIDER_HOST=registry.opentofu.org go test ./internal/provider/
//
// They build golang.org/x/example/hello, which is public.

var testProviders = map[string]func() (tfprotov6.ProviderServer, error){
	"golambda": providerserver.NewProtocol6WithError(New("test")()),
}

// Two golang/example commits that don't touch hello/.
const (
	helloA = "golang.org/x/example/hello@7f05d217867b"
	helloB = "golang.org/x/example/hello@53021d5c81c3"
)

func config(cacheDir, target, extra string) string {
	return fmt.Sprintf(`
provider "golambda" {
  cache_dir = %q
}

resource "golambda_zip_archive" "test" {
  target = %q
  %s
}

resource "terraform_data" "dep" {
  input = golambda_zip_archive.test.output_base64sha256
}
`, cacheDir, target, extra)
}

const addr = "golambda_zip_archive.test"

func TestAccZipArchive(t *testing.T) {
	cache := t.TempDir()
	sameHash := statecheck.CompareValue(compare.ValuesSame())
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviders,
		Steps: []resource.TestStep{
			{
				// Build at plan time: the hash is known before apply.
				Config: config(cache, helloA, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(addr, tfjsonpath.New("output_sha256"), knownvalue.StringRegexp(regexp.MustCompile(`^[0-9a-f]{64}$`))),
						plancheck.ExpectKnownValue("terraform_data.dep", tfjsonpath.New("input"), knownvalue.NotNull()),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					sameHash.AddStateValue(addr, tfjsonpath.New("output_sha256")),
					statecheck.ExpectKnownValue(addr, tfjsonpath.New("resolved_version"), knownvalue.StringExact("v0.0.0-20250915201037-7f05d217867b")),
					statecheck.ExpectKnownValue(addr, tfjsonpath.New("goarch"), knownvalue.StringExact("arm64")),
				},
				Check: resource.ComposeTestCheckFunc(
					checkZipEntries(addr, "bootstrap"),
				),
			},
			{
				// A new commit with identical code: only resolved_version
				// changes, and dependents see no diff.
				Config: config(cache, helloB, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction("terraform_data.dep", plancheck.ResourceActionNoop),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					sameHash.AddStateValue(addr, tfjsonpath.New("output_sha256")),
					statecheck.ExpectKnownValue(addr, tfjsonpath.New("resolved_version"), knownvalue.StringExact("v0.0.0-20250915192409-53021d5c81c3")),
				},
			},
			{
				// A deleted archive is rebuilt identically during refresh.
				PreConfig: func() { removeOutputs(t, cache) },
				Config:    config(cache, helloB, ""),
				PlanOnly:  true,
			},
			{
				// args: bootstrap becomes a wrapper script.
				Config: config(cache, helloB, `args = ["-r", "it's"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("terraform_data.dep", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeTestCheckFunc(
					checkZipEntries(addr, "hello", "bootstrap"),
					checkZipFile(addr, "bootstrap", "#!/bin/sh\nexec \"$LAMBDA_TASK_ROOT/hello\" '-r' 'it'\\''s'\n"),
				),
			},
			{
				// Cross-compile for amd64.
				Config: config(cache, helloB, `goarch = "amd64"`),
				Check:  checkZipEntries(addr, "bootstrap"),
			},
		},
	})
}

func TestAccZipArchivePlanFailures(t *testing.T) {
	cache := t.TempDir()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviders,
		Steps: []resource.TestStep{
			{
				Config:      config(cache, "golang.org/x/example/hello@main", ""),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid target`),
			},
			{
				Config:      config(cache, "golang.org/x/example/hello@0000000000aa", ""),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Go build failed`),
			},
			{
				Config:      config(cache, "golang.org/x/example/hello/reverse@7f05d217867b", ""),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`not\s+a\s+main\s+package`),
			},
			{
				Config:      config(cache, helloA, `go_version = "1.99.0"`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`not found`),
			},
		},
	})
}

func removeOutputs(t *testing.T, cache string) {
	t.Helper()
	if err := os.RemoveAll(cache + "/out"); err != nil {
		t.Fatal(err)
	}
}

func readZip(s *terraform.State, name string) (*zip.ReadCloser, error) {
	rs, ok := s.RootModule().Resources[name]
	if !ok {
		return nil, fmt.Errorf("%s not in state", name)
	}
	return zip.OpenReader(rs.Primary.Attributes["output_path"])
}

func checkZipEntries(name string, want ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		zr, err := readZip(s, name)
		if err != nil {
			return err
		}
		defer zr.Close()
		var got []string
		for _, f := range zr.File {
			got = append(got, f.Name)
			if f.Mode().Perm() != 0o755 {
				return fmt.Errorf("%s has mode %v", f.Name, f.Mode())
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("zip entries = %v, want %v", got, want)
		}
		return nil
	}
}

func checkZipFile(name, entry, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		zr, err := readZip(s, name)
		if err != nil {
			return err
		}
		defer zr.Close()
		rc, err := zr.Open(entry)
		if err != nil {
			return err
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if string(got) != want {
			return fmt.Errorf("%s = %q, want %q", entry, got, want)
		}
		return nil
	}
}

func localConfig(cacheDir, sourceDir string) string {
	return fmt.Sprintf(`
provider "golambda" {
  cache_dir = %q
}

resource "golambda_zip_archive" "test" {
  source_dir = %q
}

resource "terraform_data" "dep" {
  input = golambda_zip_archive.test.output_base64sha256
}
`, cacheDir, sourceDir)
}

// writeLocalModule writes a module with a main package in cmd/handler.
func writeLocalModule(t *testing.T, dir, mainGo string) {
	t.Helper()
	pkg := dir + "/cmd/handler"
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/go.mod", []byte("module example.com/local\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pkg+"/main.go", []byte(mainGo), 0o644); err != nil {
		t.Fatal(err)
	}
}

const localMainA = "package main\n\nfunc main() { println(\"a\") }\n"

func TestAccZipArchiveLocal(t *testing.T) {
	cache, src := t.TempDir(), t.TempDir()
	writeLocalModule(t, src, localMainA)
	dir := src + "/cmd/handler"
	sameHash := statecheck.CompareValue(compare.ValuesSame())
	diffHash := statecheck.CompareValue(compare.ValuesDiffer())
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviders,
		Steps: []resource.TestStep{
			{
				Config: localConfig(cache, dir),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(addr, tfjsonpath.New("output_sha256"), knownvalue.StringRegexp(regexp.MustCompile(`^[0-9a-f]{64}$`))),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					sameHash.AddStateValue(addr, tfjsonpath.New("output_sha256")),
					diffHash.AddStateValue(addr, tfjsonpath.New("output_sha256")),
					statecheck.ExpectKnownValue(addr, tfjsonpath.New("resolved_version"), knownvalue.Null()),
				},
				Check: checkZipEntries(addr, "bootstrap"),
			},
			{
				// A change that doesn't affect the binary (a trailing
				// comment) is rebuilt at plan and shows no diff.
				PreConfig: func() { writeLocalModule(t, src, localMainA+"\n// unrelated\n") },
				Config:    localConfig(cache, dir),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					sameHash.AddStateValue(addr, tfjsonpath.New("output_sha256")),
				},
			},
			{
				// A code change is picked up with no other input changing,
				// and the new hash is known at plan time.
				PreConfig: func() { writeLocalModule(t, src, "package main\n\nfunc main() { println(\"b\") }\n") },
				Config:    localConfig(cache, dir),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue("terraform_data.dep", tfjsonpath.New("input"), knownvalue.NotNull()),
						plancheck.ExpectResourceAction("terraform_data.dep", plancheck.ResourceActionUpdate),
					},
				},
				ConfigStateChecks: []statecheck.StateCheck{
					diffHash.AddStateValue(addr, tfjsonpath.New("output_sha256")),
				},
			},
			{
				// A deleted archive is restored without a diff.
				PreConfig: func() { removeOutputs(t, cache) },
				Config:    localConfig(cache, dir),
				PlanOnly:  true,
			},
			{
				// A deleted archive plus a code change: refresh leaves state
				// alone and the plan shows the update.
				PreConfig: func() {
					removeOutputs(t, cache)
					writeLocalModule(t, src, "package main\n\nfunc main() { println(\"c\") }\n")
				},
				Config: localConfig(cache, dir),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
					},
				},
				Check: checkZipEntries(addr, "bootstrap"),
			},
		},
	})
}

func TestAccZipArchiveLocalPlanFailures(t *testing.T) {
	cache, src := t.TempDir(), t.TempDir()
	writeLocalModule(t, src, "package main\n\nfunc main() { undefined() }\n")
	if err := os.WriteFile(src+"/lib.go", []byte("package local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviders,
		Steps: []resource.TestStep{
			{
				Config:      localConfig(cache, src+"/cmd/handler"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`undefined`),
			},
			{
				Config:      localConfig(cache, src),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`not\s+a\s+main\s+package`),
			},
			{
				Config:      localConfig(cache, src+"/missing"),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`no\s+such\s+file`),
			},
			{
				Config: fmt.Sprintf(`
provider "golambda" {
  cache_dir = %q
}

resource "golambda_zip_archive" "test" {
  target     = %q
  source_dir = %q
}
`, cache, helloA, src),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Combination`),
			},
		},
	})
}

// Deleting a resource and its source_dir together must plan a clean destroy,
// even on a worker without the archive on disk.
func TestAccZipArchiveLocalOrphanDestroy(t *testing.T) {
	cache, src := t.TempDir(), t.TempDir()
	writeLocalModule(t, src, localMainA)
	dir := src + "/cmd/handler"
	removeSource := func() {
		removeOutputs(t, cache)
		if err := os.RemoveAll(src + "/cmd"); err != nil {
			t.Fatal(err)
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testProviders,
		Steps: []resource.TestStep{
			{
				Config: localConfig(cache, dir),
			},
			{
				// Still in configuration: refresh only warns, and the plan
				// reports the missing directory without the GITHUB_TOKEN hint.
				PreConfig:   removeSource,
				Config:      localConfig(cache, dir),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid source_dir.*no\s+such\s+file`),
			},
			{
				PreConfig: removeSource,
				Config:    fmt.Sprintf("provider \"golambda\" {\n  cache_dir = %q\n}\n", cache),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionDestroy),
					},
				},
			},
		},
	})
}
