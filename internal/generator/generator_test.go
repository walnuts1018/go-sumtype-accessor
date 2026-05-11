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

type ExampleState interface {
	isExampleState()
	GetID() SharedID
	SetID(SharedID)
	GetVersion() SharedVersion
	SetVersion(SharedVersion)
}

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
`,
	})

	err := Generate(Config{
		Dir:    dir,
		Suffix: "_sumtype.go",
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	generated := readFile(t, filepath.Join(dir, "examplestate_sumtype.go"))
	for _, want := range []string{
		"func (*FirstVariant) isExampleState() {}",
		"func (v *FirstVariant) GetID() SharedID {",
		"return v.ID",
		"func (v *FirstVariant) SetID(value SharedID) {",
		"v.ID = value",
		"func (v *FirstVariant) GetVersion() SharedVersion {",
		"return v.Version",
		"func (v *FirstVariant) SetVersion(value SharedVersion) {",
		"v.Version = value",
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

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-build"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated package does not compile: %v\n%s", err, out)
	}
}

func TestGenerateRejectsGetterTypeMismatch(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string

type ExampleState interface {
	isExampleState()
	GetID() string
	SetID(SharedID)
}

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	ID SharedID
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "getter method GetID returns string, field type is SharedID") {
		t.Fatalf("Generate() error = %v, want getter type mismatch", err)
	}
}

func TestGenerateRejectsSetterTypeMismatch(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string

type ExampleState interface {
	isExampleState()
	GetID() SharedID
	SetID(string)
}

// +go-sumtype-accessor=ExampleState
type FirstVariant struct {
	ID SharedID
}
`,
	})

	err := Generate(Config{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "setter method SetID accepts string, field type is SharedID") {
		t.Fatalf("Generate() error = %v, want setter type mismatch", err)
	}
}

func TestGenerateIgnoresFieldsWithoutInterfaceAccessors(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string

type ExampleState interface {
	isExampleState()
}

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

	generated := readFile(t, filepath.Join(dir, "examplestate_sumtype.go"))
	if strings.Contains(generated, "GetID") || strings.Contains(generated, "SetID") {
		t.Fatalf("generated accessors without interface methods:\n%s", generated)
	}
}

func TestGenerateRejectsMisspelledAnnotation(t *testing.T) {
	dir := writePackage(t, map[string]string{
		"go.mod": "module example.com/sample\n\ngo 1.24\n",
		"state.go": `package sample

type SharedID string

type ExampleState interface {
	isExampleState()
	GetID() SharedID
	SetID(SharedID)
}

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
