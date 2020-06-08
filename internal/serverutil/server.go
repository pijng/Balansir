package serverutil

import (
	"expvar"
	"log"
	"net"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

//Server ...
type Server struct {
	mux               sync.RWMutex
	URL               *url.URL
	Weight            float64
	Index             int
	ActiveConnections *expvar.Float
	Alive             bool
	Proxy             *httputil.ReverseProxy
	ServerHash        string
}

//GetAlive ...
func (server *Server) GetAlive() bool {
	server.mux.RLock()
	defer server.mux.RUnlock()
	status := server.Alive
	return status
}

//SetAlive ...
func (server *Server) SetAlive(status bool) {
	server.mux.Lock()
	defer server.mux.Unlock()
	server.Alive = status
}

//CheckAlive ...
func (server *Server) CheckAlive(configTimeout int) {
	timeout := time.Second * time.Duration(configTimeout)
	connection, err := net.DialTimeout("tcp", server.URL.Host, timeout)
	if err != nil {
		server.SetAlive(false)
		log.Println("Server is down:", err)
		return
	}
	connection.Close()
	if !server.GetAlive() {
		log.Println("Server is up:", server.URL.Host)
	}
	server.SetAlive(true)
}
