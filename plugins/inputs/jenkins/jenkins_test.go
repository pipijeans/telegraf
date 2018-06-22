package jenkins

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/testutil"
	"github.com/kelwang/gojenkins"
)

func TestErr(t *testing.T) {
	tests := []struct {
		err    *Error
		output string
	}{
		{
			nil,
			"",
		},
		{
			&Error{
				reference: errConnectJenkins,
				url:       "http://badurl.com",
				err:       errors.New("unknown error"),
			},
			"error connect jenkins instance[http://badurl.com]: unknown error",
		},
		{
			newError(errors.New("2"), errEmptyMonitorData, "http://badurl.com"),
			"error empty monitor data[http://badurl.com]: 2",
		},
		{
			badFormatErr("http://badurl.com", 20.12, "string", "arch"),
			"error bad format[http://badurl.com]: fieldName: arch, want string, got float64",
		},
	}
	for _, test := range tests {
		output := test.err.Error()
		if output != test.output {
			t.Errorf("Expected %s, got %s\n", test.output, output)
		}
	}
}

func TestJobRequest(t *testing.T) {
	tests := []struct {
		input  jobRequest
		output string
	}{
		{
			jobRequest{},
			"",
		},
		{
			jobRequest{
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

type mockHandler struct {
	// responseMap is the path to repsonse interface
	// we will ouput the serialized response in json when serving http
	// example '/computer/api/json': *gojenkins.
	responseMap map[string]interface{}
}

func (h mockHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o, ok := h.responseMap[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	b, err := json.Marshal(o)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

// copied the embed struct from gojenkins lib
type monitorData struct {
	Hudson_NodeMonitors_ArchitectureMonitor interface{} `json:"hudson.node_monitors.ArchitectureMonitor"`
	Hudson_NodeMonitors_ClockMonitor        interface{} `json:"hudson.node_monitors.ClockMonitor"`
	Hudson_NodeMonitors_DiskSpaceMonitor    interface{} `json:"hudson.node_monitors.DiskSpaceMonitor"`
	Hudson_NodeMonitors_ResponseTimeMonitor struct {
		Average int64 `json:"average"`
	} `json:"hudson.node_monitors.ResponseTimeMonitor"`
	Hudson_NodeMonitors_SwapSpaceMonitor      interface{} `json:"hudson.node_monitors.SwapSpaceMonitor"`
	Hudson_NodeMonitors_TemporarySpaceMonitor interface{} `json:"hudson.node_monitors.TemporarySpaceMonitor"`
}

func TestGatherNodeData(t *testing.T) {
	tests := []struct {
		name   string
		input  mockHandler
		output *testutil.Accumulator
		oe     *Error
	}{
		{
			name: "bad endpoint",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json":          struct{}{},
					"/computer/api/json": nil,
				},
			},
			oe: &Error{
				reference: errRetrieveNode,
			},
		},
		{
			name: "bad node data",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": struct{}{},
					"/computer/api/json": gojenkins.Computers{
						Computers: []*gojenkins.NodeResponse{
							{},
							{},
							{},
						},
					},
				},
			},
			oe: &Error{
				reference: errEmptyNodeName,
			},
		},
		{
			name: "bad empty monitor data",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": struct{}{},
					"/computer/api/json": gojenkins.Computers{
						Computers: []*gojenkins.NodeResponse{
							{DisplayName: "master"},
							{DisplayName: "node1"},
						},
					},
				},
			},
			oe: &Error{
				reference: errEmptyMonitorData,
			},
		},
		{
			name: "bad monitor data format",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": struct{}{},
					"/computer/api/json": gojenkins.Computers{
						Computers: []*gojenkins.NodeResponse{
							{DisplayName: "master", MonitorData: monitorData{
								Hudson_NodeMonitors_ArchitectureMonitor: 1,
							}},
						},
					},
				},
			},
			oe: &Error{
				reference: errBadFormat,
			},
		},
		{
			name: "filtered nodes",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": struct{}{},
					"/computer/api/json": gojenkins.Computers{
						Computers: []*gojenkins.NodeResponse{
							{DisplayName: "ignore-1"},
							{DisplayName: "ignore-2"},
						},
					},
				},
			},
		},
		{
			name: "normal data collection",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": struct{}{},
					"/computer/api/json": gojenkins.Computers{
						Computers: []*gojenkins.NodeResponse{
							{
								DisplayName: "master",
								MonitorData: monitorData{
									Hudson_NodeMonitors_ArchitectureMonitor: "linux",
									Hudson_NodeMonitors_ResponseTimeMonitor: struct {
										Average int64 `json:"average"`
									}{
										Average: 10032,
									},
									Hudson_NodeMonitors_DiskSpaceMonitor: map[string]interface{}{
										"path": "/path/1",
										"size": 123,
									},
									Hudson_NodeMonitors_TemporarySpaceMonitor: map[string]interface{}{
										"path": "/path/2",
										"size": 245,
									},
									Hudson_NodeMonitors_SwapSpaceMonitor: map[string]interface{}{
										"availableSwapSpace":      212,
										"totalSwapSpace":          500,
										"availablePhysicalMemory": 101,
										"totalPhysicalMemory":     500,
									},
								},
								Offline: false,
							},
						},
					},
				},
			},
			output: &testutil.Accumulator{
				Metrics: []*testutil.Metric{
					{
						Tags: map[string]string{
							"node_name": "master",
							"arch":      "linux",
							"status":    "online",
							"disk_path": "/path/1",
							"temp_path": "/path/2",
						},
						Fields: map[string]interface{}{
							"response_time":    int64(10032),
							"disk_available":   float64(123),
							"temp_available":   float64(245),
							"swap_available":   float64(212),
							"swap_total":       float64(500),
							"memory_available": float64(101),
							"memory_total":     float64(500),
						},
					},
				},
			},
		},
	}
	for _, test := range tests {
		ts := httptest.NewServer(test.input)
		defer ts.Close()
		j := &Jenkins{
			URL:             ts.URL,
			ResponseTimeout: internal.Duration{Duration: time.Microsecond},
			NodeExclude:     []string{"ignore-1", "ignore-2"},
		}
		te := j.newInstance(ts.Client())
		acc := new(testutil.Accumulator)
		j.gatherNodesData(acc)
		if err := acc.FirstError(); err != nil {
			te = err.(*Error)
		}

		if test.oe == nil && te != nil {
			t.Fatalf("%s: failed %s, expected to be nil", test.name, te.Error())
		} else if test.oe != nil {
			test.oe.url = ts.URL + "/computer/api/json"
			if te == nil {
				t.Fatalf("%s: want err: %s, got nil", test.name, test.oe.Error())
			}
			if test.oe.reference != te.reference {
				t.Fatalf("%s: bad error msg Expected %s, got %s\n", test.name, test.oe.reference, te.reference)
			}
			if test.oe.url != te.url {
				t.Fatalf("%s: bad error url Expected %s, got %s\n", test.name, test.oe.url, te.url)
			}
		}
		if test.output == nil && len(acc.Metrics) > 0 {
			t.Fatalf("%s: collected extra data", test.name)
		} else if test.output != nil && len(test.output.Metrics) > 0 {
			for k, m := range test.output.Metrics[0].Tags {
				if acc.Metrics[0].Tags[k] != m {
					t.Fatalf("%s: tag %s metrics unmatch Expected %s, got %s\n", test.name, k, m, acc.Metrics[0].Tags[k])
				}
			}
			for k, m := range test.output.Metrics[0].Fields {
				if acc.Metrics[0].Fields[k] != m {
					t.Fatalf("%s: field %s metrics unmatch Expected %v(%T), got %v(%T)\n", test.name, k, m, m, acc.Metrics[0].Fields[k], acc.Metrics[0].Fields[k])
				}
			}
		}
	}
}

func TestNewInstance(t *testing.T) {
	mh := mockHandler{
		responseMap: map[string]interface{}{
			"/api/json": struct{}{},
		},
	}
	ts := httptest.NewServer(mh)
	defer ts.Close()
	mockClient := ts.Client()
	tests := []struct {
		// name of the test
		name   string
		input  *Jenkins
		output *Jenkins
		oe     *Error
	}{
		{
			name: "bad jenkins config",
			input: &Jenkins{
				URL:             "http://a bad url",
				ResponseTimeout: internal.Duration{Duration: time.Microsecond},
			},
			oe: &Error{
				url:       "http://a bad url",
				reference: errConnectJenkins,
			},
		},
		{
			name: "has filter",
			input: &Jenkins{
				URL:             ts.URL,
				ResponseTimeout: internal.Duration{Duration: time.Microsecond},
				JobExclude:      []string{"job1", "job2"},
				NodeExclude:     []string{"node1", "node2"},
			},
		},
		{
			name: "default config",
			input: &Jenkins{
				URL:             ts.URL,
				ResponseTimeout: internal.Duration{Duration: time.Microsecond},
			},
			output: &Jenkins{
				MaxConnections:    30,
				MaxSubJobPerLayer: 10,
			},
		},
	}
	for _, test := range tests {
		te := test.input.newInstance(mockClient)
		if test.oe == nil && te != nil {
			t.Fatalf("%s: failed %s, expected to be nil", test.name, te.Error())
		} else if test.oe != nil {
			if test.oe.reference != te.reference {
				t.Fatalf("%s: bad error msg Expected %s, got %s\n", test.name, test.oe.reference, te.reference)
			}
			if test.oe.url != te.url {
				t.Fatalf("%s: bad error url Expected %s, got %s\n", test.name, test.oe.url, te.url)
			}
		}
		if test.output != nil {
			if test.input.instance == nil {
				t.Fatalf("%s: failed %s, jenkins instance shouldn't be nil", test.name, te.Error())
			}
			if test.input.MaxConnections != test.output.MaxConnections {
				t.Fatalf("%s: different MaxConnections Expected %d, got %d\n", test.name, test.output.MaxConnections, test.input.MaxConnections)
			}
		}

	}
}

func TestGatherJobs(t *testing.T) {
	tests := []struct {
		name   string
		input  mockHandler
		output *testutil.Accumulator
		oe     *Error
	}{
		{
			name:  "empty job",
			input: mockHandler{},
		},
		{
			name: "bad inner jobs",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "job1"},
						},
					},
				},
			},
			oe: &Error{
				reference: errRetrieveInnerJobs,
				url:       "/job/job1/api/json",
			},
		},
		{
			name: "jobs has no build",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "job1"},
						},
					},
					"/job/job1/api/json": &gojenkins.JobResponse{},
				},
			},
		},
		{
			name: "bad build info",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "job1"},
						},
					},
					"/job/job1/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 1,
						},
					},
				},
			},
			oe: &Error{
				url:       "/job/job1/1/api/json",
				reference: errRetrieveLatestBuild,
			},
		},
		{
			name: "ignore building job",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "job1"},
						},
					},
					"/job/job1/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 1,
						},
					},
					"/job/job1/1/api/json": &gojenkins.BuildResponse{
						Building: true,
					},
				},
			},
		},
		{
			name: "ignore old build",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "job1"},
						},
					},
					"/job/job1/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 2,
						},
					},
					"/job/job1/2/api/json": &gojenkins.BuildResponse{
						Building:  false,
						Timestamp: 100,
					},
				},
			},
		},
		{
			name: "gather metrics",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "job1"},
							{Name: "job2"},
						},
					},
					"/job/job1/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 3,
						},
					},
					"/job/job2/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 1,
						},
					},
					"/job/job1/3/api/json": &gojenkins.BuildResponse{
						Building:  false,
						Result:    "SUCCESS",
						Duration:  25558,
						Timestamp: (time.Now().Unix() - int64(time.Minute.Seconds())) * 1000,
					},
					"/job/job2/1/api/json": &gojenkins.BuildResponse{
						Building:  false,
						Result:    "FAILURE",
						Duration:  1558,
						Timestamp: (time.Now().Unix() - int64(time.Minute.Seconds())) * 1000,
					},
				},
			},
			output: &testutil.Accumulator{
				Metrics: []*testutil.Metric{
					{
						Tags: map[string]string{
							"job_name": "job1",
							"result":   "SUCCESS",
						},
						Fields: map[string]interface{}{
							"duration":    int64(25558),
							"result_code": 0,
						},
					},
					{
						Tags: map[string]string{
							"job_name": "job2",
							"result":   "FAILURE",
						},
						Fields: map[string]interface{}{
							"duration":    int64(1558),
							"result_code": 1,
						},
					},
				},
			},
		},
		{
			name: "gather sub jobs, jobs filter",
			input: mockHandler{
				responseMap: map[string]interface{}{
					"/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "apps"},
							{Name: "ignore-1"},
						},
					},
					"/job/apps/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "k8s-cloud"},
							{Name: "chronograf"},
							{Name: "ignore-all"},
						},
					},
					"/job/apps/job/ignore-all/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "1"},
							{Name: "2"},
						},
					},
					"/job/apps/job/chronograf/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 1,
						},
					},
					"/job/apps/job/k8s-cloud/api/json": &gojenkins.JobResponse{
						Jobs: []gojenkins.InnerJob{
							{Name: "PR-100"},
							{Name: "PR-101"},
							{Name: "PR-ignore2"},
						},
					},
					"/job/apps/job/k8s-cloud/job/PR-100/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 1,
						},
					},
					"/job/apps/job/k8s-cloud/job/PR-101/api/json": &gojenkins.JobResponse{
						LastBuild: gojenkins.JobBuild{
							Number: 4,
						},
					},
					"/job/apps/job/chronograf/1/api/json": &gojenkins.BuildResponse{
						Building:  false,
						Result:    "FAILURE",
						Duration:  1558,
						Timestamp: (time.Now().Unix() - int64(time.Minute.Seconds())) * 1000,
					},
					"/job/apps/job/k8s-cloud/job/PR-101/4/api/json": &gojenkins.BuildResponse{
						Building:  false,
						Result:    "SUCCESS",
						Duration:  76558,
						Timestamp: (time.Now().Unix() - int64(time.Minute.Seconds())) * 1000,
					},
					"/job/apps/job/k8s-cloud/job/PR-100/1/api/json": &gojenkins.BuildResponse{
						Building:  false,
						Result:    "SUCCESS",
						Duration:  91558,
						Timestamp: (time.Now().Unix() - int64(time.Minute.Seconds())) * 1000,
					},
				},
			},
			output: &testutil.Accumulator{
				Metrics: []*testutil.Metric{
					{
						Tags: map[string]string{
							"job_name": "apps/chronograf",
							"result":   "FAILURE",
						},
						Fields: map[string]interface{}{
							"duration":    int64(1558),
							"result_code": 1,
						},
					},
					{
						Tags: map[string]string{
							"job_name": "apps/k8s-cloud/PR-100",
							"result":   "SUCCESS",
						},
						Fields: map[string]interface{}{
							"duration":    int64(91558),
							"result_code": 0,
						},
					},
					{
						Tags: map[string]string{
							"job_name": "apps/k8s-cloud/PR-101",
							"result":   "SUCCESS",
						},
						Fields: map[string]interface{}{
							"duration":    int64(76558),
							"result_code": 0,
						},
					},
				},
			},
		},
	}
	for _, test := range tests {
		ts := httptest.NewServer(test.input)
		defer ts.Close()
		j := &Jenkins{
			URL:             ts.URL,
			MaxBuildAge:     internal.Duration{Duration: time.Hour},
			ResponseTimeout: internal.Duration{Duration: time.Microsecond},
			JobExclude: []string{
				"ignore-1",
				"apps/ignore-all/*",
				"apps/k8s-cloud/PR-ignore2",
			},
		}
		te := j.newInstance(ts.Client())
		acc := new(testutil.Accumulator)
		j.gatherJobs(acc)
		if err := acc.FirstError(); err != nil {
			te = err.(*Error)
		}
		if test.oe == nil && te != nil {
			t.Fatalf("%s: failed %s, expected to be nil", test.name, te.Error())
		} else if test.oe != nil {
			test.oe.url = ts.URL + test.oe.url
			if te == nil {
				t.Fatalf("%s: want err: %s, got nil", test.name, test.oe.Error())
			}
			if test.oe.reference != te.reference {
				t.Fatalf("%s: bad error msg Expected %s, got %s\n", test.name, test.oe.reference, te.reference)
			}
			if test.oe.url != te.url {
				t.Fatalf("%s: bad error url Expected %s, got %s\n", test.name, test.oe.url, te.url)
			}
		}
		if test.output != nil && len(test.output.Metrics) > 0 {
			// sort metrics
			sort.Slice(acc.Metrics, func(i, j int) bool {
				return strings.Compare(acc.Metrics[i].Tags["job_name"], acc.Metrics[j].Tags["job_name"]) < 0
			})
			for i := range test.output.Metrics {
				for k, m := range test.output.Metrics[i].Tags {
					if acc.Metrics[i].Tags[k] != m {
						t.Fatalf("%s: tag %s metrics unmatch Expected %s, got %s\n", test.name, k, m, acc.Metrics[i].Tags[k])
					}
				}
				for k, m := range test.output.Metrics[i].Fields {
					if acc.Metrics[i].Fields[k] != m {
						t.Fatalf("%s: field %s metrics unmatch Expected %v(%T), got %v(%T)\n", test.name, k, m, m, acc.Metrics[i].Fields[k], acc.Metrics[0].Fields[k])
					}
				}
			}

		}
	}
}
