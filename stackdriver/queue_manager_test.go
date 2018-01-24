// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stackdriver

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/config"
	monitoring "google.golang.org/api/monitoring/v3"
)

type TestStorageClient struct {
	receivedSamples map[string][]sample
	expectedSamples map[string][]sample
	wg              sync.WaitGroup
	mtx             sync.Mutex
}

type sample struct {
	Name  string
	Value float64
}

func NewTestStorageClient() *TestStorageClient {
	return &TestStorageClient{
		receivedSamples: map[string][]sample{},
		expectedSamples: map[string][]sample{},
	}
}

func (c *TestStorageClient) expectSamples(samples []sample) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	for _, s := range samples {
		c.expectedSamples[s.Name] = append(c.expectedSamples[s.Name], s)
	}
	c.wg.Add(len(samples))
}

func (c *TestStorageClient) waitForExpectedSamples(t *testing.T) {
	c.wg.Wait()

	c.mtx.Lock()
	defer c.mtx.Unlock()
	for name, expectedSamples := range c.expectedSamples {
		if !reflect.DeepEqual(expectedSamples, c.receivedSamples[name]) {
			t.Fatalf("%s: Expected %v, got %v", name, expectedSamples, c.receivedSamples[name])
		}
	}
}

func (c *TestStorageClient) Store(req *monitoring.CreateTimeSeriesRequest) error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	count := 0
	for _, ts := range req.TimeSeries {
		metricType := ts.Metric.Type
		// Remove the Stackdriver "domain/" prefix which isn't present in the test input.
		name := metricType[len(metricsPrefix)+1:]
		for _, point := range ts.Points {
			count++
			s := sample{
				Name:  name,
				Value: *point.Value.DoubleValue,
			}
			c.receivedSamples[name] = append(c.receivedSamples[name], s)
		}
	}
	c.wg.Add(-count)
	return nil
}

func (c *TestStorageClient) Name() string {
	return "teststorageclient"
}

func TestSampleDelivery(t *testing.T) {
	// Let's create an even number of send batches so we don't run into the
	// batch timeout case.
	n := config.DefaultQueueConfig.Capacity * 2

	samples := make([]sample, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("test_metric_%d", i)
		samples = append(samples, sample{Name: name, Value: float64(i)})
	}

	c := NewTestStorageClient()
	c.expectSamples(samples[:len(samples)/2])

	cfg := config.DefaultQueueConfig
	cfg.MaxShards = 1
	m := NewQueueManager(nil, cfg, nil, c)

	// These should be received by the client.
	for _, s := range samples[:len(samples)/2] {
		m.Append(sampleToMetricFamily(s))
	}
	// These will be dropped because the queue is full.
	for _, s := range samples[len(samples)/2:] {
		m.Append(sampleToMetricFamily(s))
	}
	m.Start()
	defer m.Stop()

	c.waitForExpectedSamples(t)
}

func TestSampleDeliveryOrder(t *testing.T) {
	ts := 10
	n := config.DefaultQueueConfig.MaxSamplesPerSend * ts

	samples := make([]sample, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("test_metric_%d", i%ts)
		samples = append(samples, sample{Name: name, Value: float64(i)})
	}

	c := NewTestStorageClient()
	c.expectSamples(samples)
	m := NewQueueManager(nil, config.DefaultQueueConfig, nil, c)

	// These should be received by the client.
	for _, s := range samples {
		m.Append(sampleToMetricFamily(s))
	}
	m.Start()
	defer m.Stop()

	c.waitForExpectedSamples(t)
}

// TestBlockingStorageClient is a queue_manager StorageClient which will block
// on any calls to Store(), until the `block` channel is closed, at which point
// the `numCalls` property will contain a count of how many times Store() was
// called.
type TestBlockingStorageClient struct {
	numCalls uint64
	block    chan bool
}

func NewTestBlockedStorageClient() *TestBlockingStorageClient {
	return &TestBlockingStorageClient{
		block:    make(chan bool),
		numCalls: 0,
	}
}

func (c *TestBlockingStorageClient) Store(_ *monitoring.CreateTimeSeriesRequest) error {
	atomic.AddUint64(&c.numCalls, 1)
	<-c.block
	return nil
}

func (c *TestBlockingStorageClient) NumCalls() uint64 {
	return atomic.LoadUint64(&c.numCalls)
}

func (c *TestBlockingStorageClient) unlock() {
	close(c.block)
}

func (c *TestBlockingStorageClient) Name() string {
	return "testblockingstorageclient"
}

func (t *QueueManager) queueLen() int {
	t.shardsMtx.Lock()
	defer t.shardsMtx.Unlock()
	queueLength := 0
	for _, shard := range t.shards.queues {
		queueLength += len(shard)
	}
	return queueLength
}

func TestSpawnNotMoreThanMaxConcurrentSendsGoroutines(t *testing.T) {
	// Our goal is to fully empty the queue:
	// `MaxSamplesPerSend*Shards` samples should be consumed by the
	// per-shard goroutines, and then another `MaxSamplesPerSend`
	// should be left on the queue.
	n := config.DefaultQueueConfig.MaxSamplesPerSend * 2

	samples := make([]sample, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("test_metric_%d", i)
		samples = append(samples, sample{Name: name, Value: float64(i)})
	}

	c := NewTestBlockedStorageClient()
	cfg := config.DefaultQueueConfig
	cfg.MaxShards = 1
	cfg.Capacity = n
	m := NewQueueManager(nil, cfg, nil, c)

	m.Start()

	defer func() {
		c.unlock()
		m.Stop()
	}()

	for _, s := range samples {
		m.Append(sampleToMetricFamily(s))
	}

	// Wait until the runShard() loops drain the queue.  If things went right, it
	// should then immediately block in sendSamples(), but, in case of error,
	// it would spawn too many goroutines, and thus we'd see more calls to
	// client.Store()
	//
	// The timed wait is maybe non-ideal, but, in order to verify that we're
	// not spawning too many concurrent goroutines, we have to wait on the
	// Run() loop to consume a specific number of elements from the
	// queue... and it doesn't signal that in any obvious way, except by
	// draining the queue.  We cap the waiting at 1 second -- that should give
	// plenty of time, and keeps the failure fairly quick if we're not draining
	// the queue properly.
	for i := 0; i < 100 && m.queueLen() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	if m.queueLen() != config.DefaultQueueConfig.MaxSamplesPerSend {
		t.Fatalf("Failed to drain QueueManager queue, %d elements left",
			m.queueLen(),
		)
	}

	numCalls := c.NumCalls()
	if numCalls != uint64(1) {
		t.Errorf("Saw %d concurrent sends, expected 1", numCalls)
	}
}

func sampleToMetricFamily(s sample) *dto.MetricFamily {
	return &dto.MetricFamily{
		Name: proto.String(s.Name),
		Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{
			&dto.Metric{
				Label: []*dto.LabelPair{
					{
						Name:  stringPtr("_kubernetes_project_id_or_name"),
						Value: stringPtr("1234567890"),
					},
				},
				Gauge: &dto.Gauge{
					Value: proto.Float64(s.Value),
				},
			},
		},
	}
}