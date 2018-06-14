package jenkins

import (
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
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

	MaxTCPConcurrentConnections int               `toml:"max_tcp_concurrent_connections"`
	MaxBuildAge                 internal.Duration `toml:"max_build_age"`
	MaxSubJobsLayer             int               `toml:"max_sub_jobs_layer"`
	NewestSubJobsEachLayer      int               `toml:"newest_sub_jobs_each_layer"`
	JobExclude                  []string          `toml:"job_exclude"`
	jobFilter                   map[string]bool

	NodeExclude []string `toml:"node_exclude"`
	nodeFilter  map[string]bool
}

type byBuildNumber []gojenkins.JobBuild

const sampleConfig = `
url = "http://my-jenkins-instance:8080"
username = "admin"
password = "admin"
## Set response_timeout
response_timeout = "5s"

## Optional SSL Config
# ssl_ca = /path/to/cafile
# ssl_cert = /path/to/certfile
# ssl_key = /path/to/keyfile
## Use SSL but skip chain & host verification
# insecure_skip_verify = false

## Job & build filter
# max_build_age = "1h"
## jenkins can have unlimited layer of sub jobs
## this config will limit the layers of pull, default value 0 means
## unlimited pulling until no more sub jobs
# max_sub_jobs_layer = 0
## in workflow-multibranch-plugin, each branch will be created as a sub job
## this config will limit to call only the lasted branches
## sub jobs fetch in each layer
# empty will use default value 10
# newest_sub_jobs_each_layer = 10
# job_exclude = [ "MyJob", "MyOtherJob" ]

## Node filter
# node_exlude = [ "node1", "node2" ]

## Woker pool for jenkins plugin only
# empty this field will use default value 30
# max_tcp_concurrent_connections = 30
`

// measurement
const (
	measurementNode = "jenkins_node"
	measurementJob  = "jenkins_job"
)

type typedErr struct {
	level     int
	err       error
	reference string
	url       string
}

// const of the error level, default 0 to be the errLevel
const (
	errLevel int = iota
	continueLevel
	infoLevel
)

func wrapErr(e typedErr, err error, ref string) *typedErr {
	return &typedErr{
		level:     e.level,
		err:       err,
		reference: ref,
		url:       e.url,
	}
}

func (e *typedErr) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("error "+e.reference+"[%s]: %v", e.url, e.err)
}

func badFormatErr(te typedErr, field interface{}, want string, fieldName string) *typedErr {
	return &typedErr{
		level:     te.level,
		err:       fmt.Errorf("fieldName: %s, want %s, got %s", fieldName, want, reflect.TypeOf(field).String()),
		reference: errBadFormat,
		url:       te.url,
	}
}

// err references
const (
	errParseConfig         = "parse jenkins config"
	errConnectJenkins      = "connect jenkins instance"
	errInitJenkins         = "init jenkins instance"
	errRetrieveNode        = "retrieving nodes"
	errRetrieveJobs        = "retrieving jobs"
	errReadNodeInfo        = "reading node info"
	errEmptyMonitorData    = "empty monitor data"
	errBadFormat           = "bad format"
	errRetrieveInnerJobs   = "retrieving inner jobs"
	errRetrieveLatestBuild = "retrieving latest build"
)

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
	var err error
	te := typedErr{
		url: j.URL,
	}
	if j.instance == nil {
		if tErr := j.initJenkins(te); tErr != nil {
			return tErr
		}
	}

	nodes, err := j.instance.GetAllNodes()
	if err != nil {
		return wrapErr(te, err, errRetrieveNode)
	}

	jobs, err := j.instance.GetAllJobNames()
	if err != nil {
		return wrapErr(te, err, errRetrieveJobs)
	}

	j.gatherNodesData(nodes, acc)
	j.gatherJobs(jobs, acc)

	return nil
}

func (j *Jenkins) initJenkins(te typedErr) *typedErr {
	// create instance
	tlsCfg, err := j.ClientConfig.TLSConfig()
	if err != nil {
		return wrapErr(te, err, errParseConfig)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
		Timeout: j.ResponseTimeout.Duration,
	}

	j.instance, err = gojenkins.CreateJenkins(client, j.URL, j.Username, j.Password).Init()
	if err != nil {
		return wrapErr(te, err, errConnectJenkins)
	}
	_, err = j.instance.Init()
	if err != nil {
		return wrapErr(te, err, errConnectJenkins)
	}
	// init job filter
	j.jobFilter = make(map[string]bool)
	for _, name := range j.JobExclude {
		j.jobFilter[name] = false
	}

	// init node filter
	j.nodeFilter = make(map[string]bool)
	for _, name := range j.NodeExclude {
		j.nodeFilter[name] = false
	}

	// init tcp pool with default value
	if j.MaxTCPConcurrentConnections <= 0 {
		j.MaxTCPConcurrentConnections = 30
	}

	// default sub jobs can be acquired
	if j.NewestSubJobsEachLayer <= 0 {
		j.NewestSubJobsEachLayer = 10
	}

	return nil
}

func (j *Jenkins) gatherNodeData(node *gojenkins.Node, te typedErr, fields map[string]interface{}, tags map[string]string) *typedErr {
	tags["node_name"] = node.Raw.DisplayName
	var ok bool
	if _, ok = j.nodeFilter[tags["node_name"]]; ok {
		(&te).level = continueLevel
		return &te
	}

	info := node.Raw
	if info.MonitorData.Hudson_NodeMonitors_ArchitectureMonitor == nil {
		return wrapErr(te, fmt.Errorf("check your permission"), errEmptyMonitorData)
	}
	tags["arch"], ok = info.MonitorData.Hudson_NodeMonitors_ArchitectureMonitor.(string)
	if !ok {
		return badFormatErr(te, info.MonitorData.Hudson_NodeMonitors_ArchitectureMonitor, "string", "hudson.node_monitors.ArchitectureMonitor")
	}

	tags["status"] = "online"
	if node.Raw.Offline {
		tags["status"] = "offline"
	}

	fields["response_time"] = info.MonitorData.Hudson_NodeMonitors_ResponseTimeMonitor.Average
	if diskSpaceMonitor := info.MonitorData.Hudson_NodeMonitors_DiskSpaceMonitor; diskSpaceMonitor != nil {
		diskSpaceMonitorRoute := "hudson.node_monitors.DiskSpaceMonitor"
		var diskSpace map[string]interface{}
		if diskSpace, ok = diskSpaceMonitor.(map[string]interface{}); !ok {
			return badFormatErr(te, diskSpaceMonitor, "map[string]interface{}", diskSpaceMonitorRoute)
		}
		if tags["disk_path"], ok = diskSpace["path"].(string); !ok {
			return badFormatErr(te, diskSpace["path"], "string", diskSpaceMonitorRoute+".path")
		}
		if fields["disk_available"], ok = diskSpace["size"].(float64); !ok {
			return badFormatErr(te, diskSpace["size"], "float64", diskSpaceMonitorRoute+".size")
		}
	}

	if tempSpaceMonitor := info.MonitorData.Hudson_NodeMonitors_TemporarySpaceMonitor; tempSpaceMonitor != nil {
		tempSpaceMonitorRoute := "hudson.node_monitors.TemporarySpaceMonitor"
		var tempSpace map[string]interface{}
		if tempSpace, ok = tempSpaceMonitor.(map[string]interface{}); !ok {
			return badFormatErr(te, tempSpaceMonitor, "map[string]interface{}", tempSpaceMonitorRoute)
		}
		if tags["temp_path"], ok = tempSpace["path"].(string); !ok {
			return badFormatErr(te, tempSpace["path"], "string", tempSpaceMonitorRoute+".path")
		}
		if fields["temp_available"], ok = tempSpace["size"].(float64); !ok {
			return badFormatErr(te, tempSpace["size"], "float64", tempSpaceMonitorRoute+".size")
		}
	}

	if swapSpaceMonitor := info.MonitorData.Hudson_NodeMonitors_SwapSpaceMonitor; swapSpaceMonitor != nil {
		swapSpaceMonitorRouter := "hudson.node_monitors.SwapSpaceMonitor"
		var swapSpace map[string]interface{}
		if swapSpace, ok = swapSpaceMonitor.(map[string]interface{}); !ok {
			return badFormatErr(te, swapSpaceMonitor, "map[string]interface{}", swapSpaceMonitorRouter)
		}
		if fields["swap_available"], ok = swapSpace["availableSwapSpace"].(float64); !ok {
			return badFormatErr(te, swapSpace["availableSwapSpace"], "float64", swapSpaceMonitorRouter+".availableSwapSpace")
		}
		if fields["swap_total"], ok = swapSpace["totalSwapSpace"].(float64); !ok {
			return badFormatErr(te, swapSpace["totalSwapSpace"], "float64", swapSpaceMonitorRouter+".totalSwapSpace")
		}
		if fields["memory_available"], ok = swapSpace["availablePhysicalMemory"].(float64); !ok {
			return badFormatErr(te, swapSpace["availablePhysicalMemory"], "float64", swapSpaceMonitorRouter+".availablePhysicalMemory")
		}
		if fields["memory_total"], ok = swapSpace["totalPhysicalMemory"].(float64); !ok {
			return badFormatErr(te, swapSpace["totalPhysicalMemory"], "float64", swapSpaceMonitorRouter+".totalPhysicalMemory")
		}
	}
	return nil
}

func (j *Jenkins) gatherNodesData(nodes []*gojenkins.Node, acc telegraf.Accumulator) {

	tags := map[string]string{}
	fields := make(map[string]interface{})
	baseTe := typedErr{
		url: j.URL + "/computer/api/json",
	}

	// get node data
	for _, node := range nodes {
		te := j.gatherNodeData(node, baseTe, fields, tags)
		if te == nil {
			acc.AddFields(measurementNode, fields, tags)
			continue
		}
		switch te.level {
		case continueLevel:
			continue
		default:
			acc.AddError(te)
		}
	}
}

func (j *Jenkins) gatherJobs(jobNames []gojenkins.InnerJob, acc telegraf.Accumulator) {
	jobsC := make(chan srcJob, j.MaxTCPConcurrentConnections)
	errC := make(chan *typedErr)
	var wg sync.WaitGroup
	for _, job := range jobNames {
		wg.Add(1)
		go func(job gojenkins.InnerJob) {
			jobsC <- srcJob{
				name:    job.Name,
				parents: []string{},
				layer:   0,
			}
		}(job)
	}

	for i := 0; i < j.MaxTCPConcurrentConnections; i++ {
		go j.getJobDetail(jobsC, errC, &wg, acc)
	}

	go func() {
		wg.Wait()
		close(errC)
	}()

	select {
	case te := <-errC:
		if te != nil {
			acc.AddError(te)
		}
	}
}

func (j *Jenkins) getJobDetail(jobsC chan srcJob, errC chan<- *typedErr, wg *sync.WaitGroup, acc telegraf.Accumulator) {
	for sj := range jobsC {
		if j.MaxSubJobsLayer > 0 && sj.layer == j.MaxSubJobsLayer {
			wg.Done()
			continue
		}
		// exclude filter
		if _, ok := j.jobFilter[sj.name]; ok {
			wg.Done()
			continue
		}
		te := &typedErr{
			url: j.URL + "/job/" + strings.Join(sj.combined(), "/job/") + "/api/json",
		}
		jobDetail, err := j.instance.GetJob(sj.name, sj.parents...)
		if err != nil {
			go func(te typedErr, err error) {
				errC <- wrapErr(te, err, errRetrieveInnerJobs)
			}(*te, err)
			return
		}

		for k, innerJob := range jobDetail.Raw.Jobs {
			if k < len(jobDetail.Raw.Jobs)-j.NewestSubJobsEachLayer-1 {
				continue
			}
			wg.Add(1)
			// schedule tcp fetch for inner jobs
			go func(innerJob gojenkins.InnerJob, sj srcJob) {
				jobsC <- srcJob{
					name:    innerJob.Name,
					parents: sj.combined(),
					layer:   sj.layer + 1,
				}
			}(innerJob, sj)
		}

		// collect build info
		number := jobDetail.Raw.LastBuild.Number
		if number < 1 {
			// no build info
			wg.Done()
			continue
		}
		baseURL := "/job/" + strings.Join(sj.combined(), "/job/") + "/" + strconv.Itoa(int(number))
		// jobDetail.GetBuild is not working, doing poll directly
		build := &gojenkins.Build{
			Jenkins: j.instance,
			Depth:   1,
			Base:    baseURL,
			Raw:     new(gojenkins.BuildResponse),
		}
		status, err := build.Poll()
		if err != nil || status != 200 {
			if err == nil && status != 200 {
				err = fmt.Errorf("status code %d", status)
			}
			te.url = j.URL + baseURL + "/api/json"
			go func(te typedErr, err error) {
				errC <- wrapErr(te, err, errRetrieveLatestBuild)
			}(*te, err)
			return
		}

		if build.Raw.Building {
			log.Printf("D! Ignore running build on %s, build %v", sj.name, number)
			wg.Done()
			continue
		}

		// stop if build is too old

		if (j.MaxBuildAge != internal.Duration{Duration: 0}) {
			buildSecAgo := time.Now().Sub(build.GetTimestamp()).Seconds()
			if time.Now().Sub(build.GetTimestamp()).Seconds() > j.MaxBuildAge.Duration.Seconds() {
				log.Printf("D! Job %s build %v too old (%v seconds ago), skipping to next job", sj.name, number, buildSecAgo)
				wg.Done()
				continue
			}
		}

		gatherJobBuild(sj, build, acc)
		wg.Done()

	}
}

type srcJob struct {
	name    string
	parents []string
	layer   int
}

func (sj srcJob) combined() []string {
	return append(sj.parents, sj.name)
}

func (sj srcJob) hierarchyName() string {
	return strings.Join(sj.combined(), "/")
}

func gatherJobBuild(sj srcJob, build *gojenkins.Build, acc telegraf.Accumulator) {
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
