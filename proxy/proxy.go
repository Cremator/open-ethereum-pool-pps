package proxy

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"

	"github.com/CryptoManiac/open-ethereum-pool/policy"
	"github.com/CryptoManiac/open-ethereum-pool/rpc"
	"github.com/CryptoManiac/open-ethereum-pool/storage"
	"github.com/CryptoManiac/open-ethereum-pool/util"
)

type WorkDiff struct {
	Difficulty int64
	PassDel bool
	IsDel   bool
}

type ProxyServer struct {
	config             *Config
	blockTemplate      atomic.Value
	upstream           int32
	upstreams          []*rpc.RPCClient
	backend            *storage.RedisClient
	diff               string
	policy             *policy.PolicyServer
	hashrateExpiration time.Duration
	failsCount         int64

	// Stratum
	sessionsMu sync.RWMutex
	sessions   map[*Session]struct{}
	timeout    time.Duration
	nonceSize  int

	// EthereumStratum jobs queue
	jobsMu sync.RWMutex
	Jobs *JobQueue
	workMu sync.RWMutex
	workDiff map[string]*WorkDiff
	minDiffFloat float64
	maxDiffFloat float64
}

type Session struct {
	ip  string
	enc *json.Encoder

	sync.Mutex
	conn  *net.TCPConn
	login string

	// EthereumStratum extranonce, current difficulty
	//   and mining.extranonce.subscribe status
	Extranonce string
	Difficulty int64
	exnSub     bool
}

func NewProxy(cfg *Config, backend *storage.RedisClient) *ProxyServer {
	if len(cfg.Name) == 0 {
		log.Fatal("You must set instance name")
	}

	policy := policy.Start(&cfg.Proxy.Policy, backend)

	proxy := &ProxyServer{config: cfg, backend: backend, policy: policy}
	proxy.diff = util.GetTargetHex(cfg.Proxy.Difficulty)
	proxy.workDiff = make(map[string]*WorkDiff)
	proxy.minDiffFloat = cfg.Proxy.Stratum.MinDiffFloat
	
	if proxy.minDiffFloat < 0.1 && cfg.Proxy.Stratum.Protocol == "EthereumStratum" {
		log.Fatal("For EthereumStratum protocol type, the minimum float difficulty must be set to at least 0.1")
	}
	
	proxy.maxDiffFloat = cfg.Proxy.Stratum.MaxDiffFloat
	log.Printf("Set minimum float difficulty to %v", proxy.minDiffFloat)
	log.Printf("Set maximum float difficulty to %v", proxy.maxDiffFloat)

	nonceSize := cfg.Proxy.Stratum.NonceSize
	if nonceSize < 2 {
		nonceSize = 2
	}
	proxy.nonceSize = nonceSize
	log.Printf("Set nonce size to %v", proxy.nonceSize)

	proxy.upstreams = make([]*rpc.RPCClient, len(cfg.Upstream))
	for i, v := range cfg.Upstream {
		proxy.upstreams[i] = rpc.NewRPCClient(v.Name, v.Url, v.Timeout)
		log.Printf("Upstream: %s => %s", v.Name, v.Url)
	}
	log.Printf("Default upstream: %s => %s", proxy.rpc().Name, proxy.rpc().Url)

	if cfg.Proxy.Stratum.Enabled {
		proxy.sessions = make(map[*Session]struct{})

		switch cfg.Proxy.Stratum.Protocol {
		case "Stratum-Proxy":
			go proxy.ListenSP()
		case "EthereumStratum":
			go proxy.ListenES()
		default:
			log.Fatal("Please choose either Stratum-Proxy or EthereumStratum protocol for your stratum endpoint.")
		}
	} else {
		log.Fatal("Stratum endpoint is not configured properly.")
	}

	proxy.fetchBlockTemplate()

	proxy.hashrateExpiration = util.MustParseDuration(cfg.Proxy.HashrateExpiration)

	refreshIntv := util.MustParseDuration(cfg.Proxy.BlockRefreshInterval)
	refreshTimer := time.NewTimer(refreshIntv)
	log.Printf("Set block refresh every %v", refreshIntv)

	cleanIntv := util.MustParseDuration(cfg.Proxy.CleanInterval)
	cleanTimer := time.NewTimer(cleanIntv)

	checkIntv := util.MustParseDuration(cfg.UpstreamCheckInterval)
	checkTimer := time.NewTimer(checkIntv)

	stateUpdateIntv := util.MustParseDuration(cfg.Proxy.StateUpdateInterval)
	stateUpdateTimer := time.NewTimer(stateUpdateIntv)

	go func() {
		for {
			select {
			case <-cleanTimer.C:
				proxy.workMu.Lock()
				for k, v := range proxy.workDiff {
					if v.IsDel && !v.PassDel {
						delete(proxy.workDiff, k)
					} else if v.IsDel {
						proxy.workDiff[k].PassDel = false
					}
				}
				proxy.workMu.Unlock()
				cleanTimer.Reset(refreshIntv)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-refreshTimer.C:
				proxy.fetchBlockTemplate()
				refreshTimer.Reset(refreshIntv)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-checkTimer.C:
				proxy.checkUpstreams()
				checkTimer.Reset(checkIntv)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-stateUpdateTimer.C:
				t := proxy.currentBlockTemplate()
				if t != nil {
					err := backend.WriteNodeState(cfg.Name, t.Height, t.Difficulty)
					if err != nil {
						log.Printf("Failed to write node state to backend: %v", err)
						proxy.markSick()
					} else {
						proxy.markOk()
					}
				}
				stateUpdateTimer.Reset(stateUpdateIntv)
			}
		}
	}()

	return proxy
}

func (s *ProxyServer) Start() {
	log.Printf("Starting work listener on %v", s.config.Proxy.Listen)
	r := mux.NewRouter()
	r.HandleFunc("/", func (w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "rpc: POST method required, received "+r.Method, 405)
			return
		}
		// TODO use work data directly, without fetching it again
		log.Printf("Received new job notification from %v", s.remoteAddr(r))
		s.fetchBlockTemplate()
	})
	srv := &http.Server{
		Addr:           s.config.Proxy.Listen,
		Handler:        r,
	}
	err := srv.ListenAndServe()
	if err != nil {
		log.Fatalf("Failed to start work listener: %v", err)
	}
}


func (s *ProxyServer) remoteAddr(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func (s *ProxyServer) rpc() *rpc.RPCClient {
	i := atomic.LoadInt32(&s.upstream)
	return s.upstreams[i]
}

func (s *ProxyServer) checkUpstreams() {
	candidate := int32(0)
	backup := false

	for i, v := range s.upstreams {
		if v.Check() && !backup {
			candidate = int32(i)
			backup = true
		}
	}

	if s.upstream != candidate {
		log.Printf("Switching to %v upstream", s.upstreams[candidate].Name)
		atomic.StoreInt32(&s.upstream, candidate)
	}
}

func (s *ProxyServer) currentBlockTemplate() *BlockTemplate {
	t := s.blockTemplate.Load()
	if t != nil {
		return t.(*BlockTemplate)
	} else {
		return nil
	}
}

func (s *ProxyServer) markSick() {
	atomic.AddInt64(&s.failsCount, 1)
}

func (s *ProxyServer) isSick() bool {
	x := atomic.LoadInt64(&s.failsCount)
	if s.config.Proxy.HealthCheck && x >= s.config.Proxy.MaxFails {
		return true
	}
	return false
}

func (s *ProxyServer) markOk() {
	atomic.StoreInt64(&s.failsCount, 0)
}
