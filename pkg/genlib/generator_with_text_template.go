// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package genlib

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

var generateOnFieldNotInFieldsYaml = errors.New("generate called on a field not present in fields yaml definition")

// GeneratorWithTextTemplate
type GeneratorWithTextTemplate struct {
	tpl       *template.Template
	state     *GenState
	errChan   chan error
	totEvents uint64
}

// awsAZs list all possible AZs for a specific AWS region
// NOTE: this list is not comprehensive
// missing regions: af-south-1, ap-south-2, ap-southeast-3, ap-southeast-4, eu-central-2, eu-south-1, eu-south-2, me-central-1
var awsAZs map[string][]string = map[string][]string{
	"ap-east-1":      {"ap-east-1a", "ap-east-1b", "ap-east-1c"},
	"ap-northeast-1": {"ap-northeast-1a", "ap-northeast-1c", "ap-northeast-1d"},
	"ap-northeast-2": {"ap-northeast-2a", "ap-northeast-2b", "ap-northeast-2c", "ap-northeast-2d"},
	"ap-northeast-3": {"ap-northeast-3a", "ap-northeast-3b", "ap-northeast-3c"},
	"ap-south-1":     {"ap-south-1a", "ap-south-1b", "ap-south-1c"},
	"ap-southeast-1": {"ap-southeast-1a", "ap-southeast-1b", "ap-southeast-1c"},
	"ap-southeast-2": {"ap-southeast-2a", "ap-southeast-2b", "ap-southeast-2c"},
	"ca-central-1":   {"ca-central-1a", "ca-central-1b", "ca-central-1d"},
	"eu-central-1":   {"eu-central-1a", "eu-central-1b", "eu-central-1c"},
	"eu-north-1":     {"eu-north-1a", "eu-north-1b", "eu-north-1c"},
	"eu-west-1":      {"eu-west-1a", "eu-west-1b", "eu-west-1c"},
	"eu-west-2":      {"eu-west-2a", "eu-west-2b", "eu-west-2c"},
	"eu-west-3":      {"eu-west-3a", "eu-west-3b", "eu-west-3c"},
	"me-south-1":     {"me-south-1a", "me-south-1b", "me-south-1c"},
	"sa-east-1":      {"sa-east-1a", "sa-east-1b", "sa-east-1c"},
	"us-east-1":      {"us-east-1a", "us-east-1b", "us-east-1c", "us-east-1d", "us-east-1e", "us-east-1f"},
	"us-east-2":      {"us-east-2a", "us-east-2b", "us-east-2c"},
	"us-west-1":      {"us-west-1a", "us-west-1b"},
	"us-west-2":      {"us-west-2a", "us-west-2b", "us-west-2c", "us-west-2d"},
}

func calculateTotEventsWithTextTemplate(totSize uint64, fieldMap map[string]any, errChan chan error, tpl []byte, templateFns template.FuncMap) (uint64, error) {
	if totSize == 0 {
		return 0, nil
	}

	// Generate a single event to calculate the total number of events based on its size
	t := template.New("estimate_tot_events")
	t = t.Option("missingkey=error")
	tempTemplateFns := template.FuncMap{}
	for k, v := range templateFns {
		tempTemplateFns[k] = v
	}

	tempTemplateFns["generate"] = func(field string) any {
		state := NewGenState()
		state.prevCacheForDup[field] = make(map[any]struct{})
		state.prevCacheCardinality[field] = make([]any, 0)
		bindF, ok := fieldMap[field].(EmitF)
		if !ok {
			close(errChan)
			return nil
		}

		return bindF(state)
	}

generateErr:
	for {
		select {
		case <-errChan:
			return 0, generateOnFieldNotInFieldsYaml
		default:
			break generateErr
		}
	}

	parsedTpl, err := t.Funcs(templateFns).Parse(string(tpl))
	if err != nil {
		return 0, err
	}

	buf := bytes.NewBufferString("")
	err = parsedTpl.Execute(buf, nil)
	if err != nil {
		return 0, err
	}

	singleEventSize := uint64(buf.Len())
	if singleEventSize == 0 {
		return 1, nil
	}

	totEvents := totSize / singleEventSize
	if totEvents < 1 {
		totEvents = 1
	}

	return totEvents, nil
}

func NewGeneratorWithTextTemplate(tpl []byte, cfg Config, fields Fields, totSize uint64) (*GeneratorWithTextTemplate, error) {
	// Preprocess the fields, generating appropriate bound function
	state := NewGenState()
	fieldMap := make(map[string]any)
	for _, field := range fields {
		if err := bindField(cfg, field, fieldMap, true); err != nil {
			return nil, err
		}

		state.prevCacheForDup[field.Name] = make(map[any]struct{})
		state.prevCacheCardinality[field.Name] = make([]any, 0)
	}

	errChan := make(chan error)

	templateFns := sprig.TxtFuncMap()

	templateFns["awsAZFromRegion"] = func(region string) string {
		azs, ok := awsAZs[region]
		if !ok {
			return "NoAZ"
		}

		return azs[rand.Intn(len(azs))]
	}

	templateFns["generate"] = func(field string) any {
		bindF, ok := fieldMap[field].(EmitF)
		if !ok {
			close(errChan)
			return nil
		}

		return bindF(state)
	}

	totEvents, err := calculateTotEventsWithTextTemplate(totSize, fieldMap, errChan, tpl, templateFns)
	if err != nil {
		return nil, err
	}

	t := template.New("generator")
	t = t.Option("missingkey=error")

	parsedTpl, err := t.Funcs(templateFns).Parse(string(tpl))
	if err != nil {
		return nil, err
	}

	return &GeneratorWithTextTemplate{tpl: parsedTpl, totEvents: totEvents, state: state, errChan: errChan}, nil
}

func (gen GeneratorWithTextTemplate) Close() error {
	return nil
}

func (gen GeneratorWithTextTemplate) Emit(state *GenState, buf *bytes.Buffer) error {
	state = gen.state
	if err := gen.emit(state, buf); err != nil {
		return err
	}

	state.counter += 1

	return nil
}

func (gen GeneratorWithTextTemplate) emit(state *GenState, buf *bytes.Buffer) error {
	if gen.totEvents == 0 || state.counter < gen.totEvents {
		select {
		case <-gen.errChan:
			return generateOnFieldNotInFieldsYaml
		default:
			err := gen.tpl.Execute(buf, nil)
			if err != nil {
				return err
			}
		}
	} else {
		return io.EOF
	}

	return nil
}
