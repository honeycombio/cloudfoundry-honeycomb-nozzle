# cloudfoundry-honeycomb-nozzle

[![OSS Lifecycle](https://img.shields.io/osslifecycle/honeycombio/cloudfoundry-honeycomb-nozzle)](https://github.com/honeycombio/home/blob/main/honeycomb-oss-lifecycle-and-practices.md)

**STATUS: this project is [archived](https://github.com/honeycombio/home/blob/main/honeycomb-oss-lifecycle-and-practices.md).**

Questions? You can chat with us in the **Honeycomb Pollinators** Slack. You can find a direct link to request an invite in [Spread the Love: Appreciating Our Pollinators Community](https://www.honeycomb.io/blog/spread-the-love-appreciating-our-pollinators-community/).

---

A Nozzle for draining logs and metrics from Cloud Foundry Loggregator to Honeycomb. For more information about using Honeycomb, see the [docs](https://honeycomb.io/docs).

## Overview

This nozzle listens to the Loggregator firehose and sends events on to Honeycomb. It splits the events into three datasets in Honeycomb:

* `CF Logs` - all HTTPStartStop and LogMessage (application log) events
* `CF Metrics` - all metrics: ContainerMetric, ValueMetric, and CounterEvent
* `CF Errors` - any errors retrieving content from the firehose

The events sent to Honeycomb have all keys from the envelope as well as keys specific to each event type.  Envelope fields:

* `origin` - the source component for the event
* `eventType` - one of `HttpStartStop`, `LogMessage`, `ContainerMetric`, `CounterEvent`, `ValueMetric`, or `Error`
* `deployment` - eg 'cf'
* `job` - the source job for the event
* `index` - a GUID
* `ip` - source IP for the event
* `tag_*` - if the event is tagged, the tags will be here

Additionally, there are three fields containing GUIDs indicating this nozzle's application ID, instance ID, and version: `hnyNozzleAppID`, `hnyNozzleInstanceID`, and `hnyNozzleVersion`.

Each event type has its fields represented with a prefix:

* all `HttpStartStop` fields begin with `http`
* all `LogMessage` fields begin with `log`

If an application emits flat JSON objects to STDOUT (newline separated), they will wind up in the body of a LogMessage event. This nozzle will parse that JSON and its contents will be represented as fields named `logMessage-<fieldname>`.

For example, if your application emits the string:

    {"Source":"myapp", "UserID":"4223ab", "Age":23}

the corresponding LogMessage event will have the keys and values:

* `logMessage-Source`: "myapp"
* `logMessage-UserID`: "4223ab"
* `logMessage-Age`: 23

## Installation

For Pivotal Cloud Foundry, there will soon be a tile available for this nozzle on PivNet. To build the tile manually for upload to a PCF cluster, run:

    cd tile
    ./build.sh

For testing, you can upload the app directly to your cluster with the following commands:

    cd tile/release/honeycomb/blobs/honeycomb/
    # set up the pcf space for testing
    cf create-org honeycomb-testing   # or use an existing org
    cf create-space dev               # or use an existing space
    cf target -o "pivpart-honeycomb" -s "dev"
    # push the first rev; this will succeed but the app will crash because of missing env vars
    cf push honeycomb-manual
    # set the env vars
    cf set-env honeycomb-manual HONEYCOMB_WRITEKEY "<your-writekey>"
    cf set-env honeycomb-manual HONEYCOMB_SAMPLERATE "1"
    cf set-env honeycomb-manual HONEYCOMB_SENDMETRICS "true"
    cf set-env honeycomb-manual HONEYCOMB_SKIPSSL "true"
    cf set-env honeycomb-manual HONEYCOMB_APIURL "api.<system_domain>"
    cf set-env honeycomb-manual HONEYCOMB_APIUSERNAME "<username you created>"
    cf set-env honeycomb-manual HONEYCOMB_APIPASSWORD "<password>"
    # restart the process to launch with the env configured
    cf restart honeycomb-manual
    cf logs honeycomb-manual  # to watch the process
    # repeat as necessary
    pushd ../../../.. && ./build.sh && popd && cf push honeycomb-manual
    # clean up
    cf stop honeycomb-manual

    cf env # <-- show environment variables
    cf target # <-- show which target you've currently got
    cf target -o system # <-- target the opsmanaged system org
    cf spaces # <-- list spaces
    cf apps # <-- show currently running apps
    cf logs appname # <-- tail logs
    cf logs appname --recent # <-- show slightly older logs
    #pro tip:
    cf logs appname --recent; cf logs appname

When you are satisfied the nozzle is working correctly, upload the tile for production use.  Use either the OpsManager GUI interface to upload the tile or run the following, replacing `0.0.13` with the current version number:

    cd tile
    cat > metadata
    ###  credentials for your PCF cluster in yml format
    pcf import product/honeycomb-0.0.13.pivotal
    pcf pcf install honeycomb 0.0.13
    pcf apply-changes

For a regular Cloud Fonudry installation, build the nozzle as you would any other CFApp and deploy using your regular processes.

The Nozzle expects some configuration variables to be set via the environment. For PCF, this is done when installing the tile.

The configuration parameters are:

* Honeycomb Write Key, available from https://ui.honeycomb.io/account (env var name: `HONEYCOMB_WRITEKEY`)
* the UAA URL, of the form `https://uaa.<system_domain>` (env var name: `HONEYCOMB_UAAURL`)
* a UAA Client username with access to the firehose (env var name: `HONEYCOMB_UAAUSERNAME`)
* the Client secret for the UAA username(env var name: `HONEYCOMB_UAAPASSWORD`)
* the URL for the doppler service, of the form `wss://doppler.<system_domain>:<port>` (env var name: `HONEYCOMB_DOPPLERURL`)

## Operation

The nozzle has a health check listener. It gets the port on which to listen from Cloud Foundry via the PORT environment variable. If you make an HTTP GET request to the health check port you should get the response "I'm healthy".

The nozzle reports statistics on the number of events processed once per minute. These metrics will appear as a normal LogMessage.

It is recommended to always run at least 2 instances of the nozzle to allow for no-downtime upgrades. The number of instances required will depend on the volume of traffic going through Loggregator; a good rule of thumb is to have one nozzle instance for every thousand events per second flowing through Loggregator. You can get a count of the total volume of events from Honeycomb by looking at the LogMessage events emitted with performance information or by summing the COUNT of events across all three datasets.

The Loggregator system will balance events across all available nozzle instances.

## Feature Requests

(pull requests gladly accepted)

* change the health check to return JSON and contain performance details about the nozzle such as counters and throughput metrics
* add support for sampling events before sending them to Honeycomb, with different sampling rates based on event type

## License

This code is licensed under the Apache 2.0 License.

## Additional Resources

* Tile Development docs: https://docs.pivotal.org/tile-dev

