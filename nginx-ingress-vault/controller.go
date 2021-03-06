/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
    "reflect"
    "os"
    "time"
    "k8s.io/client-go/1.4/pkg/apis/extensions/v1beta1"

    "github.com/pearsontechnology/bitesize-controllers/nginx-ingress-vault/nginx"
    "github.com/pearsontechnology/bitesize-controllers/nginx-ingress-vault/monitor"
    "github.com/pearsontechnology/bitesize-controllers/nginx-ingress-vault/version"
    vlt "github.com/pearsontechnology/bitesize-controllers/nginx-ingress-vault/vault"
    k8s "github.com/pearsontechnology/bitesize-controllers/nginx-ingress-vault/kubernetes"

    "github.com/quipo/statsd"

    log "github.com/Sirupsen/logrus"

    "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
    "net/http"
)

func main() {

    log.SetFormatter(&log.JSONFormatter{})

    debug := os.Getenv("DEBUG")
    if debug == "true" {
        log.SetLevel(log.DebugLevel)
    }

    // Prometheus
    prometheus.MustRegister(&monitor.Status)
    http.Handle("/metrics", promhttp.Handler())
    log.Infof("Starting /metrics on port :8080")
    go func() {
        log.Fatal(http.ListenAndServe(":8080", nil))
    }()

    log.Infof("Ingress Controller version: %v", version.Version)

    v := os.Getenv("RELOAD_FREQUENCY")
    reloadFrequency, err := time.ParseDuration(v)
    if err != nil || v  == "" {
        reloadFrequency, _ = time.ParseDuration("5s")
    }

    onKubernetes := true
    if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
        log.Errorf("WARN: NOT running on Kubernetes, ingress functionality will be DISABLED")
        onKubernetes = false
    }

    stats := statsd.NewStatsdClient("localhost:8125", "nginx.config.")

    known := &v1beta1.IngressList{}

    vault, _ := vlt.NewVaultReader()
    if vault.Enabled {
        go vault.RenewToken()
    }

    // Controller loop
    for {

        if !vault.Enabled {
            vault, err = vlt.NewVaultReader()
            if err != nil {
                time.Sleep(reloadFrequency)
                continue
            }
            if vault.Enabled {
                go vault.RenewToken()
            }
        }

        if !vault.Ready() {
            vault, err = vlt.NewVaultReader()

            // Reset existing ingress list to allow pull of ssl from vault
            known = &v1beta1.IngressList{}
            time.Sleep(reloadFrequency)
            continue
        }

        time.Sleep(reloadFrequency)

        ingresses, err := k8s.GetIngresses(onKubernetes)

        if err != nil {
            log.Errorf("Error retrieving ingresses: %v", err)
            continue
        }

        if reflect.DeepEqual(ingresses.Items, known.Items) {
            continue
        }

        // Generating new config starts here
        var virtualHosts = []*nginx.VirtualHost{}

        // Reset prometheus counters
        monitor.Reset()

        for _, ingress := range ingresses.Items {
            vhost,_ := nginx.NewVirtualHost(ingress, vault)
            monitor.IncVHosts()
            vhost.CollectPaths()

            if err = vhost.Validate(); err != nil {
                log.Errorf("Ingress %s failed validation: %s", vhost.Name, err.Error() )
                monitor.IncFailedVHosts()
                continue
            }

            if err = vhost.CreateVaultCerts(); err != nil {
                log.Errorf("%s\n", err.Error() )
                vhost.HTTPSEnabled = false
            }
            if len(vhost.Paths) > 0 {
                virtualHosts = append(virtualHosts, vhost)
            }
        }

        if len(virtualHosts) == 0 && onKubernetes == true {
            continue
        }

        nginx.WriteConfig(virtualHosts)
        // cops-165 - Generate custom error page per vhost
        nginx.WriteCustomErrorPages(virtualHosts)

        err = nginx.Verify()

        stats.Incr("reload", 1)

        if err != nil {
            log.Errorf("ERR: nginx config failed validation: %v", err)
            log.Infof("Sent config error notification to statsd.")
            stats.Incr("error", 1)
        } else {
            nginx.Start()
            log.Infof("nginx config updated.")
            known = ingresses
        }

    }
}
