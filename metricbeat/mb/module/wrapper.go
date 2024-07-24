// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package module

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/management/status"
	"github.com/elastic/beats/v7/metricbeat/mb"
	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/elastic/elastic-agent-libs/monitoring"
	"github.com/elastic/elastic-agent-libs/testing"
)

// Expvar metric names.
const (
	successesKey = "success"
	failuresKey  = "failures"
	eventsKey    = "events"
)

var (
	debugf = logp.MakeDebug("module")

	fetchesLock = sync.Mutex{}
	fetches     = map[string]*stats{}
)

// Wrapper contains the Module and the private data associated with
// running the Module and its MetricSets.
//
// Use NewWrapper or NewWrappers to construct new Wrappers.
type Wrapper struct {
	mb.Module
	metricSets []*metricSetWrapper // List of pointers to its associated MetricSets.

	// Options
	maxStartDelay  time.Duration
	eventModifiers []mb.EventModifier
}

// metricSetWrapper contains the MetricSet and the private data associated with
// running the MetricSet. It contains a pointer to the parent Module.
type metricSetWrapper struct {
	mb.MetricSet
	module *Wrapper // Parent Module.
	stats  *stats   // stats for this MetricSet.

	periodic bool // Set to true if this metricset is a periodic fetcher
}

// stats bundles common metricset stats.
type stats struct {
	key      string          // full stats key
	ref      uint32          // number of modules/metricsets reusing stats instance
	success  *monitoring.Int // Total success events.
	failures *monitoring.Int // Total error events.
	events   *monitoring.Int // Total events published.
}

// NewWrapper creates a new module and its associated metricsets based on the given configuration.
func NewWrapper(config *conf.C, r *mb.Register, options ...Option) (*Wrapper, error) {
	module, metricSets, err := mb.NewModule(config, r)
	if err != nil {
		return nil, err
	}
	return createWrapper(module, metricSets, options...)
}

// NewWrapperForMetricSet creates a wrapper for the selected module and metricset.
func NewWrapperForMetricSet(module mb.Module, metricSet mb.MetricSet, options ...Option) (*Wrapper, error) {
	return createWrapper(module, []mb.MetricSet{metricSet}, options...)
}

func createWrapper(module mb.Module, metricSets []mb.MetricSet, options ...Option) (*Wrapper, error) {
	wrapper := &Wrapper{
		Module:     module,
		metricSets: make([]*metricSetWrapper, len(metricSets)),
	}

	for _, applyOption := range options {
		applyOption(wrapper)
	}

	for i, metricSet := range metricSets {
		wrapper.metricSets[i] = &metricSetWrapper{
			MetricSet: metricSet,
			module:    wrapper,
			stats:     getMetricSetStats(wrapper.Name(), metricSet.Name()),
		}
	}
	return wrapper, nil
}

// Wrapper methods

// Start starts the Module's MetricSet workers which are responsible for
// fetching metrics. The workers will continue to periodically fetch until the
// done channel is closed. When the done channel is closed all MetricSet workers
// will stop and the returned output channel will be closed.
//
// The returned channel is buffered with a length one one. It must drained to
// prevent blocking the operation of the MetricSets.
//
// Start should be called only once in the life of a Wrapper.
func (mw *Wrapper) Start(done <-chan struct{}) <-chan beat.Event {
	debugf("Starting %s", mw)

	out := make(chan beat.Event, 1)

	// Start one worker per MetricSet + host combination.
	var wg sync.WaitGroup
	wg.Add(len(mw.metricSets))
	for _, msw := range mw.metricSets {
		go func(msw *metricSetWrapper) {
			metricsPath := msw.ID()
			registry := monitoring.GetNamespace("dataset").GetRegistry()

			defer registry.Remove(metricsPath)
			defer releaseStats(msw.stats)
			defer wg.Done()
			defer msw.close()

			registry.Add(metricsPath, msw.Metrics(), monitoring.Full)
			monitoring.NewString(msw.Metrics(), "starttime").Set(common.Time(time.Now()).String())

			msw.module.UpdateStatus(status.Starting, fmt.Sprintf("%s/%s is starting", msw.module.Name(), msw.Name()))
			msw.run(done, out)
		}(msw)
	}

	// Close the output channel when all writers to the channel have stopped.
	go func() {
		wg.Wait()
		close(out)
		debugf("Stopped %s", mw)
	}()

	return out
}

// String returns a string representation of Wrapper.
func (mw *Wrapper) String() string {
	return fmt.Sprintf("Wrapper[name=%s, len(metricSetWrappers)=%d]",
		mw.Name(), len(mw.metricSets))
}

// MetricSets return the list of metricsets of the module
func (mw *Wrapper) MetricSets() []*metricSetWrapper {
	return mw.metricSets
}

// metricSetWrapper methods

func (msw *metricSetWrapper) run(done <-chan struct{}, out chan<- beat.Event) {
	defer logp.Recover(fmt.Sprintf("recovered from panic while fetching "+
		"'%s/%s' for host '%s'", msw.module.Name(), msw.Name(), msw.Host()))

	// Start each metricset randomly over a period of MaxDelayPeriod.
	if msw.module.maxStartDelay > 0 {
		delay := time.Duration(rand.Int63n(int64(msw.module.maxStartDelay)))
		debugf("%v/%v will start after %v", msw.module.Name(), msw.Name(), delay)
		select {
		case <-done:
			return
		case <-time.After(delay):
		}
	}

	debugf("Starting %s", msw)
	defer debugf("Stopped %s", msw)

	// Events and errors are reported through this.
	reporter := &eventReporter{
		msw:  msw,
		out:  out,
		done: done,
	}

	switch ms := msw.MetricSet.(type) {
	case mb.PushMetricSet: //nolint:staticcheck // PushMetricSet is deprecated but not removed
		ms.Run(reporter.V1())
	case mb.PushMetricSetV2:
		ms.Run(reporter.V2())
	case mb.PushMetricSetV2WithContext:
		ms.Run(&channelContext{done}, reporter.V2())
	case mb.ReportingMetricSet, mb.ReportingMetricSetV2, mb.ReportingMetricSetV2Error, mb.ReportingMetricSetV2WithContext: //nolint:staticcheck // ReportingMetricSet is deprecated but not removed
		msw.startPeriodicFetching(&channelContext{done}, reporter)
	default:
		// Earlier startup stages prevent this from happening.
		logp.Err("MetricSet '%s/%s' does not implement an event producing interface",
			msw.Module().Name(), msw.Name())
	}
}

// startPeriodicFetching performs an immediate fetch for the MetricSet then it
// begins a continuous timer scheduled loop to fetch data. To stop the loop the
// done channel should be closed.
func (msw *metricSetWrapper) startPeriodicFetching(ctx context.Context, reporter reporter) {
	// Indicate that it has been started as periodic fetcher
	msw.periodic = true

	// Fetch immediately.
	msw.fetch(ctx, reporter)

	// Start timer for future fetches.
	t := time.NewTicker(msw.Module().Config().Period)
	defer t.Stop()
	for {
		select {
		case <-reporter.V2().Done():
			return
		case <-t.C:
			msw.fetch(ctx, reporter)
		}
	}
}

// fetch invokes the appropriate Fetch method for the MetricSet and publishes
// the result using the publisher client. This method will recover from panics
// and log a stack track if one occurs.
func (msw *metricSetWrapper) fetch(ctx context.Context, reporter reporter) {
	switch fetcher := msw.MetricSet.(type) {
	case mb.ReportingMetricSet: //nolint:staticcheck // ReportingMetricSet is deprecated but not removed
		reporter.StartFetchTimer()
		fetcher.Fetch(reporter.V1())
	case mb.ReportingMetricSetV2:
		reporter.StartFetchTimer()
		fetcher.Fetch(reporter.V2())
	case mb.ReportingMetricSetV2Error:
		reporter.StartFetchTimer()
		err := fetcher.Fetch(reporter.V2())
		if err != nil {
			reporter.V2().Error(err)
			msw.module.UpdateStatus(status.Degraded, fmt.Sprintf("Error fetching data for metricset %s.%s: %s", msw.module.Name(), msw.MetricSet.Name(), err))
			logp.Err("Error fetching data for metricset %s.%s: %s", msw.module.Name(), msw.Name(), err)
		} else {
			msw.module.UpdateStatus(status.Running, "")
		}
	case mb.ReportingMetricSetV2WithContext:
		reporter.StartFetchTimer()
		err := fetcher.Fetch(ctx, reporter.V2())
		if err != nil {
			reporter.V2().Error(err)
			msw.module.UpdateStatus(status.Degraded, fmt.Sprintf("Error fetching data for metricset %s.%s: %s", msw.module.Name(), msw.MetricSet.Name(), err))
			logp.Err("Error fetching data for metricset %s.%s: %s", msw.module.Name(), msw.Name(), err)
		} else {
			msw.module.UpdateStatus(status.Running, "")
		}
	default:
		panic(fmt.Sprintf("unexpected fetcher type for %v", msw))
	}
}

// close closes the underlying MetricSet if it implements the mb.Closer
// interface.
func (msw *metricSetWrapper) close() error {
	if closer, ok := msw.MetricSet.(mb.Closer); ok {
		return closer.Close()
	}
	return nil
}

// String returns a string representation of metricSetWrapper.
func (msw *metricSetWrapper) String() string {
	return fmt.Sprintf("metricSetWrapper[module=%s, name=%s, host=%s]",
		msw.module.Name(), msw.Name(), msw.Host())
}

func (msw *metricSetWrapper) Test(d testing.Driver) {
	d.Run(msw.Name(), func(d testing.Driver) {
		events := make(chan beat.Event, 1)
		done := receiveOneEvent(d, events, msw.module.maxStartDelay+5*time.Second)
		msw.run(done, events)
	})
}

type reporter interface {
	StartFetchTimer()
	V1() mb.PushReporter //nolint:staticcheck // PushReporter is deprecated but not removed
	V2() mb.PushReporterV2
}

// eventReporter implements the Reporter interface which is a callback interface
// used by MetricSet implementations to report an event(s), an error, or an error
// with some additional metadata.
type eventReporter struct {
	msw   *metricSetWrapper
	done  <-chan struct{}
	out   chan<- beat.Event
	start time.Time // Start time of the current fetch (or zero for push sources).
}

// startFetchTimer demarcates the start of a new fetch. The elapsed time of a
// fetch is computed based on the time of this call.
func (r *eventReporter) StartFetchTimer() { r.start = time.Now() }
func (r *eventReporter) V1() mb.PushReporter { //nolint:staticcheck // PushReporter is deprecated but not removed
	return reporterV1{v2: r.V2(), module: r.msw.module.Name()}
}
func (r *eventReporter) V2() mb.PushReporterV2 { return reporterV2{r} }

// channelContext implements context.Context by wrapping a channel
type channelContext struct {
	done <-chan struct{}
}

func (r *channelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (r *channelContext) Done() <-chan struct{}       { return r.done }
func (r *channelContext) Err() error {
	select {
	case <-r.done:
		return context.Canceled
	default:
		return nil
	}
}
func (r *channelContext) Value(key interface{}) interface{} { return nil }

// reporterV1 wraps V2 to provide a v1 interface.
type reporterV1 struct {
	v2     mb.PushReporterV2
	module string
}

func (r reporterV1) Done() <-chan struct{}     { return r.v2.Done() }
func (r reporterV1) Event(event mapstr.M) bool { return r.ErrorWith(nil, event) }
func (r reporterV1) Error(err error) bool      { return r.ErrorWith(err, nil) }
func (r reporterV1) ErrorWith(err error, meta mapstr.M) bool {
	// Skip nil events without error
	if err == nil && meta == nil {
		return true
	}
	return r.v2.Event(mb.TransformMapStrToEvent(r.module, meta, err))
}

type reporterV2 struct {
	*eventReporter
}

func (r reporterV2) Done() <-chan struct{} { return r.done }
func (r reporterV2) Error(err error) bool  { return r.Event(mb.Event{Error: err}) }
func (r reporterV2) Event(event mb.Event) bool {
	if event.Took == 0 && !r.start.IsZero() {
		event.Took = time.Since(r.start)
	}
	if r.msw.periodic {
		event.Period = r.msw.Module().Config().Period
	}

	if event.Timestamp.IsZero() {
		if !r.start.IsZero() {
			event.Timestamp = r.start
		} else {
			event.Timestamp = time.Now().UTC()
		}
	}

	if event.Host == "" {
		event.Host = r.msw.HostData().SanitizedURI
	}

	if event.Error == nil {
		r.msw.stats.success.Add(1)
	} else {
		r.msw.stats.failures.Add(1)
	}

	if event.Namespace == "" {
		event.Namespace = r.msw.Registration().Namespace
	}
	beatEvent := event.BeatEvent(r.msw.module.Name(), r.msw.MetricSet.Name(), r.msw.module.eventModifiers...)
	if !writeEvent(r.done, r.out, beatEvent) {
		return false
	}
	r.msw.stats.events.Add(1)

	return true
}

// other utility functions

func writeEvent(done <-chan struct{}, out chan<- beat.Event, event beat.Event) bool {
	select {
	case <-done:
		return false
	case out <- event:
		return true
	}
}

func getMetricSetStats(module, name string) *stats {
	key := fmt.Sprintf("metricbeat.%s.%s", module, name)

	fetchesLock.Lock()
	defer fetchesLock.Unlock()

	if s := fetches[key]; s != nil {
		s.ref++
		return s
	}

	reg := monitoring.Default.NewRegistry(key)
	s := &stats{
		key:      key,
		ref:      1,
		success:  monitoring.NewInt(reg, successesKey),
		failures: monitoring.NewInt(reg, failuresKey),
		events:   monitoring.NewInt(reg, eventsKey),
	}

	fetches[key] = s
	return s
}

func releaseStats(s *stats) {
	fetchesLock.Lock()
	defer fetchesLock.Unlock()

	s.ref--
	if s.ref > 0 {
		return
	}

	delete(fetches, s.key)
	monitoring.Default.Remove(s.key)
}
