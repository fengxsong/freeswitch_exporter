package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/html/charset"
)

// Collector implements prometheus.Collector (see below).
// it also contains the config of the exporter.
type Collector struct {
	Timeout  time.Duration
	Password string
	disables map[string]struct{}

	conn  net.Conn
	input *bufio.Reader
	url   *url.URL
	mutex sync.Mutex

	logger log.Logger

	probeSuccessGauge  prometheus.Gauge
	probeDurationGauge prometheus.Gauge
}

// Metric represents a prometheus metric. It is either fetched from an api command,
// or from "status" parsing (thus the RegexIndex)
type Metric struct {
	Name       string
	Help       string
	Type       prometheus.ValueType
	Command    string
	RegexIndex int
}

const (
	namespace = "freeswitch"
)

type Gateways struct {
	XMLName xml.Name  `xml:"gateways"`
	Gateway []Gateway `xml:"gateway"`
}

type Gateway struct {
	Name           string  `xml:"name"`
	Profile        string  `xml:"profile"`
	Scheme         string  `xml:"scheme"`
	Realm          string  `xml:"realm"`
	UserName       string  `xml:"username"`
	Password       string  `xml:"password"`
	From           string  `xml:"from"`
	Contact        string  `xml:"contact"`
	Exten          string  `xml:"exten"`
	To             string  `xml:"to"`
	Proxy          string  `xml:"proxy"`
	Context        string  `xml:"context"`
	Expires        int     `xml:"expires"`
	FReq           int     `xml:"freq"`
	Ping           int     `xml:"ping"`
	PingFreq       int     `xml:"pingfreq"`
	PingMin        int     `xml:"pingmin"`
	PingCount      int     `xml:"pingcount"`
	PingMax        int     `xml:"pingmax"`
	PingTime       float64 `xml:"pingtime"`
	Pinging        int     `xml:"pinging"`
	State          string  `xml:"state"`
	Status         string  `xml:"status"`
	UptimeUsec     string  `xml:"uptime-usec"`
	CallsIn        int     `xml:"calls-in"`
	CallsOut       int     `xml:"calls-out"`
	FailedCallsIn  int     `xml:"failed-calls-in"`
	FailedCallsOut int     `xml:"failed-calls-out"`
}

type Registrations struct {
	XMLName  xml.Name `xml:"result"`
	Text     string   `xml:",chardata"`
	RowCount string   `xml:"row_count,attr"`
	Row      []struct {
		Text  string `xml:",chardata"`
		RowID string `xml:"row_id,attr"`

		Type struct {
			Text string `xml:",chardata"`
		} `xml:"type"`

		RegUser struct {
			Text string `xml:",chardata"`
		} `xml:"reg_user"`
		Realm struct {
			Text string `xml:",chardata"`
		} `xml:"realm"`
		Token struct {
			Text string `xml:",chardata"`
		} `xml:"token"`
		Url struct {
			Text string `xml:",chardata"`
		} `xml:"url"`
		Expires struct {
			Text string `xml:",chardata"`
		} `xml:"expires"`
		NetworkIp struct {
			Text string `xml:",chardata"`
		} `xml:"network_ip"`
		NetworkPort struct {
			Text string `xml:",chardata"`
		} `xml:"network_port"`
		NetworkProto struct {
			Text string `xml:",chardata"`
		} `xml:"network_proto"`
		Hostname struct {
			Text string `xml:",chardata"`
		} `xml:"hostname"`
	} `xml:"row"`
}

type Configuration struct {
	XMLName     xml.Name `xml:"configuration"`
	Text        string   `xml:",chardata"`
	Name        string   `xml:"name,attr"`
	Description string   `xml:"description,attr"`
	Modules     struct {
		Text string `xml:",chardata"`
		Load []struct {
			Text   string `xml:",chardata"`
			Module string `xml:"module,attr"`
		} `xml:"load"`
	} `xml:"modules"`
}

type Result struct {
	XMLName  xml.Name `xml:"result"`
	Text     string   `xml:",chardata"`
	RowCount string   `xml:"row_count,attr"`
	Row      []struct {
		Text  string `xml:",chardata"`
		RowID string `xml:"row_id,attr"`
		Type  struct {
			Text string `xml:",chardata"`
		} `xml:"type"`
		Name struct {
			Text string `xml:",chardata"`
		} `xml:"name"`
		Ikey struct {
			Text string `xml:",chardata"`
		} `xml:"ikey"`
	} `xml:"row"`
}

type Verto struct {
	XMLName xml.Name `xml:"profiles"`
	Text    string   `xml:",chardata"`
	Profile []struct {
		Text string `xml:",chardata"`
		Name struct {
			Text string `xml:",chardata"`
		} `xml:"name"`
		Type struct {
			Text string `xml:",chardata"`
		} `xml:"type"`
		Data struct {
			Text string `xml:",chardata"`
		} `xml:"data"`
		State struct {
			Text string `xml:",chardata"`
		} `xml:"state"`
	} `xml:"profile"`
}

var (
	metricList = []Metric{
		{Name: "current_calls", Type: prometheus.GaugeValue, Help: "Number of calls active", Command: "api show calls count as json"},
		{Name: "detailed_bridged_calls", Type: prometheus.GaugeValue, Help: "Number of detailed_bridged_calls active", Command: "api show detailed_bridged_calls as json"},
		{Name: "detailed_calls", Type: prometheus.GaugeValue, Help: "Number of detailed_calls active", Command: "api show detailed_calls as json"},
		{Name: "bridged_calls", Type: prometheus.GaugeValue, Help: "Number of bridged_calls active", Command: "api show bridged_calls as json"},
		{Name: "registrations", Type: prometheus.GaugeValue, Help: "Number of registrations active", Command: "api show registrations as json"},
		{Name: "current_channels", Type: prometheus.GaugeValue, Help: "Number of channels active", Command: "api show channels count as json"},
		{Name: "uptime_seconds", Type: prometheus.GaugeValue, Help: "Uptime in seconds", Command: "api uptime s"},
		{Name: "time_synced", Type: prometheus.GaugeValue, Help: "Is FreeSWITCH time in sync with exporter host time", Command: "api strepoch"},
		{Name: "sessions_total", Type: prometheus.CounterValue, Help: "Number of sessions since startup", RegexIndex: 1},
		{Name: "current_sessions", Type: prometheus.GaugeValue, Help: "Number of sessions active", RegexIndex: 2},
		{Name: "current_sessions_peak", Type: prometheus.GaugeValue, Help: "Peak sessions since startup", RegexIndex: 3},
		{Name: "current_sessions_peak_last_5min", Type: prometheus.GaugeValue, Help: "Peak sessions for the last 5 minutes", RegexIndex: 4},
		{Name: "current_sps", Type: prometheus.GaugeValue, Help: "Number of sessions per second", RegexIndex: 5},
		{Name: "current_sps_peak", Type: prometheus.GaugeValue, Help: "Peak sessions per second since startup", RegexIndex: 7},
		{Name: "current_sps_peak_last_5min", Type: prometheus.GaugeValue, Help: "Peak sessions per second for the last 5 minutes", RegexIndex: 8},
		{Name: "max_sps", Type: prometheus.GaugeValue, Help: "Max sessions per second allowed", RegexIndex: 6},
		{Name: "max_sessions", Type: prometheus.GaugeValue, Help: "Max sessions allowed", RegexIndex: 9},
		{Name: "current_idle_cpu", Type: prometheus.GaugeValue, Help: "CPU idle", RegexIndex: 11},
		{Name: "min_idle_cpu", Type: prometheus.GaugeValue, Help: "Minimum CPU idle", RegexIndex: 10},
	}
	statusRegex = regexp.MustCompile(`(\d+) session\(s\) since startup\s+(\d+) session\(s\) - peak (\d+), last 5min (\d+)\s+(\d+) session\(s\) per Sec out of max (\d+), peak (\d+), last 5min (\d+)\s+(\d+) session\(s\) max\s+min idle cpu (\d+\.\d+)\/(\d+\.\d+)`)
)

// NewCollector processes uri, timeout and methods and returns a new Collector.
func NewCollector(uri string, timeout time.Duration, password string, logger log.Logger, disables ...string) (*Collector, error) {
	var url *url.URL
	var err error

	if url, err = url.Parse(uri); err != nil {
		return nil, fmt.Errorf("cannot parse URI: %w", err)
	}

	tmp := make(map[string]struct{})
	for i := range disables {
		tmp[disables[i]] = struct{}{}
	}

	c := &Collector{
		Timeout:  timeout,
		Password: password,
		disables: tmp,
		url:      url,
		logger:   logger,
		probeSuccessGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "probe_success",
			Help: "Displays whether or not the probe was a success",
		}),
		probeDurationGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "probe_duration_seconds",
			Help: "Returns how long the probe took to complete in seconds",
		}),
	}

	return c, nil
}

type collector struct {
	name   string
	ignore bool // ignore and log command not found error
	fn     func(*Collector, chan<- prometheus.Metric) error
}

var collectors = []collector{
	{"builtin", false, scapeMetrics},
	{"status", false, scrapeStatus},
	{"sofiastatus", false, sofiaStatusMetrics},
	{"memory", false, memoryMetrics},
	{"loadmodule", false, loadModuleMetrics},
	{"endpoint", false, endpointMetrics},
	{"codec", false, codecMetrics},
	{"registrations", false, registrationsMetrics},
	{"verto", true, vertoMetrics},
	{"rtp", false, variableRtpAudioMetrics},
}

func namesOfCollectors() []string {
	var ret []string
	for i := range collectors {
		ret = append(ret, collectors[i].name)
	}
	return ret
}

// scrape will connect to the freeswitch instance and push metrics to the Prometheus channel.
func (c *Collector) scrape(ch chan<- prometheus.Metric) error {
	address := c.url.Host

	if c.url.Scheme == "unix" {
		address = c.url.Path
	}

	var err error

	c.conn, err = net.DialTimeout(c.url.Scheme, address, c.Timeout)
	if err != nil {
		return err
	}
	c.conn.SetDeadline(time.Now().Add(c.Timeout))
	defer c.conn.Close()

	c.input = bufio.NewReader(c.conn)

	if err = c.fsAuth(); err != nil {
		return err
	}

	for i := range collectors {
		if _, ok := c.disables[collectors[i].name]; ok {
			continue
		}
		if err := collectors[i].fn(c, ch); err != nil {
			if !collectors[i].ignore || !c.ignoreAndLogCommandNotFoundError(err) {
				return err
			}
		}
	}

	return nil
}

func (c *Collector) ignoreAndLogCommandNotFoundError(err error) bool {
	if strings.Contains(err.Error(), "Command not found") {
		level.Warn(c.logger).Log("err", err)
		return true
	}
	return false
}

func variableRtpAudioMetrics(_ *Collector, _ chan<- prometheus.Metric) error {
	return nil
}

func scapeMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	for _, metricDef := range metricList {
		if len(metricDef.Command) == 0 {
			// this metric will be fetched by scapeStatus
			continue
		}

		value, err := c.fetchMetric(&metricDef)
		if err != nil {
			return err
		}

		metric, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_"+metricDef.Name, metricDef.Help, nil, nil),
			metricDef.Type,
			value,
		)
		if err != nil {
			return err
		}

		ch <- metric
	}

	return nil
}

func loadModuleMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api xml_locate configuration configuration name modules.conf")
	if err != nil {
		return err
	}
	cfgs := Configuration{}

	decode := xml.NewDecoder(bytes.NewReader(response))
	decode.CharsetReader = charset.NewReaderLabel
	err = decode.Decode(&cfgs)
	if err != nil {
		return fmt.Errorf("loadModuleMetrics error: %s, response: %s", err, string(response))
	}
	level.Debug(c.logger).Log("response", fmt.Sprintf("%#v", cfgs))

	fsLoadModules := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "freeswitch_load_module",
			Help: "freeswitch load module status",
		},
		[]string{"module"},
	)

	for _, m := range cfgs.Modules.Load {
		status, err := c.fsCommand("api module_exists " + m.Module)
		if err != nil {
			return err
		}
		load_module := 0

		if string(status) == "true" {
			load_module = 1
		}
		level.Debug(c.logger).Log("module", m.Module, "loadstatus", string(status))
		fsLoadModules.WithLabelValues(m.Module).Set(float64(load_module))
	}
	fsLoadModules.MetricVec.Collect(ch)
	return nil
}

func sofiaStatusMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api sofia xmlstatus gateway")
	if err != nil {
		return err
	}

	gw := Gateways{}
	decode := xml.NewDecoder(bytes.NewReader(response))
	decode.CharsetReader = charset.NewReaderLabel
	err = decode.Decode(&gw)
	if err != nil {
		return fmt.Errorf("sofiaStatusMetrics error: %s, response: %s", err, string(response))
	}
	level.Debug(c.logger).Log("response", fmt.Sprintf("%#v", gw))

	for _, gateway := range gw.Gateway {
		status := 0
		if gateway.Status == "UP" {
			status = 1
		}
		level.Debug(c.logger).Log("sofia", gateway.Name, "status", status)
		fs_status, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_status", "freeswitch gateways status", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile, "context": gateway.Context, "scheme": gateway.Scheme, "status": gateway.Status}),
			prometheus.GaugeValue,
			float64(status),
		)
		if err != nil {
			return err
		}

		ch <- fs_status

		call_in, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_call_in", "freeswitch gateway call-in", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.CallsIn),
		)
		if err != nil {
			return err
		}

		ch <- call_in

		call_out, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_call_out", "freeswitch gateway call-out", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.CallsOut),
		)
		if err != nil {
			return err
		}

		ch <- call_out

		failed_call_in, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_failed_call_in", "freeswitch gateway failed-call-in", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.FailedCallsIn),
		)
		if err != nil {
			return err
		}

		ch <- failed_call_in

		failed_call_out, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_failed_call_out", "freeswitch gateway failed-call-out", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.FailedCallsOut),
		)
		if err != nil {
			return err
		}

		ch <- failed_call_out

		ping, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_ping", "freeswitch gateway ping", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.Ping),
		)
		if err != nil {
			return err
		}

		ch <- ping

		pingfreq, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_pingfreq", "freeswitch gateway pingfreq", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.PingFreq),
		)
		if err != nil {
			return err
		}

		ch <- pingfreq

		pingmin, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_pingmin", "freeswitch gateway pingmin", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.PingMin),
		)
		if err != nil {
			return err
		}

		ch <- pingmin

		pingmax, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_pingmax", "freeswitch gateway pingmax", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.PingMax),
		)
		if err != nil {
			return err
		}

		ch <- pingmax

		pingcount, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_pingcount", "freeswitch gateway pingcount", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.PingCount),
		)
		if err != nil {
			return err
		}

		ch <- pingcount

		pingtime, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_sofia_gateway_pingtime", "freeswitch gateway pingtime", nil, prometheus.Labels{"name": gateway.Name, "proxy": gateway.Proxy, "profile": gateway.Profile}),
			prometheus.GaugeValue,
			float64(gateway.PingTime),
		)
		if err != nil {
			return err
		}

		ch <- pingtime
	}
	return nil
}

func memoryMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api memory")
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(bytes.NewReader(response))

	for scanner.Scan() {
		line := scanner.Text()
		if line == "+OK" {
			break
		}

		matches := regexp.MustCompile(`(.+?) \((.+?)\):\s+(\d+)`).FindStringSubmatch(line)
		if matches == nil {
			level.Debug(c.logger).Log("msg", "cannot find stringsubmatch in parsed memory line", "line", line)
			continue
		}

		help := matches[1]
		field := matches[2]
		value, err := strconv.ParseFloat(matches[3], 64)
		if err != nil {
			return fmt.Errorf("error parsing memory: %w", err)
		}

		metric, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_memory_"+field, help, nil, nil),
			prometheus.GaugeValue,
			value,
		)
		if err != nil {
			return err
		}

		ch <- metric
	}

	return nil
}

func endpointMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api show endpoint as xml")
	if err != nil {
		return err
	}

	rt := Result{}
	decode := xml.NewDecoder(bytes.NewReader(response))
	decode.CharsetReader = charset.NewReaderLabel
	err = decode.Decode(&rt)
	if err != nil {
		return fmt.Errorf("endpointMetrics error: %s, response: %s", err, string(response))
	}
	level.Debug(c.logger).Log("response", fmt.Sprintf("%#v", rt))

	for _, ep := range rt.Row {
		ep_load, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_endpoint_status", "freeswitch endpoint status", nil, prometheus.Labels{"type": ep.Type.Text, "name": ep.Name.Text, "ikey": ep.Ikey.Text}),
			prometheus.GaugeValue,
			float64(1),
		)
		if err != nil {
			return err
		}

		ch <- ep_load
	}
	return nil
}

func registrationsMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api show registrations as xml")
	if err != nil {
		return err
	}
	rt := Registrations{}
	decode := xml.NewDecoder(bytes.NewReader(response))
	decode.CharsetReader = charset.NewReaderLabel
	err = decode.Decode(&rt)
	if err != nil {
		return fmt.Errorf("registrationsMetrics error: %s, response: %s", err, string(response))
	}
	level.Debug(c.logger).Log("response", fmt.Sprintf("%#v", rt))

	for _, cc := range rt.Row {
		cc_load, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_registration_details", "freeswitch registration status", nil, prometheus.Labels{"reg_user": cc.RegUser.Text, "hostname": cc.Hostname.Text, "realm": cc.Realm.Text, "token": cc.Token.Text, "url": cc.Url.Text, "expires": cc.Expires.Text, "network_ip": cc.NetworkIp.Text, "network_port": cc.NetworkPort.Text, "network_proto": cc.NetworkProto.Text}),
			prometheus.GaugeValue,
			float64(1),
		)
		if err != nil {
			return err
		}

		ch <- cc_load
	}
	return nil
}

func codecMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api show codec as xml")
	if err != nil {
		return err
	}
	rt := Result{}
	decode := xml.NewDecoder(bytes.NewReader(response))
	decode.CharsetReader = charset.NewReaderLabel
	err = decode.Decode(&rt)
	if err != nil {
		return fmt.Errorf("codecMetrics error: %s, response: %s", err, string(response))
	}
	level.Debug(c.logger).Log("response", fmt.Sprintf("%#v", rt))
	for _, cc := range rt.Row {
		cc_load, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_codec_status", "freeswitch endpoint status", nil, prometheus.Labels{"type": cc.Type.Text, "name": cc.Name.Text, "ikey": cc.Ikey.Text}),
			prometheus.GaugeValue,
			float64(1),
		)
		if err != nil {
			return err
		}

		ch <- cc_load
	}
	return nil
}

func vertoMetrics(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api verto xmlstatus")
	if err != nil {
		return err
	}

	vt := Verto{}
	decode := xml.NewDecoder(bytes.NewReader(response))
	decode.CharsetReader = charset.NewReaderLabel
	err = decode.Decode(&vt)
	if err != nil {
		return fmt.Errorf("vertoMetrics error: %s, response: %s", err, string(response))
	}
	level.Debug(c.logger).Log("response", fmt.Sprintf("%#v", vt))

	for _, cc := range vt.Profile {
		vt_status := 0
		if cc.State.Text == "RUNNING" {
			vt_status = 1
		}
		vt_load, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_verto_status", "freeswitch endpoint status", nil, prometheus.Labels{"name": cc.Name.Text, "type": cc.Type.Text, "data": cc.Data.Text}),
			prometheus.GaugeValue,
			float64(vt_status),
		)
		if err != nil {
			return err
		}

		ch <- vt_load
	}
	return nil
}

func scrapeStatus(c *Collector, ch chan<- prometheus.Metric) error {
	response, err := c.fsCommand("api status")
	if err != nil {
		return err
	}

	matches := statusRegex.FindAllSubmatch(response, -1)
	if len(matches) != 1 {
		return errors.New("error parsing status")
	}

	for _, metricDef := range metricList {
		if len(metricDef.Command) != 0 {
			// this metric will be fetched by fetchMetric
			continue
		}

		if len(matches[0]) < metricDef.RegexIndex {
			return errors.New("error parsing status")
		}

		strValue := string(matches[0][metricDef.RegexIndex])
		value, err := strconv.ParseFloat(strValue, 64)
		if err != nil {
			return fmt.Errorf("error parsing status: %w", err)
		}

		metric, err := prometheus.NewConstMetric(
			prometheus.NewDesc(namespace+"_"+metricDef.Name, metricDef.Help, nil, nil),
			metricDef.Type,
			value,
		)
		if err != nil {
			return err
		}

		ch <- metric
	}

	return nil
}

func (c *Collector) fetchMetric(metricDef *Metric) (float64, error) {
	now := time.Now()
	response, err := c.fsCommand(metricDef.Command)
	if err != nil {
		return 0, err
	}

	switch metricDef.Name {
	case "current_calls", "current_channels", "detailed_bridged_calls", "detailed_calls", "registrations", "bridged_calls":
		r := struct {
			Count float64 `json:"row_count"`
		}{}

		err = json.Unmarshal(response, &r)
		if err != nil {
			return 0, fmt.Errorf("cannot read JSON response for %s: %w", metricDef.Name, err)
		}
		return r.Count, nil
	case "uptime_seconds":
		raw := string(response)
		if raw[len(raw)-1:] == "\n" {
			raw = raw[:len(raw)-1]
		}

		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot read uptime: %w", err)
		}
		return value, nil
	case "time_synced":
		value, err := strconv.ParseInt(string(response), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot read FreeSWITCH time: %w", err)
		}

		// the maximum allowed time deviation between devices is 3 seconds
		if math.Abs(float64(now.Unix()-value)) < 3 {
			return 1, nil
		}

		level.Warn(c.logger).Log("msg", fmt.Sprintf("time not in sync between system (%v) and FreeSWITCH (%v)",
			now.Unix(), value))

		return 0, nil
	}

	return 0, fmt.Errorf("unknown metric: %s", metricDef.Name)
}

func (c *Collector) fsCommand(command string) ([]byte, error) {
	_, err := io.WriteString(c.conn, command+"\n\n")
	if err != nil {
		return nil, fmt.Errorf("cannot write command: %w", err)
	}

	mimeReader := textproto.NewReader(c.input)
	message, err := mimeReader.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("cannot read command response: %w", err)
	}

	value := message.Get("Content-Length")
	if value == "" {
		return nil, errors.New("missing header 'Content-Length'")
	}
	length, err := strconv.Atoi(value)
	if err != nil {
		return nil, err
	}

	body := make([]byte, length)
	_, err = io.ReadFull(c.input, body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func (c *Collector) fsAuth() error {
	mimeReader := textproto.NewReader(c.input)
	message, err := mimeReader.ReadMIMEHeader()

	if err != nil {
		return fmt.Errorf("read auth failed: %w", err)
	}

	if message.Get("Content-Type") != "auth/request" {
		return errors.New("auth failed: unknown content-type")
	}

	_, err = io.WriteString(c.conn, fmt.Sprintf("auth %s\n\n", c.Password))
	if err != nil {
		return fmt.Errorf("write auth failed: %w", err)
	}

	message, err = mimeReader.ReadMIMEHeader()
	if err != nil {
		return fmt.Errorf("read auth failed: %w", err)
	}

	if message.Get("Content-Type") != "command/reply" {
		return errors.New("auth failed: unknown reply")
	}

	if message.Get("Reply-Text") != "+OK accepted" {
		return fmt.Errorf("auth failed: %s", message.Get("Reply-Text"))
	}

	return nil
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	// do nothing, we only need to scrape metrics hen triggered
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	start := time.Now()
	err := c.scrape(ch)
	duration := time.Since(start).Seconds()
	c.probeDurationGauge.Set(duration)

	if err != nil {
		c.probeSuccessGauge.Set(0)
		level.Error(c.logger).Log("duration", duration, "err", err)
		totalScrapes.WithLabelValues(c.url.String(), "failed").Inc()
	} else {
		c.probeSuccessGauge.Set(1)
		totalScrapes.WithLabelValues(c.url.String(), "success").Inc()
		level.Info(c.logger).Log("duration", duration, "msg", "Probe succeeded")
	}
	ch <- c.probeDurationGauge
	ch <- c.probeSuccessGauge
}
