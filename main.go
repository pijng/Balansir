package main

import (
	"balansir/internal/cacheutil"
	"balansir/internal/configutil"
	"balansir/internal/helpers"
	"balansir/internal/metricsutil"
	"balansir/internal/poolutil"
	"balansir/internal/ratelimit"
	"balansir/internal/rateutil"
	"balansir/internal/serverutil"
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"expvar"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

type tunnel struct {
	wg sync.WaitGroup
}

func roundRobin(w http.ResponseWriter, r *http.Request) {
	index := pool.NextPool()
	endpoint := pool.ServerList[index]
	if configuration.SessionPersistence {
		w = helpers.SetCookieToResponse(w, endpoint.ServerHash, &configuration)
	}
	helpers.ServeDistributor(endpoint, configuration.Timeout, w, r, configuration.GzipResponse)
}

func weightedRoundRobin(w http.ResponseWriter, r *http.Request) {
	poolChoice := pool.GetPoolChoice()
	endpoint, err := poolutil.WeightedChoice(poolChoice)
	if err != nil {
		processingRequests.Done()
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if configuration.SessionPersistence {
		w = helpers.SetCookieToResponse(w, endpoint.ServerHash, &configuration)
	}
	helpers.ServeDistributor(endpoint, configuration.Timeout, w, r, configuration.GzipResponse)
}

func leastConnections(w http.ResponseWriter, r *http.Request) {
	endpoint := pool.GetLeastConnectedServer()
	if configuration.SessionPersistence {
		w = helpers.SetCookieToResponse(w, endpoint.ServerHash, &configuration)
	}
	endpoint.ActiveConnections.Add(1)
	helpers.ServeDistributor(endpoint, configuration.Timeout, w, r, configuration.GzipResponse)
	endpoint.ActiveConnections.Add(-1)
	processingRequests.Done()
}

func weightedLeastConnections(w http.ResponseWriter, r *http.Request) {
	endpoint := pool.GetWeightedLeastConnectedServer()
	if configuration.SessionPersistence {
		w = helpers.SetCookieToResponse(w, endpoint.ServerHash, &configuration)
	}
	endpoint.ActiveConnections.Add(1)
	helpers.ServeDistributor(endpoint, configuration.Timeout, w, r, configuration.GzipResponse)
	endpoint.ActiveConnections.Add(-1)
}

func newServeMux() *http.ServeMux {
	sm := http.NewServeMux()
	sm.HandleFunc("/", loadBalance)
	sm.HandleFunc("/balansir/metrics", metricsutil.Metrics)

	statsChannel := make(chan metricsutil.Stats, 1)
	go startMetricsPolling(statsChannel)
	mp := &metricsutil.MetricsPasser{MetricsChan: statsChannel}
	sm.HandleFunc("/balansir/metrics/stats", mp.MetrictStats)

	sm.Handle("/content/", http.StripPrefix("/content/", http.FileServer(http.Dir("content"))))
	return sm
}

func loadBalance(w http.ResponseWriter, r *http.Request) {
	requestFlow.wg.Wait()

	processingRequests.Add(1)
	defer processingRequests.Done()

	if configuration.Cache {
		response, err := cacheCluster.Get(r.URL.String())
		if err == nil {
			cacheutil.ServeFromCache(w, r, response)
			return
		}
	}

	if configuration.RateLimit {
		ip := helpers.ReturnIPFromHost(r.RemoteAddr)
		limiter := visitors.GetVisitor(ip, &configuration)
		if !limiter.Allow() {
			http.Error(w, http.StatusText(429), http.StatusTooManyRequests)
			return
		}
	}

	availableServers := poolutil.ExcludeUnavailableServers(pool.ServerList)
	if len(availableServers) == 0 {
		// log.Println("all servers are down")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	rateCounter.RateIncrement()
	rtStart := time.Now()
	defer rateCounter.ResponseCount(rtStart)

	if configuration.TransparentProxyMode {
		r = helpers.AddRemoteAddrToRequest(r)
	}

	if configuration.SessionPersistence {
		cookieHash, _ := r.Cookie("_balansir_server_hash")
		if cookieHash != nil {
			endpoint, err := pool.GetServerByHash(cookieHash.Value)
			if err != nil {
				log.Println(err)
				return
			}
			endpoint.Proxy.ServeHTTP(w, r)
			return
		}
	}

	switch configuration.Algorithm {
	case "round-robin":
		roundRobin(w, r)

	case "weighted-round-robin":
		weightedRoundRobin(w, r)

	case "least-connections":
		leastConnections(w, r)

	case "weighted-least-connections":
		weightedLeastConnections(w, r)
	}
}

func serversCheck() {
	timer := time.NewTicker(time.Duration(configuration.Delay) * time.Second)
	for {
		select {
		case <-timer.C:
			serverPoolWg.Wait()
			for _, server := range pool.ServerList {
				server.CheckAlive(&configuration)
			}
			configuration.Mux.Lock()
			timer = time.NewTicker(time.Duration(configuration.Delay) * time.Second)
			configuration.Mux.Unlock()
		}
	}
}

func startMetricsPolling(stats chan<- metricsutil.Stats) {
	stats <- metricsutil.Stats{}
	for {
		availableEndpoints := poolutil.ExcludeUnavailableServers(pool.ServerList)
		stats <- metricsutil.GetBalansirStats(rateCounter, &configuration, availableEndpoints)
	}
}

func proxyCacheResponse(r *http.Response) error {
	//Check if URL must be cached
	if ok, TTL := helpers.Contains(r.Request.URL.Path, configuration.CacheRules); ok {

		//Here we're checking if response' url is not cached.
		_, err := cacheCluster.Get(r.Request.URL.Path)
		if err != nil {

			var headers []cacheutil.Header

			for key, val := range r.Header {
				header := cacheutil.Header{
					Key:   key,
					Value: val,
				}
				headers = append(headers, header)
			}

			body, _ := ioutil.ReadAll(r.Body)

			response := cacheutil.Response{
				Headers: headers,
				Body:    body,
			}
			resp, err := json.Marshal(response)
			if err != nil {
				return err
			}

			//Reassign response body
			bodyBuf := bytes.NewBuffer(body)
			r.Body = ioutil.NopCloser(bodyBuf)

			//Set complete response to cache
			//`Set` returns an error if response couldn't be written to shard, due to
			//potential exceeding of max capacity.
			//Consider adding some logger here (why?)
			err = cacheCluster.Set(r.Request.URL.Path, resp, TTL)
			if err != nil {
				log.Println(err)
			}
		}
	}
	return nil
}

func fillConfiguration(file []byte, config *configutil.Configuration) error {
	requestFlow.wg.Add(1)

	processingRequests.Wait()
	defer requestFlow.wg.Done()

	config.Mux.Lock()
	defer config.Mux.Unlock()

	serverPoolWg.Add(1)
	if err := json.Unmarshal(file, &config); err != nil {
		return err
	}
	serverPoolWg.Done()

	if helpers.ServerPoolsEquals(&serverPoolHash, serverPoolHash, configuration.ServerList) {
		var serverHash string
		serverPoolWg.Add(len(configuration.ServerList))

		pool.ClearPool()

		for index, server := range configuration.ServerList {
			switch configuration.Algorithm {
			case "weighted-round-robin", "weighted-least-connections":
				if server.Weight < 0 {
					log.Fatalf(`Negative weight (%v) is specified for (%s) endpoint in config["server_list"]. Please set it's the weight to 0 if you want to mark it as dead one.`, server.Weight, server.URL)
				} else if server.Weight > 1 {
					log.Fatalf(`Weight can't be greater than 1. You specified (%v) weight for (%s) endpoint in config["server_list"].`, server.Weight, server.URL)
				}
			}

			serverURL, err := url.Parse(configuration.Protocol + "://" + strings.TrimSpace(server.URL))
			if err != nil {
				log.Fatal(err)
			}

			proxy := httputil.NewSingleHostReverseProxy(serverURL)
			proxy.ErrorHandler = helpers.ProxyErrorHandler
			proxy.Transport = &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   time.Duration(configuration.WriteTimeout) * time.Second,
					KeepAlive: time.Duration(configuration.ReadTimeout) * time.Second,
				}).DialContext,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
			}

			if configuration.Cache {
				proxy.ModifyResponse = proxyCacheResponse
			}

			connections := expvar.NewFloat(helpers.RandomStringBytes(5))

			if configuration.SessionPersistence {
				md := md5.Sum([]byte(serverURL.String()))
				serverHash = hex.EncodeToString(md[:16])
			}

			pool.AddServer(&serverutil.Server{
				URL:               serverURL,
				Weight:            server.Weight,
				ActiveConnections: connections,
				Index:             index,
				Alive:             true,
				Proxy:             proxy,
				ServerHash:        serverHash,
			})
			serverPoolWg.Done()
		}

		switch configuration.Algorithm {
		case "weighted-round-robin", "weighted-least-connections":
			nonZeroServers := pool.ExcludeZeroWeightServers()
			if len(nonZeroServers) <= 0 {
				log.Fatalf(`0 weight is specified for all your endpoints in config["server_list"]. Please consider adding at least one endpoint with non-zero weight.`)
			}
		}
	}

	return nil
}

func configWatch() {
	file, err := ioutil.ReadFile("config.json")
	if err != nil {
		log.Fatal(err)
	}
	md := md5.Sum(file)
	fileHash := hex.EncodeToString(md[:16])
	var fileHashNext string
	for {
		file, _ = ioutil.ReadFile("config.json")
		md = md5.Sum(file)
		fileHashNext = hex.EncodeToString(md[:16])
		if fileHash != fileHashNext {
			fileHash = fileHashNext
			err := fillConfiguration(file, &configuration)
			if err != nil {
				log.Fatalf(`Error reading configuration: %s`, err)
			}
			log.Println("Configuration file changes applied to Balansir")
		}
		time.Sleep(time.Second)
	}
}

func listenAndServeTLSWithAutocert() {

	certManager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(configuration.WhiteHosts...),
		Cache:      autocert.DirCache(configuration.CertDir),
	}

	server := &http.Server{
		Addr: ":" + strconv.Itoa(configuration.TLSPort),
		TLSConfig: &tls.Config{
			GetCertificate: certManager.GetCertificate,
		},
		ReadTimeout:  time.Duration(configuration.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(configuration.WriteTimeout) * time.Second,
	}

	port := configuration.Port

	go func() {
		http.HandleFunc("/", loadBalance)
		http.HandleFunc("/balansir/metrics", metricsutil.Metrics)

		statsChannel := make(chan metricsutil.Stats, 1)
		go startMetricsPolling(statsChannel)
		mp := &metricsutil.MetricsPasser{MetricsChan: statsChannel}
		http.HandleFunc("/balansir/metrics/stats", mp.MetrictStats)

		err := http.ListenAndServe(
			":"+strconv.Itoa(port),
			certManager.HTTPHandler(nil),
		)
		if err != nil {
			log.Fatalf(`Error starting listener: %s`, err)
		}
	}()

	err := server.ListenAndServeTLS("", "")
	if err != nil {
		log.Fatalf(`Error starting TLS listener: %s`, err)
	}
}

func listenAndServeTLSWithSelfSignedCerts() {
	go func() {
		server := http.Server{
			Addr:         ":" + strconv.Itoa(configuration.Port),
			Handler:      http.HandlerFunc(helpers.RedirectTLS),
			ReadTimeout:  time.Duration(configuration.ReadTimeout) * time.Second,
			WriteTimeout: time.Duration(configuration.WriteTimeout) * time.Second,
		}
		log.Fatal(server.ListenAndServe())
	}()

	if err := http.ListenAndServeTLS(":"+strconv.Itoa(configuration.TLSPort), configuration.SSLCertificate, configuration.SSLKey, newServeMux()); err != nil {
		log.Fatalf(`Error starting TLS listener: %s`, err)
	}
}

var configuration configutil.Configuration
var pool poolutil.ServerPool
var serverPoolWg sync.WaitGroup
var requestFlow tunnel
var processingRequests sync.WaitGroup
var serverPoolHash string
var visitors *ratelimit.Limiter
var cacheCluster *cacheutil.CacheCluster
var rateCounter *rateutil.Rate

func main() {
	file, err := ioutil.ReadFile("config.json")
	if err != nil {
		log.Fatal(err)
	}

	if err := fillConfiguration(file, &configuration); err != nil {
		log.Fatalf(`Error reading configuration: %s`, err)
	}

	go serversCheck()
	go configWatch()

	visitors = ratelimit.NewLimiter()

	if configuration.RateLimit {
		go visitors.CleanOldVisitors()
	}

	if configuration.Cache {
		args := cacheutil.CacheClusterArgs{
			ShardsAmount:     configuration.CacheShardsAmount,
			MaxSize:          configuration.CacheShardMaxSizeMb,
			ExceedFallback:   configuration.CacheShardExceedFallback,
			CacheAlgorithm:   configuration.CacheAlgorithm,
			BackgroundUpdate: configuration.CacheBackgroundUpdate,
			TransportTimeout: configuration.WriteTimeout,
			DialerTimeout:    configuration.ReadTimeout,
			CacheRules:       configuration.CacheRules,
			Port:             configuration.Port,
		}

		cacheCluster = cacheutil.New(args)
		debug.SetGCPercent(cacheutil.GCPercentRatio(configuration.CacheShardsAmount, configuration.CacheShardMaxSizeMb))
		log.Print("Cache enabled")
	}

	rateCounter = rateutil.NewRateCounter()

	if configuration.Protocol == "https" {

		if configuration.Autocert {
			listenAndServeTLSWithAutocert()
		} else {
			listenAndServeTLSWithSelfSignedCerts()
		}
	} else {
		server := http.Server{
			Addr:         ":" + strconv.Itoa(configuration.Port),
			Handler:      newServeMux(),
			ReadTimeout:  time.Duration(configuration.ReadTimeout) * time.Second,
			WriteTimeout: time.Duration(configuration.WriteTimeout) * time.Second,
		}
		log.Fatal(server.ListenAndServe())
	}

}
