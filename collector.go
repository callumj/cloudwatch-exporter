package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stoewer/go-strcase"
)

const batchSize = 500

var (
	// FIXME: technically it may not start with 0-9
	prometheusMetricNameRegexp = regexp.MustCompile("[^a-zA-Z0-9_:]")
)

type collector struct {
	logger log.Logger
	*reporter
	namespace  string
	metricName string
	descMap    map[string]*prometheus.Desc
	errDesc    *prometheus.Desc
}

func newCollector(logger log.Logger, reporter *reporter, namespace, metricName string) *collector {
	return &collector{
		logger:     logger,
		reporter:   reporter,
		namespace:  namespace,
		metricName: metricName,
		descMap:    make(map[string]*prometheus.Desc),
		errDesc:    prometheus.NewDesc("cloudwatch_error", "Error collecting metrics", nil, nil),
	}
}

// Describe implements Prometheus.Collector.
func (c collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("dummy", "dummy", nil, nil)
}

// Collect implements Prometheus.Collector.
func (c collector) Collect(ch chan<- prometheus.Metric) {
	metrics, err := c.reporter.ListMetrics(c.namespace, c.metricName)
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to list metrics", "err", err)
		ch <- prometheus.NewInvalidMetric(c.errDesc, err)
		return
	}
	level.Debug(c.logger).Log("msg", "list metrics returned", "metrics", metrics)

	// if we have less than batchSize results, we don't want to have zero entries
	length := len(metrics)
	if length > batchSize {
		length = batchSize
	}
	var (
		batch = make([]types.Metric, length, batchSize)
		i     = 0
	)
	for _, metric := range metrics {
		batch[i] = metric
		i++
		if i < batchSize {
			continue
		}
		i = 0
		c.collectBatch(ch, batch)
	}
	c.collectBatch(ch, batch[:i]) // The length of the array might be bigger than the number of entries when processing more than one batch
}

func (c collector) collectMetric(ch chan<- prometheus.Metric, m *types.Metric, value float64) {
	var (
		namespace = strcase.SnakeCase(prometheusMetricNameRegexp.ReplaceAllString(*m.Namespace, "_"))
		name      = strcase.SnakeCase(prometheusMetricNameRegexp.ReplaceAllString(*m.MetricName, "_"))

		lns = make([]string, len(m.Dimensions))
		lvs = make([]string, len(m.Dimensions))
	)
	// FIXME: do we need to sort the keys?
	for i, d := range m.Dimensions {
		lns[i] = *d.Name
		lvs[i] = *d.Value
	}

	key := strings.Join(lns, " ")
	level.Debug(c.logger).Log("msg", "Using key", "key", key)
	desc, ok := c.descMap[key]
	if !ok {
		level.Debug(c.logger).Log("msg", "Key not found, creating new decs")
		desc = prometheus.NewDesc(namespace+"_"+name, fmt.Sprintf("Cloudwatch Metric %s/%s", *m.Namespace, *m.MetricName), lns, nil)
		c.descMap[key] = desc
	}
	level.Debug(c.logger).Log("msg", "Sending metric", "desc", desc.String(), "lvs", fmt.Sprintf("%+v", lvs), "value", fmt.Sprintf("%f", value))
	ch <- prometheus.MustNewConstMetric(
		desc,
		prometheus.UntypedValue,
		value,
		lvs...,
	)
}

func sprintDims(ds []types.Dimension) (out string) {
	for _, d := range ds {
		out = fmt.Sprintf("%s%s=%s,", out, *d.Name, *d.Value)
	}
	return out
}

func (c collector) collectBatch(ch chan<- prometheus.Metric, metrics []types.Metric) {
	// FIXME: API call fails when MetricDataQueries is empty but we might
	// want to avoid that situation in the first place
	if len(metrics) == 0 {
		return
	}
	results, err := c.reporter.GetMetricsResults(metrics)
	if err != nil {
		level.Error(c.logger).Log("msg", "failed to get metric results", "err", err)
		ch <- prometheus.NewInvalidMetric(c.errDesc, err)
		return
	}
	nr := len(results)
	nm := len(metrics)
	if nr != nm {
		panic(fmt.Sprintf("not same length: %d != %d", nr, nm))
	}
	for _, result := range results {
		// idx is index in batch
		idx, err := strconv.Atoi((*result.Id)[1:]) // strip "n" prefix
		if err != nil {
			panic(err)
		}
		level.Debug(c.logger).Log("id", *result.Id)
		m := metrics[idx]
		level.Debug(c.logger).Log("msg", "creating metric", "index", idx, "dimensions", sprintDims(m.Dimensions))
		if len(result.Values) == 0 {
			level.Debug(c.logger).Log("msg", "no values found")
			continue
		}
		c.collectMetric(ch, &m, result.Values[0])
	}
}