package generator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateAccessorsFromAnnotatedCommonFields(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string
type SharedVersion int64
type PrivateDetail string
type SharedKey string

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	ID      SharedID
	Version SharedVersion
}

// +go-sumtype-accessor=ExampleState
type SecondVariant struct {
	ID           SharedID
	Version      SharedVersion
	RetryCount   int32
	DisplayLabel string
}

// +go-sumtype-accessor=ExampleState
type ThirdVariant struct {
	ID           SharedID
	Version      SharedVersion
	RetryCount   int32
	PrivateValue PrivateDetail
}

type IgnoredVariant struct {
	ID      SharedID
	Version SharedVersion
}

// +go-sumtype-accessor=AnotherState
type AlphaVariant struct {
	Key SharedKey
}

// +go-sumtype-accessor=AnotherState
type BetaVariant struct {
	Key   SharedKey
	Extra string
}
`,
	})

	err := Generate(Config{
		Dir:    dir,
		Output: "accessors.gen.go",
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	generated := readFile(t, filepath.Join(dir, "accessors.gen.go"))
	for _, want := range []string{
		"type AnotherState interface {",
		"isAnotherState()",
		"GetKey() SharedKey",
		"SetKey(SharedKey)",
		"func (*AlphaVariant) isAnotherState() {}",
		"func (v *AlphaVariant) GetKey() SharedKey {",
		"return v.Key",
		"func (v *AlphaVariant) SetKey(value SharedKey) {",
		"v.Key = value",
		"type ExampleState interface {",
		"isExampleState()",
		"GetID() SharedID",
		"SetID(SharedID)",
		"GetVersion() SharedVersion",
		"SetVersion(SharedVersion)",
		"func (*FirstVariant) isExampleState() {}",
		"func (v *FirstVariant) GetID() SharedID {",
		"return v.ID",
		"func (v *FirstVariant) SetID(value SharedID) {",
		"v.ID = value",
		"func (v *FirstVariant) GetVersion() SharedVersion {",
		"return v.Version",
		"func (v *FirstVariant) SetVersion(value SharedVersion) {",
		"v.Version = value",
		"func (*BetaVariant) isAnotherState() {}",
		"func (*SecondVariant) isExampleState() {}",
		"func (*ThirdVariant) isExampleState() {}",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated file missing %q:\n%s", want, generated)
		}
	}
	if strings.Contains(generated, "RetryCount") {
		t.Fatalf("generated file includes non-common field accessor:\n%s", generated)
	}
	if strings.Contains(generated, "IgnoredVariant") {
		t.Fatalf("generated file includes non-annotated type:\n%s", generated)
	}
	if _, err := os.Stat(filepath.Join(dir, "examplestate_sumtype.go")); !os.IsNotExist(err) {
		t.Fatalf("generated per-interface file exists, want only configured output file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "anotherstate_sumtype.go")); !os.IsNotExist(err) {
		t.Fatalf("generated per-interface file exists, want only configured output file: %v", err)
	}

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-build"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated package does not compile: %v\n%s", err, out)
	}
}

func TestGenerateAccessorsWithoutExistingInterface(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	ID SharedID
}

// +go-sumtype-accessor=ExampleState
type SecondVariant struct {
	ID SharedID
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	generated := readFile(t, filepath.Join(dir, "sumtype_accessors.go"))
	for _, want := range []string{
		"type ExampleState interface {",
		"isExampleState()",
		"GetID() SharedID",
		"SetID(SharedID)",
		"func (*FirstVariant) isExampleState() {}",
		"func (v *FirstVariant) GetID() SharedID {",
		"func (v *FirstVariant) SetID(value SharedID) {",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated file missing %q:\n%s", want, generated)
		}
	}
}

func TestGenerateAccessorsForImportedCommonFieldTypes(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

import "time"

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	StartedAt time.Time
}

// +go-sumtype-accessor=ExampleState
type SecondVariant struct {
	StartedAt time.Time
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	generated := readFile(t, filepath.Join(dir, "sumtype_accessors.go"))
	for _, want := range []string{
		`import "time"`,
		"GetStartedAt() time.Time",
		"SetStartedAt(time.Time)",
		"func (v *FirstVariant) GetStartedAt() time.Time {",
		"func (v *FirstVariant) SetStartedAt(value time.Time) {",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated file missing %q:\n%s", want, generated)
		}
	}

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-build"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated package does not compile: %v\n%s", err, out)
	}
}

func TestGenerateAccessorsForFunctionCommonFieldTypes(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

import "time"

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	Callback func(time.Time) error
}

// +go-sumtype-accessor=ExampleState
type SecondVariant struct {
	Callback func(time.Time) error
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	generated := readFile(t, filepath.Join(dir, "sumtype_accessors.go"))
	for _, want := range []string{
		`import "time"`,
		"GetCallback() func(time.Time) error",
		"SetCallback(func(time.Time) error)",
		"func (v *FirstVariant) GetCallback() func(time.Time) error {",
		"func (v *FirstVariant) SetCallback(value func(time.Time) error) {",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated file missing %q:\n%s", want, generated)
		}
	}

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-build"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated package does not compile: %v\n%s", err, out)
	}
}

func TestGenerateAccessorsForAnonymousStructCommonFieldTypes(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

import "time"

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	Window struct {
		StartedAt time.Time ` + "`json:\"startedAt\"`" + `
	}
}

// +go-sumtype-accessor=ExampleState
type SecondVariant struct {
	Window struct {
		StartedAt time.Time ` + "`json:\"startedAt\"`" + `
	}
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	generated := readFile(t, filepath.Join(dir, "sumtype_accessors.go"))
	for _, want := range []string{
		`import "time"`,
		"StartedAt time.Time `json:\"startedAt\"`",
		"func (v *FirstVariant) GetWindow() struct {",
		"func (v *FirstVariant) SetWindow(value struct {",
	} {
		if !strings.Contains(generated, want) {
			t.Fatalf("generated file missing %q:\n%s", want, generated)
		}
	}

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-build"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated package does not compile: %v\n%s", err, out)
	}
}

func TestLoadPackageReturnsAllPackageErrors(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"first.go": `package sample

func BrokenOne(
`,
		"second.go": `package sample

func BrokenTwo(
`,
	})

	_, err := loadPackage(dir)
	if err == nil {
		t.Fatal("loadPackage() error = nil, want package errors")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) < 2 {
		t.Fatalf("loadPackage() error = %T, want joined package errors: %v", err, err)
	}
	for _, want := range []string{"first.go", "second.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("loadPackage() error missing %q: %v", want, err)
		}
	}
}

func TestLoadPackageIncludesImportsAndDependencies(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"dep/dep.go": `package dep

type Value string
`,
		"state.go": `package sample

import "example.com/sample/dep"

type State struct {
	Value dep.Value
}
`,
	})

	pkg, err := loadPackage(dir)
	if err != nil {
		t.Fatalf("loadPackage() error = %v", err)
	}
	imported := pkg.Imports["example.com/sample/dep"]
	if imported == nil {
		t.Fatalf("loadPackage() imports missing dependency: %#v", pkg.Imports)
	}
	if imported.Types == nil || imported.Types.Scope().Lookup("Value") == nil {
		t.Fatalf("loadPackage() dependency types missing Value: %#v", imported.Types)
	}
}

func TestGenerateRejectsMisspelledAnnotation(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string

// +go-sumtype-accesor=ExampleState
type FirstVariant struct {
	ID SharedID
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "no sumtype accessor annotations found") {
		t.Fatalf("Generate() error = %v, want misspelled annotation to be ignored", err)
	}
}

func writePackage(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
