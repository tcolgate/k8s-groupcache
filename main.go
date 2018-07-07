// Copyright (c) 2016 Tristan Colgate-McFarlane
//
// This file is part of k8s-groupcache.
//
// k8s-groupcache is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// k8s-groupcache is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with k8s-groupcache.  If not, see <http://www.gnu.org/licenses/>.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"go.opencensus.io/examples/exporter"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/trace"
	"go.opencensus.io/zpages"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/golang/groupcache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	scheme      = flag.String("peer.scheme", "http", "scheme for co`")
	port        = flag.String("peer.port", "8080", "port to listen on")
	path        = flag.String("peer.path", "/", "scheme")
	peerName    = flag.String("peer.self", "", "name of self, if not set defaults to hostname")
	serviceName = flag.String("peer.service", "", "the kube service for the group")
	serviceNS   = flag.String("peer.serviceNS", "default", "the kube service for the group")
	cacheBane   = flag.String("cache.name", "group", "the name of the group to start")
	cacheSize   = flag.Int64("cache.byts", 64<<20, "size of the cache, in bytes")
)

func main() {
	buckets := []float64{0.001, 0.01, 0.1, 1.0, 10.0, 100.0}
	flag.Parse()
	var err error

	self := *peerName
	if *peerName == "" {
		if self, err = os.Hostname(); err != nil {
			log.Fatalf("could not determine hostname, %v", err)
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		log.Fatalf("failed loading kube config, %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed building kube client, %v", err)
	}

	sinf := informers.NewSharedInformerFactoryWithOptions(clientset, 1*time.Minute,
		informers.WithNamespace(""),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf(".metadata.name=%s", *serviceName)
			return
		}),
	)

	peers := groupcache.NewHTTPPool(fmt.Sprintf("%s://%s:%s/%s", *scheme, self, *port, *path))

	addEndpoint := func(updated interface{}) {
		peers.Set()
	}

	deleteEndpoint := func(updated interface{}) {
		peers.Set()
	}

	updateEndpoint := func(old, new interface{}) {
		peers.Set()
	}

	epsInf := sinf.Core().V1().Endpoints().Informer()
	epsInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    addEndpoint,
		UpdateFunc: updateEndpoint,
		DeleteFunc: deleteEndpoint,
	})

	stopCh := make(chan struct{})
	epsInf.Run(stopCh)

	// Wait for the caches to be synced before starting workers
	log.Print("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, epsInf.HasSynced); !ok {
		log.Fatalf("failed to wait for caches to sync")
	}

	loadTime := prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   "groupcache",
		Subsystem:   "load",
		Name:        "duration_seconds",
		Help:        "time taken by upstream queries",
		Buckets:     buckets,
		ConstLabels: prometheus.Labels{"cache": *cacheBane},
	})

	var group = groupcache.NewGroup(*cacheBane, *cacheSize, groupcache.GetterFunc(
		func(ctx groupcache.Context, key string, dest groupcache.Sink) error {
			if gctx, ok := ctx.(context.Context); ok {
				_, span := trace.StartSpan(gctx, "cache lookup")
				defer span.End()
			}
			timer := prometheus.NewTimer(loadTime)
			defer timer.ObserveDuration()

			fileName := key
			dest.SetString(fileName)
			return nil
		}))

	prometheus.Register(&cacheCollector{group, loadTime})
	http.Handle("/metrics", promhttp.Handler())
	zpages.Handle(http.DefaultServeMux, "/debug")

	// Register stats and trace exporters to export the collected data.
	exporter := &exporter.PrintExporter{}
	view.RegisterExporter(exporter)
	trace.RegisterExporter(exporter)

	http.HandleFunc("/lookup/", func(w http.ResponseWriter, r *http.Request) {
		var bs []byte
		if err := group.Get(r.Context(), "mykey", groupcache.AllocatingByteSliceSink(&bs)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		io.Copy(w, bytes.NewBuffer(bs))
	})

	if err = http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatalf("server stopped, %v", err)
	}
}

type cacheCollector struct {
	*groupcache.Group
	loadTime prometheus.Histogram
}

func (c *cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("dummy", "dummy", nil, nil)
}

func (c *cacheCollector) Collect(ch chan<- prometheus.Metric) {
	ls := []string{"cache"}
	lvs := []string{c.Name()}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_gets", "get actions to the group", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.Gets.Get()),
		lvs...,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_cache_hits", "get hits on the group", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.CacheHits.Get()),
		lvs...,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_loads", "loads ", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.Loads.Get()),
		lvs...,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_dedupe_loads", "", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.LoadsDeduped.Get()),
		lvs...,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_local_load_errorss", "", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.LocalLoadErrs.Get()),
		lvs...,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_local_loads", "", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.LocalLoads.Get()),
		lvs...,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_peer_load_errors", "", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.PeerErrors.Get()),
		lvs...,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_peer_loads", "", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.PeerLoads.Get()),
		lvs...,
	)
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("groupcache_requests", "", ls, nil),
		prometheus.CounterValue,
		float64(c.Stats.ServerRequests.Get()),
		lvs...,
	)
	ch <- c.loadTime
}
