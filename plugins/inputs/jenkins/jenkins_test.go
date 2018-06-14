package jenkins

import (
	"errors"
	"testing"
)

func TestErr(t *testing.T) {
	tests := []struct {
		err    *typedErr
		output string
	}{
		{
			nil,
			"",
		},
		{
			&typedErr{
				reference: errConnectJenkins,
				url:       "http://badurl.com",
				err:       errors.New("unknown error"),
			},
			"error connect jenkins instance[http://badurl.com]: unknown error",
		},
		{
			wrapErr(typedErr{
				reference: errConnectJenkins,
				url:       "http://badurl.com",
				err:       errors.New("unknown error"),
			}, errors.New("2"), errEmptyMonitorData),
			"error empty monitor data[http://badurl.com]: 2",
		},
		{
			badFormatErr(typedErr{
				reference: errConnectJenkins,
				url:       "http://badurl.com",
				err:       errors.New("unknown error"),
			}, "20", "float64", "arch"),
			"error bad format[http://badurl.com]: fieldName: arch, want float64, got string",
		},
	}
	for _, test := range tests {
		output := test.err.Error()
		if output != test.output {
			t.Errorf("Expected %s, got %s\n", test.output, output)
		}
	}
}

func TestSrcJob(t *testing.T) {
	tests := []struct {
		input  srcJob
		output string
	}{
		{
			srcJob{},
			"",
		},
		{
			srcJob{
				name:    "1",
				parents: []string{"3", "2"},
			},
			"3/2/1",
		},
	}
	for _, test := range tests {
		output := test.input.hierarchyName()
		if output != test.output {
			t.Errorf("Expected %s, got %s\n", test.output, output)
		}
	}
}

func TestResultCode(t *testing.T) {
	tests := []struct {
		input  string
		output int
	}{
		{"SUCCESS", 0},
		{"Failure", 1},
		{"NOT_BUILT", 2},
		{"UNSTABLE", 3},
		{"ABORTED", 4},
	}
	for _, test := range tests {
		output := mapResultCode(test.input)
		if output != test.output {
			t.Errorf("Expected %d, got %d\n", test.output, output)
		}
	}
}
