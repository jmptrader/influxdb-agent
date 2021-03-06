package main

import (
	"bytes"
	log "code.google.com/p/log4go"
	"encoding/json"
	"fmt"
	"github.com/errplane/errplane-go"
	"github.com/pmylund/go-cache"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	. "utils"
)

type PluginStateOutput int

type ProcessState interface {
	ExitStatus() int
}

type ProcessStateWrapper struct {
	status *os.ProcessState
}

func (self *ProcessStateWrapper) ExitStatus() int {
	return self.status.Sys().(syscall.WaitStatus).ExitStatus()
}

func (p *PluginStateOutput) String() string {
	switch *p {
	case OK:
		return "ok"
	case WARNING:
		return "warning"
	case CRITICAL:
		return "critical"
	case UNKNOWN:
		return "unknown"
	default:
		panic(fmt.Errorf("WTF unknown state %d", *p))
	}
}

const (
	OK PluginStateOutput = iota
	WARNING
	CRITICAL
	UNKNOWN
)

var (
	DEFAULT_INSTANCE  = &Instance{"default", nil, nil}
	DEFAULT_INSTANCES = []*Instance{&Instance{"", nil, nil}}
	OutputCache       = cache.New(0, 0)
)

type PluginOutput struct {
	state     PluginStateOutput
	msg       string
	points    []*errplane.JsonPoints
	metrics   map[string]float64
	timestamp time.Time
}

// handles running plugins
func monitorPlugins(ep *errplane.Errplane) {
	var previousConfig *AgentConfiguration
	var plugins map[string]*PluginMetadata

	for {
		config, err := GetPluginsToRun()
		if err != nil {
			log.Error("Error while getting configuration from backend. Error: %s", err)
			if previousConfig == nil {
				goto sleep
			}
			config = previousConfig
		}

		log.Debug("Iterating through %d plugins", len(config.Plugins))

		// get the list of plugins that should be turned from the config service
		plugins = getAvailablePlugins()

		for name, instances := range config.Plugins {
			plugin, ok := plugins[name]
			if !ok {
				log.Error("Cannot find plugin '%s'. Error: %s", name, err)
				continue
			}

			if len(instances) == 0 {
				instances = DEFAULT_INSTANCES
			}

			for _, instance := range instances {
				go runPlugin(ep, instance, plugin)
			}
		}

	sleep:
		time.Sleep(AgentConfig.Sleep)
	}
}

func runPlugin(ep *errplane.Errplane, instance *Instance, plugin *PluginMetadata) {
	args := instance.ArgsList
	for name, value := range instance.Args {
		args = append(args, "--"+name, value)
	}
	log.Debug("Running command %s %s", path.Join(plugin.Path, "status"), strings.Join(args, " "))
	cmdPath := path.Join(plugin.Path, "status")
	cmd := exec.Command(cmdPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error("Cannot run plugin %s. Error: %s", cmd, err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Error("Cannot run plugin %s. Error: %s", cmdPath, err)
		return
	}

	ch := make(chan error)
	go killPlugin(cmdPath, cmd, ch)

	output, err := ioutil.ReadAll(stdout)
	if err != nil {
		log.Error("Error while reading output from plugin %s. Error: %s", cmdPath, err)
		ch <- err
		return
	}

	lines := strings.Split(string(output), "\n")

	err = cmd.Wait()
	ch <- err

	if len(lines) > 0 {
		log.Debug("output of plugin %s is %s", cmdPath, lines[0])
		firstLine := lines[0]
		output, err := parsePluginOutput(plugin, &ProcessStateWrapper{cmd.ProcessState}, firstLine)
		if err != nil {
			log.Error("Cannot parse plugin %s output. Output: %s. Error: %s", cmdPath, firstLine, err)
			return
		}

		log.Debug("parsed output is %#v", output)

		// status are printed to plugins.<plugin-name>.status with a value of 1 and dimension status that is either ok, warning, critical or unknown
		// other metrics are written to plugins.<plugin-name>.<metric-name> with the given value
		// all metrics have the host name as a dimension

		dimensions := errplane.Dimensions{
			"host":       AgentConfig.Hostname,
			"status":     output.state.String(),
			"status_msg": output.msg,
		}
		if instance.Name != "" {
			dimensions["instance"] = instance.Name
		}

		report(ep, fmt.Sprintf("plugins.%s.status", plugin.Name), 1.0, time.Now(), dimensions, nil)

		// create a map from metric name to current value
		currentValues := make(map[string]float64)
		log.Debug("Calculating the rates for plugin %s %v", plugin.Name, plugin.CalculateRates)

		// process the errplane output
		if output.points != nil {
			// add the plugins.<plugin-name>.<instance-name> to the metric names
			// if the instance name isn't empty add it to the dimensions
			for _, write := range output.points {
				for _, metric := range plugin.CalculateRates {
					ok, err := regexp.MatchString(metric, write.Name)
					if err != nil {
						log.Error("Invalid regex %s. Error: %s", metric, err)
						continue
					}
					if ok && len(write.Points) > 0 {
						currentValues[write.Name] = write.Points[0].Value
					}
				}

				write.Name = fmt.Sprintf("plugins.%s.%s", plugin.Name, write.Name)
				if instance.Name != "" {
					for _, point := range write.Points {
						point.Dimensions["instance"] = instance.Name
					}
				}
			}

			ep.SendHttp(&errplane.WriteOperation{Writes: output.points})
		}

		// process nagios output
		if output.metrics != nil {
			dimensions := errplane.Dimensions{"host": AgentConfig.Hostname}
			if instance.Name != "" {
				dimensions["instance"] = instance.Name
			}
			for name, value := range output.metrics {
				for _, metric := range plugin.CalculateRates {
					ok, err := regexp.MatchString(metric, name)
					if err != nil {
						log.Error("Invalid regex %s. Error: %s", metric, err)
						continue
					}
					if ok {
						currentValues[name] = value
					}

				}
				report(ep, fmt.Sprintf("plugins.%s.%s", plugin.Name, name), value, time.Now(), dimensions, nil)
			}
		}

		log.Debug("Current values: %v", currentValues)

		// calculate the rate of change
		cacheKey := fmt.Sprintf("%s/%s", plugin.Name, instance.Name)
		_previousOutput, ok := OutputCache.Get(cacheKey)
		defer OutputCache.Set(cacheKey, output, -1)
		log.Debug("Previous output for %s is %v", plugin.Name, _previousOutput)
		if !ok {
			return
		}

		previousOutput := _previousOutput.(*PluginOutput)
		timeDiff := output.timestamp.Sub(previousOutput.timestamp).Seconds()
		for name, value := range previousOutput.metrics {
			currentValue, ok := currentValues[name]
			if !ok {
				continue
			}

			diff := currentValue - value
			diff = diff / timeDiff
			report(ep, fmt.Sprintf("plugins.%s.%s.rate", plugin.Name, name), diff, time.Now(), dimensions, nil)
		}
	}
}

func parsePluginOutput(plugin *PluginMetadata, cmdState ProcessState, firstLine string) (*PluginOutput, error) {
	outputType := plugin.Output
	switch outputType {
	case "nagios":
		return parseNagiosOutput(cmdState, firstLine)
	case "errplane":
		return parseErrplaneOutput(cmdState, firstLine)
	default:
		return nil, fmt.Errorf("Unknown plugin output type '%s', supported types are 'errplane' and 'nagios'", outputType)
	}
}

func parseErrplaneOutput(cmdState ProcessState, firstLine string) (*PluginOutput, error) {
	exitStatus := cmdState.ExitStatus()
	firstLine = strings.TrimSpace(firstLine)
	statusAndMetrics := strings.Split(firstLine, "|")
	status := strings.TrimSpace(statusAndMetrics[0])
	writes := make([]*errplane.JsonPoints, 0)
	metric := strings.TrimSpace(statusAndMetrics[1])

	err := json.Unmarshal([]byte(metric), &writes)
	if err != nil {
		return nil, err
	}

	return &PluginOutput{PluginStateOutput(exitStatus), status, writes, nil, time.Now()}, nil
}

func parseNagiosOutput(cmdState ProcessState, firstLine string) (*PluginOutput, error) {
	firstLine = strings.TrimSpace(firstLine)

	statusAndMetrics := strings.Split(firstLine, "|")
	switch len(statusAndMetrics) {
	case 1, 2: // that's fine, anything else is an error
	default:
		return nil, fmt.Errorf("First line format doesn't match what the agent expects. See the docs for more details")
	}

	exitStatus := cmdState.ExitStatus()
	status := strings.TrimSpace(statusAndMetrics[0])

	if len(statusAndMetrics) == 1 {
		return &PluginOutput{PluginStateOutput(exitStatus), status, nil, nil, time.Now()}, nil
	}

	metricsLine := strings.TrimSpace(statusAndMetrics[1])

	type ParserState int
	const (
		IN_QUOTED_FIELD = iota
		IN_VALUE
		START
	)

	metricName := ""
	value := ""
	token := bytes.NewBufferString("")
	state := START
	metrics := make(map[string]string)

	for i := 0; i < len(metricsLine); i++ {
		switch metricsLine[i] {
		case '\'':
			switch state {
			case IN_QUOTED_FIELD:
				// if we're in a quoted field and we got double single quotes, treat them as a single quote
				// otherwise a '=' should follow and we'll change state to IN_VALUE
				if i+1 < len(metricsLine) && metricsLine[i+1] == '\'' {
					token.WriteByte('\'')
					i++
				}
			case IN_VALUE:
				// We're probably starting a new metric name
				state = IN_QUOTED_FIELD
				value = value + token.String()
				token = bytes.NewBufferString("")
				metrics[metricName] = value
				metricName, value = "", ""
			case START:
				// quote at the beginning of the metrics
				state = IN_QUOTED_FIELD
			}
		case '=':
			switch state {
			case IN_VALUE:
				// we're parsing a value, and suddently started parsing a new metric, e.g. `name=baz foo=bar`
				//																																						e're here ^ but we're parsing the value of the `name`
				metrics[metricName] = value
				fallthrough
			case START:
				metricName = token.String()
				token = bytes.NewBufferString("")
				value = ""
				state = IN_VALUE
			case IN_QUOTED_FIELD:
				// we finished parsing the metric name and started parsing the value
				state = IN_VALUE
				metricName = token.String()
				token = bytes.NewBufferString("")
			}
		case ' ':
			switch state {
			case IN_VALUE:
				value = value + " " + token.String()
			case IN_QUOTED_FIELD:
				metricName = metricName + " " + token.String()
			}
			token = bytes.NewBufferString("")
		default:
			token.WriteByte(metricsLine[i])
		}
	}

	metrics[metricName] = value + token.String()

	metricsMap := make(map[string]float64)

	for key, value := range metrics {
		value = strings.Split(strings.TrimSpace(value), ";")[0]
		if len(value) == 0 {
			continue // empty value, don't bother
		}

		uom := value[len(value)-1]
		switch uom {
		case 's':
			switch value[len(value)-2] {
			case 'u', 'm':
				value = value[0 : len(value)-2]
			default:
				value = value[0 : len(value)-1]
			}
		case 'B':
			switch value[len(value)-2] {
			case 'K', 'M', 'G':
				value = value[0 : len(value)-2]
			default:
				value = value[0 : len(value)-1]
			}
		case '%', 'c':
			value = value[0 : len(value)-1]
		}

		var err error
		metricsMap[key], err = strconv.ParseFloat(value, 64)
		if err != nil {
			delete(metricsMap, key)
			log.Debug("Cannot parse the value of metric %s into a float. Error: %s", key, err)
		}
	}

	return &PluginOutput{PluginStateOutput(exitStatus), status, nil, metricsMap, time.Now()}, nil
}

func killPlugin(cmdPath string, cmd *exec.Cmd, ch chan error) {
	select {
	case err := <-ch:
		if exitErr, ok := err.(*exec.ExitError); ok && !exitErr.Exited() {
			log.Error("plugin %s didn't die gracefully. Killing it.", cmdPath)
			cmd.Process.Kill()
		}
	case <-time.After(AgentConfig.Sleep):
		err := cmd.Process.Kill()
		if err != nil {
			log.Error("Cannot kill plugin %s. Error: %s", cmdPath, err)
		}
		log.Error("Plugin %s killed because it took more than %s to execute", cmdPath, AgentConfig.Sleep)
	}
}
