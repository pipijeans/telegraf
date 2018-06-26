package jenkins

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/filter"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/internal/tls"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/kelwang/gojenkins"
)

// Jenkins plugin gathers information about the nodes and jobs running in a jenkins instance.
type Jenkins struct {
	URL      string
	Username string
	Password string
	// HTTP Timeout specified as a string - 3s, 1m, 1h
	ResponseTimeout internal.Duration

	tls.ClientConfig
	instance *gojenkins.Jenkins

	MaxConnections    int               `toml:"max_connections"`
	MaxBuildAge       internal.Duration `toml:"max_build_age"`
	MaxSubJobDepth    int               `toml:"max_subjob_depth"`
	MaxSubJobPerLayer int               `toml:"max_subjob_per_layer"`
	JobExclude        []string          `toml:"job_exclude"`
	jobFilter         filter.Filter

	NodeExclude []string `toml:"node_exclude"`
	nodeFilter  filter.Filter

	semaphore chan struct{}
}

type byBuildNumber []gojenkins.JobBuild

const sampleConfig = `
url = "http://my-jenkins-instance:8080"
#  username = "admin"
#  password = "admin"
## Set response_timeout
response_timeout = "5s"

## Optional SSL Config
#  ssl_ca = /path/to/cafile
#  ssl_cert = /path/to/certfile
#  ssl_key = /path/to/keyfile
## Use SSL but skip chain & host verification
#  insecure_skip_verify = false

## Job & build filter
#  max_build_age = "1h"
## jenkins can have unlimited layer of sub jobs
## this config will limit the layers of pull, default value 0 means
## unlimited pulling until no more sub jobs
#  max_subjob_depth = 0
## in workflow-multibranch-plugin, each branch will be created as a sub job
## this config will limit to call only the lasted branches
## sub jobs fetch in each layer
#  empty will use default value 10
#  max_subjob_per_layer = 10
#  job_exclude = [ "job1", "job2/subjob1/subjob2", "job3/*"]

## Node filter
#  node_exclude = [ "node1", "node2" ]

## Woker pool for jenkins plugin only
#  empty this field will use default value 30
#  max_connections = 30
`

// measurement
const (
	measurementNode = "jenkins_node"
	measurementJob  = "jenkins_job"
)

func badFormatErr(url string, field interface{}, want string, fieldName string) error {
	return fmt.Errorf("error bad format[%s]: fieldName: %s, want %s, got %T", url, fieldName, want, field)
}

// SampleConfig implements telegraf.Input interface
func (j *Jenkins) SampleConfig() string {
	return sampleConfig
}

// Description implements telegraf.Input interface
func (j *Jenkins) Description() string {
	return "Read jobs and cluster metrics from Jenkins instances"
}

// Gather implements telegraf.Input interface
func (j *Jenkins) Gather(acc telegraf.Accumulator) error {
	if j.instance == nil {
		client, te := j.initClient()
		if te != nil {
			return te
		}
		if te = j.newInstance(client); te != nil {
			return te
		}
	}

	j.gatherNodesData(acc)
	j.gatherJobs(acc)

	return nil
}

func (j *Jenkins) initClient() (*http.Client, error) {
	tlsCfg, err := j.ClientConfig.TLSConfig()
	if err != nil {
		return nil, fmt.Errorf("error parse jenkins config[%s]: %v", j.URL, err)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			MaxIdleConns:    j.MaxConnections,
		},
		Timeout: j.ResponseTimeout.Duration,
	}, nil
}

// seperate the client as dependency to use httptest Client for mocking
func (j *Jenkins) newInstance(client *http.Client) error {
	// create instance
	var err error
	j.instance, err = gojenkins.CreateJenkins(client, j.URL, j.Username, j.Password).Init()
	if err != nil {
		return fmt.Errorf("error connect jenkins instance[%s]: %v", j.URL, err)
	}

	// init job filter
	j.jobFilter, err = filter.Compile(j.JobExclude)
	if err != nil {
		return fmt.Errorf("error compile job filters[%s]: %v", j.URL, err)
	}

	// init node filter
	j.nodeFilter, err = filter.Compile(j.NodeExclude)
	if err != nil {
		return fmt.Errorf("error compile node filters[%s]: %v", j.URL, err)
	}

	// init tcp pool with default value
	if j.MaxConnections <= 0 {
		j.MaxConnections = 30
	}

	// default sub jobs can be acquired
	if j.MaxSubJobPerLayer <= 0 {
		j.MaxSubJobPerLayer = 10
	}

	j.semaphore = make(chan struct{}, j.MaxConnections)

	return nil
}

func (j *Jenkins) gatherNodeData(node *gojenkins.Node, url string, acc telegraf.Accumulator) error {
	tags := map[string]string{}
	fields := make(map[string]interface{})

	info := node.Raw

	// detect the parsing error, since gojenkins lib won't do it
	if info == nil || info.DisplayName == "" {
		return fmt.Errorf("error empty node name[%s]: ", url)
	}

	tags["node_name"] = info.DisplayName
	var ok bool
	// filter out excluded node_name
	if j.nodeFilter != nil && j.nodeFilter.Match(tags["node_name"]) {
		return nil
	}

	if info.MonitorData.Hudson_NodeMonitors_ArchitectureMonitor == nil {
		return fmt.Errorf("error empty monitor data[%s]: ", url)
	}
	tags["arch"], ok = info.MonitorData.Hudson_NodeMonitors_ArchitectureMonitor.(string)
	if !ok {
		return badFormatErr(url, info.MonitorData.Hudson_NodeMonitors_ArchitectureMonitor, "string", "hudson.node_monitors.ArchitectureMonitor")
	}

	tags["status"] = "online"
	if info.Offline {
		tags["status"] = "offline"
	}
	fields["response_time"] = info.MonitorData.Hudson_NodeMonitors_ResponseTimeMonitor.Average
	if diskSpaceMonitor := info.MonitorData.Hudson_NodeMonitors_DiskSpaceMonitor; diskSpaceMonitor != nil {
		diskSpaceMonitorRoute := "hudson.node_monitors.DiskSpaceMonitor"
		var diskSpace map[string]interface{}
		if diskSpace, ok = diskSpaceMonitor.(map[string]interface{}); !ok {
			return badFormatErr(url, diskSpaceMonitor, "map[string]interface{}", diskSpaceMonitorRoute)
		}
		if tags["disk_path"], ok = diskSpace["path"].(string); !ok {
			return badFormatErr(url, diskSpace["path"], "string", diskSpaceMonitorRoute+".path")
		}
		if fields["disk_available"], ok = diskSpace["size"].(float64); !ok {
			return badFormatErr(url, diskSpace["size"], "float64", diskSpaceMonitorRoute+".size")
		}
	}

	if tempSpaceMonitor := info.MonitorData.Hudson_NodeMonitors_TemporarySpaceMonitor; tempSpaceMonitor != nil {
		tempSpaceMonitorRoute := "hudson.node_monitors.TemporarySpaceMonitor"
		var tempSpace map[string]interface{}
		if tempSpace, ok = tempSpaceMonitor.(map[string]interface{}); !ok {
			return badFormatErr(url, tempSpaceMonitor, "map[string]interface{}", tempSpaceMonitorRoute)
		}
		if tags["temp_path"], ok = tempSpace["path"].(string); !ok {
			return badFormatErr(url, tempSpace["path"], "string", tempSpaceMonitorRoute+".path")
		}
		if fields["temp_available"], ok = tempSpace["size"].(float64); !ok {
			return badFormatErr(url, tempSpace["size"], "float64", tempSpaceMonitorRoute+".size")
		}
	}

	if swapSpaceMonitor := info.MonitorData.Hudson_NodeMonitors_SwapSpaceMonitor; swapSpaceMonitor != nil {
		swapSpaceMonitorRouter := "hudson.node_monitors.SwapSpaceMonitor"
		var swapSpace map[string]interface{}
		if swapSpace, ok = swapSpaceMonitor.(map[string]interface{}); !ok {
			return badFormatErr(url, swapSpaceMonitor, "map[string]interface{}", swapSpaceMonitorRouter)
		}
		if fields["swap_available"], ok = swapSpace["availableSwapSpace"].(float64); !ok {
			return badFormatErr(url, swapSpace["availableSwapSpace"], "float64", swapSpaceMonitorRouter+".availableSwapSpace")
		}
		if fields["swap_total"], ok = swapSpace["totalSwapSpace"].(float64); !ok {
			return badFormatErr(url, swapSpace["totalSwapSpace"], "float64", swapSpaceMonitorRouter+".totalSwapSpace")
		}
		if fields["memory_available"], ok = swapSpace["availablePhysicalMemory"].(float64); !ok {
			return badFormatErr(url, swapSpace["availablePhysicalMemory"], "float64", swapSpaceMonitorRouter+".availablePhysicalMemory")
		}
		if fields["memory_total"], ok = swapSpace["totalPhysicalMemory"].(float64); !ok {
			return badFormatErr(url, swapSpace["totalPhysicalMemory"], "float64", swapSpaceMonitorRouter+".totalPhysicalMemory")
		}
	}
	acc.AddFields(measurementNode, fields, tags)

	return nil
}

func (j *Jenkins) gatherNodesData(acc telegraf.Accumulator) {
	var nodes []*gojenkins.Node
	var err error
	err = j.doGet(func() error {
		nodes, err = j.instance.GetAllNodes()
		return err
	})

	url := j.URL + "/computer/api/json"
	// since gojenkins lib will never return error
	// returns error for len(nodes) is 0
	if err != nil || len(nodes) == 0 {
		acc.AddError(fmt.Errorf("error retrieving nodes[%s]: %v", url, err))
		return
	}
	// get node data
	for _, node := range nodes {
		te := j.gatherNodeData(node, url, acc)
		if te == nil {
			continue
		}
		acc.AddError(te)
	}
}

func (j *Jenkins) gatherJobs(acc telegraf.Accumulator) {
	jobs, err := j.instance.GetAllJobNames()
	if err != nil {
		acc.AddError(fmt.Errorf("error retrieving jobs[%s]: %v", j.URL, err))
		return
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(name string, wg *sync.WaitGroup, acc telegraf.Accumulator) {
			defer wg.Done()
			if te := j.getJobDetail(jobRequest{
				name:    name,
				parents: []string{},
				layer:   0,
			}, wg, acc); te != nil {
				acc.AddError(te)
			}
		}(job.Name, &wg, acc)
	}
	wg.Wait()
}

// wrap the tcp request with doGet
// block tcp request if buffered channel is full
func (j *Jenkins) doGet(tcp func() error) error {
	j.semaphore <- struct{}{}
	if err := tcp(); err != nil {
		if err == gojenkins.ErrSessionExpired {
			// ignore the error here, since config parsing should be finished.
			client, _ := j.initClient()
			// SessionExpired use a go routine to create a new session
			go j.newInstance(client)
		}
		<-j.semaphore
		return err
	}
	<-j.semaphore
	return nil
}

func (j *Jenkins) getJobDetail(sj jobRequest, wg *sync.WaitGroup, acc telegraf.Accumulator) error {
	if j.MaxSubJobDepth > 0 && sj.layer == j.MaxSubJobDepth {
		return nil
	}
	// filter out excluded job.
	if j.jobFilter != nil && j.jobFilter.Match(sj.hierarchyName()) {
		return nil
	}
	url := j.URL + "/job/" + strings.Join(sj.combined(), "/job/") + "/api/json"
	var jobDetail *gojenkins.Job
	var err error
	err = j.doGet(func() error {
		jobDetail, err = j.instance.GetJob(sj.name, sj.parents...)
		return err
	})
	if err != nil {
		return fmt.Errorf("error retrieving inner jobs[%s]: ", url)
	}

	for k, innerJob := range jobDetail.Raw.Jobs {
		if k < len(jobDetail.Raw.Jobs)-j.MaxSubJobPerLayer-1 {
			continue
		}
		wg.Add(1)
		// schedule tcp fetch for inner jobs
		go func(innerJob gojenkins.InnerJob, sj jobRequest, wg *sync.WaitGroup, acc telegraf.Accumulator) {
			defer wg.Done()
			if te := j.getJobDetail(jobRequest{
				name:    innerJob.Name,
				parents: sj.combined(),
				layer:   sj.layer + 1,
			}, wg, acc); te != nil {
				acc.AddError(te)
			}
		}(innerJob, sj, wg, acc)
	}

	// collect build info
	number := jobDetail.Raw.LastBuild.Number
	if number < 1 {
		// no build info
		return nil
	}
	baseURL := "/job/" + strings.Join(sj.combined(), "/job/") + "/" + strconv.Itoa(int(number))
	// jobDetail.GetBuild is not working, doing poll directly
	build := &gojenkins.Build{
		Jenkins: j.instance,
		Depth:   1,
		Base:    baseURL,
		Raw:     new(gojenkins.BuildResponse),
	}
	var status int
	err = j.doGet(func() error {
		status, err = build.Poll()
		return err
	})
	if err != nil || status != 200 {
		if err == nil && status != 200 {
			err = fmt.Errorf("status code %d", status)
		}
		return fmt.Errorf("error retrieving inner jobs[%s]: %v", j.URL+baseURL+"/api/json", err)
	}

	if build.Raw.Building {
		log.Printf("D! Ignore running build on %s, build %v", sj.name, number)
		return nil
	}

	// stop if build is too old

	if (j.MaxBuildAge != internal.Duration{Duration: 0}) {
		buildAgo := time.Now().Sub(build.GetTimestamp())
		if buildAgo.Seconds() > j.MaxBuildAge.Duration.Seconds() {
			log.Printf("D! Job %s build %v too old (%s ago), skipping to next job", sj.name, number, buildAgo)
			return nil
		}
	}

	gatherJobBuild(sj, build, acc)
	return nil
}

type jobRequest struct {
	name    string
	parents []string
	layer   int
}

func (sj jobRequest) combined() []string {
	return append(sj.parents, sj.name)
}

func (sj jobRequest) hierarchyName() string {
	return strings.Join(sj.combined(), "/")
}

func gatherJobBuild(sj jobRequest, build *gojenkins.Build, acc telegraf.Accumulator) {
	tags := map[string]string{"job_name": sj.hierarchyName(), "result": build.GetResult()}
	fields := make(map[string]interface{})
	fields["duration"] = build.GetDuration()
	fields["result_code"] = mapResultCode(build.GetResult())

	acc.AddFields(measurementJob, fields, tags, build.GetTimestamp())
}

// perform status mapping
func mapResultCode(s string) int {
	switch strings.ToLower(s) {
	case "success":
		return 0
	case "failure":
		return 1
	case "not_built":
		return 2
	case "unstable":
		return 3
	case "aborted":
		return 4
	}
	return -1
}

func init() {
	inputs.Add("jenkins", func() telegraf.Input {
		return &Jenkins{}
	})
}
