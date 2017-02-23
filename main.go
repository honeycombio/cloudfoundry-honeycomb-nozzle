package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"time"

	logger "github.com/Sirupsen/logrus"
	"github.com/cloudfoundry-incubator/uaago"
	"github.com/cloudfoundry/noaa/consumer"
	"github.com/cloudfoundry/sonde-go/events"
	"github.com/honeycombio/libhoney-go"
	"github.com/honeycombio/urlshaper"
	"github.com/kelseyhightower/envconfig"
	"github.com/maplebed/go-cfenv"
)

const (
	honeycombDefaultDataset = "Default should be overridden for each event type"
	honeycombLogDataset     = "CF Logs"
	honeycombErrorDataset   = "CF Errors"
	honeycombMetricsDataset = "CF Metrics"

	reportingInterval = 60 //seconds
)

// Config will be populated by environment variables, set by PCF:
// HONEYCOMB_WRITEKEY
// HONEYCOMB_SAMPLERATE
// HONEYCOMB_SENDMETRICS
// HONEYCOMB_SKIPSSL
// auth should use
// HONEYCOMB_APIURL
// HONEYCOMB_APIUSERNAME
// HONEYCOMB_APIPASSWORD
// old-skool uaa client left for backwards compatibility but don't use it
// HONEYCOMB_UAAURL
// HONEYCOMB_UAAUSERNAME
// HONEYCOMB_UAAPASSWORD
// HONEYCOMB_DOPPLERURL
type HnyConfig struct {
	WriteKey    string
	SampleRate  uint
	SendMetrics bool
	SkipSSL     bool
	// used if you're connecting using API credentials
	APIURL      string
	APIUsername string
	APIPassword string
	// used if you're connecting using a UUA config
	UAAURL      string
	UAAUsername string
	UAAPassword string
	DopplerURL  string
}

// if we use API auth, we'll get a cache. Otherwise just skip looking up names
var cache *inMemCache

func main() {
	// get honeycomb-specific config variables
	c := HnyConfig{}
	if err := envconfig.Process("honeycomb", &c); err != nil {
		panic(err)
	}

	// get all the standard app environment stuff
	appEnv, _ := cfenv.Current()

	// set up a listener to respond to health check requests
	http.HandleFunc("/", healthCheckHandler)
	go func() {
		http.ListenAndServe(":"+strconv.Itoa(appEnv.Port), nil)
	}()

	// initialize libhoney and prepare to send events to Honeycomb
	libhoney.Init(libhoney.Config{
		WriteKey:   c.WriteKey,
		Dataset:    honeycombDefaultDataset,
		SampleRate: c.SampleRate,
	})
	defer libhoney.Close()
	libhoney.AddField("hnyNozzleAppID", appEnv.AppID)
	libhoney.AddField("hnyNozzleInstanceID", appEnv.ID)
	libhoney.AddField("hnyNozzleVersion", appEnv.Version)
	libhoney.AddDynamicField("hnyNozzleNumGoroutines",
		func() interface{} { return runtime.NumGoroutine() })
	getAlloc := func() interface{} {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		return mem.Alloc
	}
	libhoney.AddDynamicField("hnyNozzleMem", getAlloc)

	// do the PCF auth dance
	trafficControllerURL, token := getTrafficControllerURLAndToken(c)
	consumer := consumer.New(trafficControllerURL, &tls.Config{
		InsecureSkipVerify: c.SkipSSL,
	}, nil)

	// get our event and errors channels for the Firehose
	evs, errors := consumer.Firehose("honeycomb", token)

	ticker := time.NewTicker(time.Duration(reportingInterval) * time.Second)

	// TODO make these counters visible to the health check so the operator can
	// see something incrementing to show that traffic is flowing through this
	// nozzle
	var eventCount, metricEventsSkipped, errCount int
	var prevEvCount, prevMetricEventsSkipped, prevErrCount int
	for {
		hnyEv := libhoney.NewEvent()
		select {
		case ev := <-evs:
			// if we're supposed to skip metrics, short circuit all parsing here
			if !c.SendMetrics {
				if ev.EventType != nil {
					switch *ev.EventType {
					case events.Envelope_ContainerMetric:
						fallthrough
					case events.Envelope_CounterEvent:
						fallthrough
					case events.Envelope_ValueMetric:
						metricEventsSkipped++
						continue
					}
				}
			}
			eventCount++
			if err := translateEvent(ev, hnyEv); err != nil {
				panic(err)
			}
		case ev := <-errors:
			errCount++
			hnyEv.AddField("error", ev.Error())
			hnyEv.Dataset = honeycombErrorDataset
		case <-ticker.C:
			// calculate and report metrics
			curEventCount := eventCount - prevEvCount
			curSkippedMetrics := metricEventsSkipped - prevMetricEventsSkipped
			curErrCount := errCount - prevErrCount
			evsPerSec := float64(curEventCount) / float64(reportingInterval)
			metricsPerSec := float64(curSkippedMetrics) / float64(reportingInterval)
			errsPerSec := float64(curErrCount) / float64(reportingInterval)
			prevEvCount = eventCount
			prevMetricEventsSkipped = metricEventsSkipped
			prevErrCount = errCount
			stats, _ := json.Marshal(map[string]interface{}{
				"TotalEvents":               eventCount,
				"TotalSkippedMetrics":       metricEventsSkipped,
				"TotalErrors":               errCount,
				"ReportingIntervalSec":      reportingInterval,
				"EventsPerInterval":         curEventCount,
				"MetricsPerIntervalSkipped": curSkippedMetrics,
				"ErrsPerInterval":           curErrCount,
				"EventsPerSec":              evsPerSec,
				"MetricsPerSecSkipped":      metricsPerSec,
				"ErrsPerSec":                errsPerSec,
			})
			fmt.Printf(string(stats) + "\n")
		}
		hnyEv.Send()
	}
}

func getTrafficControllerURLAndToken(conf HnyConfig) (string, string) {
	var token, trafficControllerURL string
	if conf.APIURL != "" {
		logger.Printf("Fetching auth token via API: %v\n", conf.APIURL)

		fetcher, err := NewAPIClient(conf.APIURL, conf.APIUsername,
			conf.APIPassword, conf.SkipSSL)
		if err != nil {
			logger.Fatal("Unable to build API client", err)
		}
		cache = &inMemCache{
			gcfClient: fetcher.client,
		}
		token, err = fetcher.FetchAuthToken()
		if err != nil {
			logger.Fatal("Unable to fetch token via API", err)
		}

		trafficControllerURL = fetcher.FetchTrafficControllerURL()
		if trafficControllerURL == "" {
			logger.Fatal("trafficControllerURL from client was blank")
		}
	} else if conf.UAAURL != "" {
		logger.Printf("Fetching auth token via UAA: %v\n", conf.UAAURL)

		uaaClient, err := uaago.NewClient(conf.UAAURL)
		if err != nil {
			panic(err)
		}
		token, err = uaaClient.GetAuthToken(conf.UAAUsername, conf.UAAPassword, true)
		if err != nil {
			panic(err)
		}
		trafficControllerURL = conf.DopplerURL
	} else {
		logger.Fatal(errors.New("One of NOZZLE_API_URL or NOZZLE_UAA_URL are required"))
	}
	return trafficControllerURL, token
}

func translateEvent(cfEv *events.Envelope, hnyEv *libhoney.Event) error {
	// add generic fields
	hnyEv.AddField("origin", cfEv.GetOrigin())
	hnyEv.AddField("eventType", cfEv.GetEventType().String())
	hnyEv.AddField("deployment", cfEv.GetDeployment())
	hnyEv.AddField("job", cfEv.GetJob())
	hnyEv.AddField("index", cfEv.GetIndex())
	hnyEv.AddField("ip", cfEv.GetIp())
	for name, val := range cfEv.Tags {
		hnyEv.AddField("tag_"+name, val)
	}

	// get in to event type specific stuff
	switch *cfEv.EventType {
	case events.Envelope_HttpStartStop:
		hnyEv.Dataset = honeycombLogDataset
		trHttpStartStopEvent(cfEv, hnyEv)
	case events.Envelope_LogMessage:
		hnyEv.Dataset = honeycombLogDataset
		trLogMessage(cfEv, hnyEv)
	case events.Envelope_ContainerMetric:
		hnyEv.Dataset = honeycombMetricsDataset
		trContainerMetric(cfEv, hnyEv)
		hnyEv.AddField("containerMetric", cfEv.GetContainerMetric().String())
	case events.Envelope_CounterEvent:
		hnyEv.Dataset = honeycombMetricsDataset
		trCounterEvent(cfEv, hnyEv)
		hnyEv.AddField("counterEvent", cfEv.GetCounterEvent().String())
	case events.Envelope_ValueMetric:
		hnyEv.Dataset = honeycombMetricsDataset
		trValueMetric(cfEv, hnyEv)
		hnyEv.AddField("valueMetric", cfEv.GetValueMetric().String())
	case events.Envelope_Error:
		hnyEv.Dataset = honeycombErrorDataset
		hnyEv.AddField("errorField", cfEv.GetError().String())
	}

	return nil
}

// trValueMetric unpacks a ValueMetric event to a honeycomb event
func trValueMetric(cfEv *events.Envelope, hnyEv *libhoney.Event) {
	// example:
	// name:"total_tcp_routes" value:0 unit:"gauge"
	vm := cfEv.ValueMetric
	prefix := "valuem"
	if vm.Name != nil {
		hnyEv.AddField(prefix+"Name", vm.GetName())
	}
	if vm.Value != nil {
		hnyEv.AddField(prefix+"Value", vm.GetValue())
	}
	if vm.Unit != nil {
		hnyEv.AddField(prefix+"Unit", vm.GetUnit())
	}
}

// trCounterEvent unpacks a CounterEvent event to a honeycomb event
func trCounterEvent(cfEv *events.Envelope, hnyEv *libhoney.Event) {
	// example:
	// applicationId:"abcd1234-abcd-1234-abcd-12345678abcd" instanceIndex:0 cpuPercentage:0.2489 memoryBytes:356458496 diskBytes:146571264 memoryBytesQuota:536870912 diskBytesQuota:1073741824
	ce := cfEv.CounterEvent
	prefix := "ce"
	if ce.Name != nil {
		hnyEv.AddField(prefix+"Name", ce.GetName())
	}
	if ce.Delta != nil {
		hnyEv.AddField(prefix+"Delta", ce.GetDelta())
	}
	if ce.Total != nil {
		hnyEv.AddField(prefix+"Total", ce.GetTotal())
	}
}

// trContainerMetric unpacks a ContainerMetric event to a honeycomb event
func trContainerMetric(cfEv *events.Envelope, hnyEv *libhoney.Event) {
	// example:
	// applicationId:"abcd1234-abcd-1234-abcd-12345678abcd" instanceIndex:0 cpuPercentage:0.24897312998041832 memoryBytes:356458496 diskBytes:146571264 memoryBytesQuota:536870912 diskBytesQuota:1073741824
	cm := cfEv.ContainerMetric
	prefix := "cm"
	if cm.ApplicationId != nil {
		hnyEv.AddField(prefix+"ApplicationId", cm.GetApplicationId())
	}
	if cm.InstanceIndex != nil {
		hnyEv.AddField(prefix+"InstanceIndex", cm.GetInstanceIndex())
	}
	if cm.CpuPercentage != nil {
		hnyEv.AddField(prefix+"CpuPercentage", cm.GetCpuPercentage())
	}
	if cm.MemoryBytes != nil {
		hnyEv.AddField(prefix+"MemoryBytes", cm.GetMemoryBytes())
	}
	if cm.DiskBytes != nil {
		hnyEv.AddField(prefix+"DiskBytes", cm.GetDiskBytes())
	}
	if cm.MemoryBytesQuota != nil {
		hnyEv.AddField(prefix+"MemoryBytesQuota", cm.GetMemoryBytesQuota())
	}
	if cm.DiskBytesQuota != nil {
		hnyEv.AddField(prefix+"DiskBytesQuota", cm.GetDiskBytesQuota())
	}
}

// trLogMessage unpacks an application log event to a honeycomb event
func trLogMessage(cfEv *events.Envelope, hnyEv *libhoney.Event) {
	message := cfEv.LogMessage
	prefix := "log"
	if message.Message != nil {
		msgContent := message.GetMessage()
		hnyEv.AddField(prefix+"Message", string(msgContent))
		parsedContent := make(map[string]interface{})
		if err := json.Unmarshal(msgContent, &parsedContent); err == nil {
			for k, v := range parsedContent {
				hnyEv.AddField(prefix+"Message-"+k, v)
			}
		}
	}
	if message.MessageType != nil {
		hnyEv.AddField(prefix+"MessageType", message.GetMessageType().String())
	}
	if message.Timestamp != nil {
		hnyEv.AddField(prefix+"Timestamp", time.Unix(0, message.GetTimestamp()))
	}
	if message.AppId != nil {
		appID := message.GetAppId()
		hnyEv.AddField(prefix+"AppId", appID)
		name, _ := cache.getNameFromID(appID)
		if name != "" {
			hnyEv.AddField(prefix+"AppName", name)
		}
	}
	if message.SourceType != nil {
		hnyEv.AddField(prefix+"SourceType", message.GetSourceType())
	}
	if message.SourceInstance != nil {
		hnyEv.AddField(prefix+"SourceInstance", message.GetSourceInstance())
	}
}

// trHttpStartStopEvent unpacks an http start/stop event to a honeycomb event
func trHttpStartStopEvent(cfEv *events.Envelope, hnyEv *libhoney.Event) {
	hss := cfEv.HttpStartStop
	prefix := "http"
	start := time.Unix(0, hss.GetStartTimestamp())
	end := time.Unix(0, hss.GetStopTimestamp())
	dur := float64(end.Sub(start)) / float64(time.Millisecond)
	hnyEv.AddField(prefix+"StartTimestamp", start)
	hnyEv.AddField(prefix+"StopTimestamp", end)
	hnyEv.AddField(prefix+"DurMs", dur)
	if hss.RequestId != nil {
		hnyEv.AddField(prefix+"RequestId", formatUUID(hss.GetRequestId()))
	}
	if hss.PeerType != nil {
		hnyEv.AddField(prefix+"PeerType", hss.GetPeerType().String())
	}
	if hss.Method != nil {
		hnyEv.AddField(prefix+"Method", hss.GetMethod().String())
	}
	if hss.Uri != nil {
		hnyEv.AddField(prefix+"Uri", hss.GetUri())
		parser := &urlshaper.Parser{}
		if result, err := parser.Parse(hss.GetUri()); err == nil {
			hnyEv.AddField(prefix+"UriPath", result.Path)
			if result.Query != "" {
				hnyEv.AddField(prefix+"UriQuery", result.Query)
				hnyEv.AddField(prefix+"UriQueryShape", result.QueryShape)
			}
		}
	}
	if hss.RemoteAddress != nil {
		hnyEv.AddField(prefix+"RemoteAddress", hss.GetRemoteAddress())
	}
	if hss.UserAgent != nil {
		hnyEv.AddField(prefix+"UserAgent", hss.GetUserAgent())
	}
	if hss.StatusCode != nil {
		hnyEv.AddField(prefix+"StatusCode", hss.GetStatusCode())
	}
	if hss.ContentLength != nil {
		hnyEv.AddField(prefix+"ContentLength", hss.GetContentLength())
	}
	if hss.ApplicationId != nil {
		hnyEv.AddField(prefix+"ApplicationId", formatUUID(hss.GetApplicationId()))
	}
	if hss.InstanceIndex != nil {
		hnyEv.AddField(prefix+"InstanceIndex", hss.GetInstanceIndex())
	}
	if hss.InstanceId != nil {
		hnyEv.AddField(prefix+"InstanceId", hss.GetInstanceId())
	}
	for i, forw := range hss.Forwarded {
		index := strconv.Itoa(i)
		hnyEv.AddField(prefix+"Forwarded-"+index, forw)
	}
}

func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "I'm healthy")
}

func formatUUID(uuid *events.UUID) string {
	if uuid == nil {
		return ""
	}
	var uuidBytes [16]byte
	binary.LittleEndian.PutUint64(uuidBytes[:8], uuid.GetLow())
	binary.LittleEndian.PutUint64(uuidBytes[8:], uuid.GetHigh())
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:])
}
