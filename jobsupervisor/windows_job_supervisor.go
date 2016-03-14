// +build windows

package jobsupervisor

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudfoundry/bosh-utils/state"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/http_server"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	boshalert "github.com/cloudfoundry/bosh-agent/agent/alert"
	boshdirs "github.com/cloudfoundry/bosh-agent/settings/directories"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	boshsys "github.com/cloudfoundry/bosh-utils/system"
)

const (
	serviceDescription = "vcap"

	serviceWrapperExeFileName       = "job-service-wrapper.exe"
	serviceWrapperConfigFileName    = "job-service-wrapper.xml"
	serviceWrapperAppConfigFileName = "job-service-wrapper.exe.config"
	serviceWrapperEventJSONFileName = "job-service-wrapper.wrapper.log.json"
	serviceWrapperAppConfigBody     = `
<configuration>
  <startup>
    <supportedRuntime version="v4.0" />
  </startup>
</configuration>
`

	startJobScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ Start-Service $_.Name }
`
	stopJobScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ Stop-Service $_.Name }
`
	listAllJobsScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ $_.Name }
`
	deleteAllJobsScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ $_.delete() }
`
	getStatusScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ $_.State }
`
	unmonitorJobScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ Set-Service $_.Name -startuptype "Disabled" }
`
	autoStartJobScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'") | ForEach{ Set-Service $_.Name -startuptype "Automatic" }
`

	waitForDeleteAllScript = `
(get-wmiobject win32_service -filter "description='` + serviceDescription + `'").Length
`
)

type serviceLogMode struct {
	Mode string `xml:"mode,attr"`
}

type serviceOnfailure struct {
	Action string `xml:"action,attr"`
	Delay  string `xml:"delay,attr"`
}

type serviceEnv struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type WindowsServiceWrapperConfig struct {
	XMLName     xml.Name         `xml:"service"`
	ID          string           `xml:"id"`
	Name        string           `xml:"name"`
	Description string           `xml:"description"`
	Executable  string           `xml:"executable"`
	Arguments   []string         `xml:"argument"`
	LogPath     string           `xml:"logpath"`
	LogMode     serviceLogMode   `xml:"log"`
	Onfailure   serviceOnfailure `xml:"onfailure"`
	Env         []serviceEnv     `xml:"env,omitempty"`
}

type WindowsProcess struct {
	Name       string            `json:"name"`
	Executable string            `json:"executable"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
}

func (p *WindowsProcess) ServiceWrapperConfig(logPath string) *WindowsServiceWrapperConfig {
	srcv := &WindowsServiceWrapperConfig{
		ID:          p.Name,
		Name:        p.Name,
		Description: serviceDescription,
		Executable:  p.Executable,
		Arguments:   p.Args,
		LogPath:     logPath,
		LogMode: serviceLogMode{
			Mode: "append",
		},
		Onfailure: serviceOnfailure{
			Action: "restart",
			Delay:  "5 sec",
		},
	}
	for k, v := range p.Env {
		srcv.Env = append(srcv.Env, serviceEnv{Name: k, Value: v})
	}

	return srcv
}

type WindowsProcessConfig struct {
	Processes []WindowsProcess `json:"processes"`
}

type windowsJobSupervisor struct {
	cmdRunner             boshsys.CmdRunner
	dirProvider           boshdirs.Provider
	fs                    boshsys.FileSystem
	logger                boshlog.Logger
	logTag                string
	msgCh                 chan *windowsServiceEvent
	monitor               *state.Monitor
	jobFailuresServerPort int
	cancelServer          chan bool
}

func NewWindowsJobSupervisor(
	cmdRunner boshsys.CmdRunner,
	dirProvider boshdirs.Provider,
	fs boshsys.FileSystem,
	logger boshlog.Logger,
	jobFailuresServerPort int,
	cancelChan chan bool,
) JobSupervisor {
	monitor, _ := state.New()
	return &windowsJobSupervisor{
		cmdRunner:             cmdRunner,
		dirProvider:           dirProvider,
		fs:                    fs,
		logger:                logger,
		logTag:                "windowsJobSupervisor",
		msgCh:                 make(chan *windowsServiceEvent, 8),
		monitor:               monitor,
		jobFailuresServerPort: jobFailuresServerPort,
		cancelServer:          cancelChan,
	}
}

func (s *windowsJobSupervisor) Reload() error {
	return nil
}

func (s *windowsJobSupervisor) Start() error {
	s.monitor.Start()

	_, _, _, err := s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", autoStartJobScript)
	if err != nil {
		return bosherr.WrapError(err, "Starting windows job process")
	}
	_, _, _, err = s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", startJobScript)
	if err != nil {
		return bosherr.WrapError(err, "Starting windows job process")
	}

	err = s.fs.RemoveAll(s.stoppedFilePath())
	if err != nil {
		return bosherr.WrapError(err, "Removing stopped file")
	}

	return nil
}

func (s *windowsJobSupervisor) Stop() error {
	s.monitor.Stop()

	_, _, _, err := s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", unmonitorJobScript)
	if err != nil {
		return bosherr.WrapError(err, "Disabling services")
	}
	_, _, _, err = s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", stopJobScript)
	if err != nil {
		return bosherr.WrapError(err, "Stopping services")
	}
	if err := s.fs.WriteFileString(s.stoppedFilePath(), ""); err != nil {
		return bosherr.WrapError(err, "Removing stop services")
	}
	return nil
}

func (s *windowsJobSupervisor) Unmonitor() error {
	_, _, _, err := s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", unmonitorJobScript)
	return err
}

func (s *windowsJobSupervisor) Status() (status string) {
	if s.fs.FileExists(s.stoppedFilePath()) {
		return "stopped"
	}

	stdout, _, _, err := s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", getStatusScript)
	if err != nil {
		return "failing"
	}

	stdout = strings.TrimSpace(stdout)
	if len(stdout) == 0 {
		s.logger.Debug(s.logTag, "No statuses reported for job processes")
		return "running"
	}

	statuses := strings.Split(stdout, "\r\n")
	s.logger.Debug(s.logTag, "Got statuses %#v", statuses)

	for _, status := range statuses {
		if status != "Running" {
			return "failing"
		}
	}

	return "running"
}

/*
func (m monitJobSupervisor) Processes() (processes []Process, err error) {
	processes = []Process{}

	monitStatus, err := m.client.Status()
	if err != nil {
		return processes, bosherr.WrapError(err, "Getting service status")
	}

	for _, service := range monitStatus.ServicesInGroup("vcap") {
		process := Process{
			Name:  service.Name,
			State: service.Status,
			Uptime: UptimeVitals{
				Secs: service.Uptime,
			},
			Memory: MemoryVitals{
				Kb:      service.MemoryKilobytesTotal,
				Percent: service.MemoryPercentTotal,
			},
			CPU: CPUVitals{
				Total: service.CPUPercentTotal,
			},
		}
		processes = append(processes, process)
	}

	return
}
*/

/*
m, err := mgr.Connect()
			if err != nil {
				return 0, err
			}
			defer m.Disconnect()
			s, err := m.OpenService(serviceName)
			if err != nil {
				return 0, err
			}
			defer s.Close()
			st, err := s.Query()
			if err != nil {
				return 0, err
			}
			return st.State, nil
*/

// var windowsService

var windowsSvcStateMap = map[svc.State]string{
	svc.Stopped:         "stopped",
	svc.StartPending:    "starting",
	svc.StopPending:     "stop_pending",
	svc.Running:         "running",
	svc.ContinuePending: "continue_pending",
	svc.PausePending:    "pause_pending",
	svc.Paused:          "paused",
}

// Exported for testing.
func SvcStateString(s svc.State) string {
	return windowsSvcStateMap[s]
}

func (s *windowsJobSupervisor) Processes() ([]Process, error) {
	stdout, _, _, err := s.cmdRunner.RunCommand("powershell", "-noprofile", "-noninteractive", "/C", listAllJobsScript)
	if err != nil {
		return nil, bosherr.WrapError(err, "Listing windows job process")
	}

	m, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()

	var procs []Process
	for _, s := range strings.Split(stdout, "\n") {
		if len(s) == 0 {
			continue
		}
		name := strings.TrimSpace(s)
		service, err := m.OpenService(name)
		if err != nil {
			return nil, bosherr.WrapErrorf(err, "Opening windows service: %q", name)
		}
		defer service.Close()
		st, err := service.Query()
		if err != nil {
			return nil, bosherr.WrapErrorf(err, "Querying windows service: %q", name)
		}
		p := Process{
			Name:  name,
			State: SvcStateString(st.State),
		}
		procs = append(procs, p)
	}

	return procs, nil
}

func (s *windowsJobSupervisor) AddJob(jobName string, jobIndex int, configPath string) error {
	configFileContents, err := s.fs.ReadFile(configPath)
	if err != nil {
		return err
	}

	if len(configFileContents) == 0 {
		s.logger.Debug(s.logTag, "Skipping job configuration for %q, empty monit config file %q", jobName, configPath)
		return nil
	}

	var processConfig WindowsProcessConfig
	err = json.Unmarshal(configFileContents, &processConfig)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	for _, process := range processConfig.Processes {
		logPath := path.Join(s.dirProvider.LogsDir(), jobName, process.Name)
		err := s.fs.MkdirAll(logPath, os.FileMode(0750))
		if err != nil {
			return bosherr.WrapErrorf(err, "Creating log directory for service '%s'", process.Name)
		}

		buf.Reset()
		serviceConfig := process.ServiceWrapperConfig(logPath)
		if err := xml.NewEncoder(&buf).Encode(serviceConfig); err != nil {
			return bosherr.WrapErrorf(err, "Rendering service config template for service '%s'", process.Name)
		}

		s.logger.Debug(s.logTag, "Configuring service wrapper for job %q with configPath %q", jobName, configPath)

		jobDir := filepath.Dir(configPath)

		processDir := filepath.Join(jobDir, process.Name)
		err = s.fs.MkdirAll(processDir, os.FileMode(0750))
		if err != nil {
			return bosherr.WrapErrorf(err, "Creating job directory for service '%s' at '%s'", process.Name, processDir)
		}

		serviceWrapperConfigFile := filepath.Join(processDir, serviceWrapperConfigFileName)
		err = s.fs.WriteFile(serviceWrapperConfigFile, buf.Bytes())
		if err != nil {
			return bosherr.WrapErrorf(err, "Saving service config file for service '%s'", process.Name)
		}

		err = s.fs.WriteFileString(filepath.Join(processDir, serviceWrapperAppConfigFileName), serviceWrapperAppConfigBody)
		if err != nil {
			return bosherr.WrapErrorf(err, "Saving app runtime config file for service '%s'", process.Name)
		}

		serviceWrapperExePath := filepath.Join(s.dirProvider.BoshBinDir(), serviceWrapperExeFileName)
		err = s.fs.CopyFile(serviceWrapperExePath, filepath.Join(processDir, serviceWrapperExeFileName))
		if err != nil {
			return bosherr.WrapErrorf(err, "Copying service wrapper in job directory '%s'", processDir)
		}

		cmdToRun := filepath.Join(processDir, serviceWrapperExeFileName)

		_, _, _, err = s.cmdRunner.RunCommand(cmdToRun, "install")
		if err != nil {
			return bosherr.WrapErrorf(err, "Creating service '%s'", process.Name)
		}
	}

	return nil
}

func (s *windowsJobSupervisor) RemoveAllJobs() error {
	s.monitor.Exit()

	const MaxRetries = 100
	const RetryInterval = time.Millisecond * 5

	_, _, _, err := s.cmdRunner.RunCommand(
		"powershell",
		"-noprofile",
		"-noninteractive",
		"/C",
		deleteAllJobsScript,
	)
	if err != nil {
		return bosherr.WrapErrorf(err, "Removing Windows job supervisor services")
	}

	i := 0
	start := time.Now()
	for {
		stdout, _, _, err := s.cmdRunner.RunCommand(
			"powershell",
			"-noprofile",
			"-noninteractive",
			"/C",
			waitForDeleteAllScript,
		)
		if err != nil {
			return bosherr.WrapErrorf(err, "Checking if Windows job supervisor services exist")
		}
		if strings.TrimSpace(stdout) == "0" {
			break
		}

		i++
		if i == MaxRetries {
			return bosherr.Errorf("removing Windows job supervisor services after %d attempts",
				MaxRetries)
		}
		s.logger.Debug(s.logTag, "Waiting for services to be deleted: attempt (%d) time (%s)",
			i, time.Since(start))

		time.Sleep(RetryInterval)
	}

	s.logger.Debug(s.logTag, "Removed Windows job supervisor services: attempts (%d) time (%s)",
		i, time.Since(start))

	return nil
}

type windowsServiceEvent struct {
	Datetime    string `json:"datetime"`
	Event       string `json:"event"`
	ProcessName string `json:"processName"`
	ExitCode    int    `json:"exitCode"`
}

func (s *windowsJobSupervisor) MonitorJobFailures(handler JobFailureHandler) error {
	hl := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		decoder := json.NewDecoder(r.Body)
		var event windowsServiceEvent
		err := decoder.Decode(&event)
		if err != nil {
			s.logger.Error(s.logTag, "MonitorJobFailures received unknown request: %s", err)
			return
		}
		handler(boshalert.MonitAlert{
			Action:      "Start",
			Date:        event.Datetime,
			Event:       event.Event,
			ID:          event.ProcessName,
			Service:     event.ProcessName,
			Description: fmt.Sprintf("exited with code %d", event.ExitCode),
		})
	})
	server := http_server.New(fmt.Sprintf("localhost:%d", s.jobFailuresServerPort), hl)
	process := ifrit.Invoke(server)
	for {
		select {
		case <-s.cancelServer:
			process.Signal(os.Kill)
		case err := <-process.Wait():
			if err != nil {
				return bosherr.WrapError(err, "Listen for HTTP")
			}
			return nil
		}
	}
}

func (s *windowsJobSupervisor) stoppedFilePath() string {
	return filepath.Join(s.dirProvider.MonitDir(), "stopped")
}
