package spec

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/amalychev/devopus/internal/catalog"
)

// The YAML form of a project: what 'devopus project config show -o yaml' prints and
// what the workspace server keeps in each version of a project's settings.

// YAML encodes the project.
func (p *Project) YAML() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseYAML decodes a project; unknown keys are errors.
func ParseYAML(data []byte) (*Project, error) {
	var p Project
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("project config: %w", err)
	}
	if p.Apps == nil {
		p.Apps = map[string]*App{}
	}
	if p.Services == nil {
		p.Services = map[string]*Service{}
	}
	return &p, nil
}

// Clone is a deep copy.
func (p *Project) Clone() *Project {
	b, err := p.YAML()
	if err != nil {
		panic(err)
	}
	c, err := ParseYAML(b)
	if err != nil {
		panic(err)
	}
	return c
}

// NameRe is what application, service and project names look like: they become
// host names and parts of URLs.
var NameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// unpinned are versions that move under people's feet.
var unpinned = map[string]bool{"": true, "latest": true}

// Check is the validation of the workspace server: what the engine needs (Validate)
// and what keeps a team's environments the same on every machine — names that make
// hosts, a source for every application, a pinned version for every service.
func (p *Project) Check() error {
	var errs []string
	if err := p.Validate(); err != nil {
		errs = append(errs, err.Error())
	}
	if p.Domain == "" {
		errs = append(errs, "domain: not set")
	}
	for _, n := range p.AppNames() {
		a := p.Apps[n]
		if !NameRe.MatchString(n) {
			errs = append(errs, fmt.Sprintf("apps.%s: a name is lowercase letters, digits and dashes, starting with a letter", n))
		}
		if a == nil {
			errs = append(errs, fmt.Sprintf("apps.%s: empty", n))
			continue
		}
		if a.Repo == "" && a.Path == "" {
			errs = append(errs, fmt.Sprintf("apps.%s: where is the code? set a repository (--repo) or a folder (--path)", n))
		}
	}
	for _, n := range p.ServiceNames() {
		s := p.Services[n]
		if !NameRe.MatchString(n) {
			errs = append(errs, fmt.Sprintf("services.%s: a name is lowercase letters, digits and dashes, starting with a letter", n))
		}
		t, ok := catalog.Lookup(s.Type)
		if !ok {
			continue // Validate said it
		}
		if t.Kind != catalog.Process && s.Image == "" && unpinned[s.Version] {
			errs = append(errs, fmt.Sprintf("services.%s: name the %s version, so everyone runs the same one", n, t.Name))
		}
		for _, u := range s.Uses {
			if p.Services[u] == nil {
				errs = append(errs, fmt.Sprintf("services.%s: uses unknown service %q", n, u))
			}
		}
		if s.Type == catalog.Cronjob && s.Schedule == "" {
			errs = append(errs, fmt.Sprintf("services.%s: a cronjob needs a schedule", n))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
