package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/devcorb/devcorb/schema"
)

// Result is a loaded file: the model (nil if the file could not be decoded) and
// everything validation found.
type Result struct {
	Workspace   *Workspace
	Diagnostics Diagnostics
	Dir         string // directory of the file; relative paths resolve against it
}

// Load reads, validates and decodes a workspace file. The error is set only when the
// file cannot be read or is not YAML; problems of content are in Diagnostics.
func Load(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	res, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	res.Dir = filepath.Dir(path)
	if res.Workspace != nil {
		res.Diagnostics = append(res.Diagnostics, checkPaths(res.Workspace, res.Dir, res.doc)...).Sort()
	}
	return res.Result, nil
}

type parsed struct {
	*Result
	doc *yaml.Node
}

// Parse validates and decodes workspace YAML. Checks that need the file system
// (paths of modules) run only in Load.
func Parse(data []byte) (*Result, error) {
	res, err := parse(data)
	if err != nil {
		return nil, err
	}
	return res.Result, nil
}

func parse(data []byte) (*parsed, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, errors.New("empty file")
	}
	res := &parsed{Result: &Result{}, doc: &doc}

	inst, err := toJSON(&doc)
	if err != nil {
		return nil, err
	}
	schemaDiags := validateSchema(inst, &doc)
	res.Diagnostics = append(res.Diagnostics, schemaDiags...)

	var w Workspace
	if err := doc.Decode(&w); err != nil {
		// The schema already explains type mismatches with paths; keep its messages.
		if !schemaDiags.HasErrors() {
			res.Diagnostics = append(res.Diagnostics, Diagnostic{Severity: Error, Message: err.Error()})
		}
		res.Diagnostics = res.Diagnostics.Sort()
		return res, nil
	}
	res.Workspace = &w
	res.Diagnostics = append(res.Diagnostics, Validate(&w, &doc)...).Sort()
	return res, nil
}

// toJSON converts the YAML document into the JSON data model the schema validator takes.
func toJSON(doc *yaml.Node) (any, error) {
	var v any
	if err := doc.Decode(&v); err != nil {
		return nil, err
	}
	b, err := json.Marshal(stringKeys(v))
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}

func stringKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = stringKeys(x)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = stringKeys(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = stringKeys(x)
		}
		return t
	}
	return v
}

var compiled = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.WorkspaceV1))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schema.WorkspaceV1ID, doc); err != nil {
		return nil, err
	}
	return c.Compile(schema.WorkspaceV1ID)
})

var printer = message.NewPrinter(language.English)

func validateSchema(inst any, doc *yaml.Node) Diagnostics {
	sch, err := compiled()
	if err != nil {
		panic("embedded workspace schema does not compile: " + err.Error())
	}
	err = sch.Validate(inst)
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out Diagnostics
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		msg := e.ErrorKind.LocalizedString(printer)
		if hint := hintFor(e); hint != "" {
			msg += " (" + hint + ")"
		}
		out = append(out, Diagnostic{
			Severity: Error,
			Path:     joinPath(e.InstanceLocation),
			Line:     lineOf(doc, e.InstanceLocation),
			Message:  msg,
		})
	}
	walk(ve)
	return out
}

// hintFor adds the human meaning of patterns the schema enforces.
func hintFor(e *jsonschema.ValidationError) string {
	kw := e.ErrorKind.KeywordPath()
	loc := e.InstanceLocation
	if len(kw) > 0 && kw[0] == "minProperties" && len(loc) == 1 && loc[0] == "modules" {
		return "a project needs at least one module: code to develop and fork"
	}
	if len(kw) == 0 || kw[0] != "pattern" {
		return ""
	}
	switch {
	case len(loc) == 1 && loc[0] == "project":
		return "lowercase letters, digits and dashes, 2–31 characters, starting with a letter"
	case len(loc) == 1 && loc[0] == "domain":
		return "a host ending with .localhost, e.g. shop.localhost"
	case len(loc) == 2 && (loc[0] == "modules" || loc[0] == "services"):
		return "a DNS label: lowercase letters, digits and dashes, starting with a letter"
	case len(loc) >= 3 && loc[len(loc)-1] == "runtime":
		return "<lang>:<version>, e.g. node:22"
	case strings.HasSuffix(joinPath(loc), "sleep_after"):
		return "a duration such as 30m or 2h"
	}
	return ""
}
