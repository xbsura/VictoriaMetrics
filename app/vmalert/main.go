package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/datasource"
	"github.com/VictoriaMetrics/VictoriaMetrics/app/vmalert/notifier"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/envflag"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/procutil"
	"github.com/VictoriaMetrics/metrics"
)

var (
	rulePath = flagutil.NewArray("rule", `Path to the file with alert rules. 
Supports patterns. Flag can be specified multiple times. 
Examples:
 -rule /path/to/file. Path to a single file with alerting rules
 -rule dir/*.yaml -rule /*.yaml. Relative path to all .yaml files in "dir" folder, 
absolute path to all .yaml files in root.`)
	validateAlertAnnotations = flag.Bool("rule.validateAnnotations", true, "Indicates to validate annotation templates")
	httpListenAddr           = flag.String("httpListenAddr", ":8880", "Address to listen for http connections")
	datasourceURL            = flag.String("datasource.url", "", "Victoria Metrics or VMSelect url. Required parameter. e.g. http://127.0.0.1:8428")
	basicAuthUsername        = flag.String("datasource.basicAuth.username", "", "Optional basic auth username to use for -datasource.url")
	basicAuthPassword        = flag.String("datasource.basicAuth.password", "", "Optional basic auth password to use for -datasource.url")
	evaluationInterval       = flag.Duration("evaluationInterval", 1*time.Minute, "How often to evaluate the rules. Default 1m")
	notifierURL              = flag.String("notifier.url", "", "Prometheus alertmanager URL. Required parameter. e.g. http://127.0.0.1:9093")
	externalURL              = flag.String("external.url", "", "External URL is used as alert's source for sent alerts to the notifier")
)

// TODO: alerts state persistence
func main() {
	envflag.Parse()
	buildinfo.Init()
	logger.Init()
	checkFlags()
	eu, err := getExternalURL(*externalURL, *httpListenAddr, httpserver.IsTLS())
	if err != nil {
		logger.Fatalf("can not get external url:%s ", err)
	}
	notifier.InitTemplateFunc(eu)

	wg := sync.WaitGroup{}
	reloadCh := make(chan bool, 1)
	var ctx context.Context
	var cancel context.CancelFunc

	loadGroups := func(groups []Group) bool {
		ctx, cancel = context.WithCancel(context.Background())
		logger.Infof("reading alert rules configuration file from %s", strings.Join(*rulePath, ";"))

		w := &watchdog{
			storage: datasource.NewVMStorage(*datasourceURL, *basicAuthUsername, *basicAuthPassword, &http.Client{}),
			alertProvider: notifier.NewAlertManager(*notifierURL, func(group, name string) string {
				return fmt.Sprintf("%s/api/v1/%s/%s/status", eu, group, name)
			}, &http.Client{}),
		}
		for i := range groups {
			wg.Add(1)
			go func(group Group) {
				w.run(ctx, group, *evaluationInterval)
				wg.Done()
			}(groups[i])
		}
		go httpserver.Serve(*httpListenAddr, (&requestHandler{groups: groups, reloadCh: reloadCh}).handler)
		return true
	}

	reloadGroups := func() bool {
		// when reload failed, not fatal, just print log
		groups, err := Parse(*rulePath, *validateAlertAnnotations)
		if err != nil {
			logger.Errorf("Cannot parse configuration file: %s", err)
			return false
		}
		if err := httpserver.Stop(*httpListenAddr); err != nil {
			logger.Warnf("cannot stop the webservice: %s", err)
			return false
		}
		cancel()
		wg.Wait()
		return loadGroups(groups)
	}

	groups, err := Parse(*rulePath, *validateAlertAnnotations)
	if err != nil {
		logger.Fatalf("Cannot parse configuration file: %s", err)
	}

	loadGroups(groups)
	go func() {
		for range reloadCh {
			logger.Infof("start reload rule config")
			if reloadGroups() {
				logger.Infof("reload rule config ok")
			} else {
				logger.Warnf("reload rule config failed")
			}
		}
	}()

	var sig os.Signal
	for {
		sig = procutil.WaitForSigterm()
		logger.Infof("service received signal %s", sig)
		if sig == syscall.SIGHUP {
			reloadCh <- true
		} else {
			break
		}
	}

	if err := httpserver.Stop(*httpListenAddr); err != nil {
		logger.Fatalf("cannot stop the webservice: %s", err)
	}
	cancel()
	wg.Wait()
}

type watchdog struct {
	storage       *datasource.VMStorage
	alertProvider notifier.Notifier
}

var (
	iterationTotal    = metrics.NewCounter(`vmalert_iteration_total`)
	iterationDuration = metrics.NewSummary(`vmalert_iteration_duration_seconds`)

	execTotal    = metrics.NewCounter(`vmalert_execution_total`)
	execErrors   = metrics.NewCounter(`vmalert_execution_errors_total`)
	execDuration = metrics.NewSummary(`vmalert_execution_duration_seconds`)
)

func (w *watchdog) run(ctx context.Context, group Group, evaluationInterval time.Duration) {
	logger.Infof("watchdog for %s has been started", group.Name)
	t := time.NewTicker(evaluationInterval)
	defer t.Stop()
	for {

		select {
		case <-t.C:
			iterationTotal.Inc()
			iterationStart := time.Now()
			for _, rule := range group.Rules {
				execTotal.Inc()

				execStart := time.Now()
				err := rule.Exec(ctx, w.storage)
				execDuration.UpdateDuration(execStart)

				if err != nil {
					execErrors.Inc()
					logger.Errorf("failed to execute rule %q.%q: %s", group.Name, rule.Name, err)
					continue
				}

				if err := rule.Send(ctx, w.alertProvider); err != nil {
					logger.Errorf("failed to send alert for rule %q.%q: %s", group.Name, rule.Name, err)
				}
			}
			iterationDuration.UpdateDuration(iterationStart)
		case <-ctx.Done():
			logger.Infof("%s received stop signal", group.Name)
			return
		}
	}
}

func getExternalURL(externalURL, httpListenAddr string, isSecure bool) (*url.URL, error) {
	if externalURL != "" {
		return url.Parse(externalURL)
	}
	hname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	port := ""
	if ipport := strings.Split(httpListenAddr, ":"); len(ipport) > 1 {
		port = ":" + ipport[1]
	}
	schema := "http://"
	if isSecure {
		schema = "https://"
	}
	return url.Parse(fmt.Sprintf("%s%s%s", schema, hname, port))
}

func checkFlags() {
	if *notifierURL == "" {
		flag.PrintDefaults()
		logger.Fatalf("notifier.url is empty")
	}
	if *datasourceURL == "" {
		flag.PrintDefaults()
		logger.Fatalf("datasource.url is empty")
	}
}
